package trace

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
)

func newTestWriter(t *testing.T) (*Writer, *age.X25519Identity, string) {
	t.Helper()
	org, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	w, err := NewWriter(dir, org.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	return w, org, dir
}

var testStart = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

// TestNothingPlaintextHitsDisk is the property the whole package exists for:
// sentinel strings sealed into records must not appear anywhere on disk, and
// the organization's identity must recover them exactly.
func TestNothingPlaintextHitsDisk(t *testing.T) {
	w, org, dir := newTestWriter(t)
	sess := w.Session("s_test", testStart)
	key, _, err := sess.Create("u_1", "", testStart)
	if err != nil {
		t.Fatal(err)
	}

	sentinels := []string{
		"SENTINEL-prompt-fix-the-billing-retry",
		"SENTINEL-secret-ACCT-4491",
		"SENTINEL-tool-go-test-output",
	}
	for i, s := range sentinels {
		payload, _ := json.Marshal(map[string]string{"text": s})
		if err := sess.Append(key, Record{
			At: testStart, Session: "s_test", Actor: "u_1",
			Kind:    []string{"prompt", "tool.result", "assistant.message"}[i],
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}

	var files []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 1 {
		t.Fatalf("files written = %v", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sentinels {
		if bytes.Contains(data, []byte(s)) {
			t.Fatalf("plaintext %q found on disk", s)
		}
	}
	// Not even the field names of the sealed record.
	for _, leak := range []string{"prompt", "actor", "payload"} {
		if bytes.Contains(data, []byte(`"`+leak+`"`)) {
			t.Fatalf("record structure %q leaked to disk", leak)
		}
	}

	got, err := Records(bytes.NewReader(data), org)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("records = %d", len(got))
	}
	for i, s := range sentinels {
		var payload struct{ Text string }
		json.Unmarshal(got[i].Payload, &payload)
		if payload.Text != s {
			t.Fatalf("record %d round trip = %q, want %q", i, payload.Text, s)
		}
	}
}

// TestBothRecipientsOpenTheSameFile is the whole point of the key hierarchy:
// the developer reads their own session, an admin reads every session, and
// the two of them are reading one file that was written once.
func TestBothRecipientsOpenTheSameFile(t *testing.T) {
	w, org, _ := newTestWriter(t)
	user, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	sess := w.Session("s_two", testStart)
	key, header, err := sess.Create("u_1", user.Recipient().String(), testStart)
	if err != nil {
		t.Fatal(err)
	}
	if len(header.Keys) != 2 {
		t.Fatalf("header carries %d keys, want org and user", len(header.Keys))
	}
	payload, _ := json.Marshal(map[string]string{"text": "hello"})
	if err := sess.Append(key, Record{Session: "s_two", Kind: "prompt", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sess.Path())
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []struct {
		name string
		id   age.Identity
	}{{"organization", org}, {"user", user}} {
		got, err := Records(bytes.NewReader(data), id.id)
		if err != nil {
			t.Fatalf("%s: %v", id.name, err)
		}
		if len(got) != 1 || got[0].Kind != "prompt" {
			t.Fatalf("%s read %v", id.name, got)
		}
	}

	// Nobody else. The header is readable, the records are not.
	if got, err := Records(bytes.NewReader(data), stranger); err == nil && len(got) > 0 {
		t.Fatalf("a stranger read %d records", len(got))
	}
}

// TestGrantOpensAnOlderSession is the recovery path: a developer whose key
// was not registered when the session ran — or who lost the machine that held
// it — is let back in by an admin appending one wrapped key, without the
// records being touched.
func TestGrantOpensAnOlderSession(t *testing.T) {
	w, org, _ := newTestWriter(t)
	sess := w.Session("s_grant", testStart)
	key, _, err := sess.Create("u_1", "", testStart) // no registered key
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"text": "before the grant"})
	if err := sess.Append(key, Record{Session: "s_grant", Kind: "prompt", Payload: payload}); err != nil {
		t.Fatal(err)
	}

	newKey, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(sess.Path())
	if got, err := Records(bytes.NewReader(data), newKey); err == nil && len(got) > 0 {
		t.Fatal("the new identity could read the session before being granted")
	}

	// What an admin does in the browser: unwrap with the org key, re-wrap.
	var header Header
	firstLine, _, _ := bufio.NewReader(bytes.NewReader(data)).ReadLine()
	if err := json.Unmarshal(firstLine, &header); err != nil {
		t.Fatal(err)
	}
	orgCopy, ok := header.Key(OrgRecipient)
	if !ok {
		t.Fatal("no organization copy in the header")
	}
	plain, err := UnwrapKey(orgCopy, org)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := WrapKey(plain, newKey.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Grant("u_1", wrapped); err != nil {
		t.Fatal(err)
	}

	data, _ = os.ReadFile(sess.Path())
	got, err := Records(bytes.NewReader(data), newKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("after the grant, records = %d, want 1", len(got))
	}
	var out struct{ Text string }
	json.Unmarshal(got[0].Payload, &out)
	if out.Text != "before the grant" {
		t.Fatalf("record text = %q", out.Text)
	}
}

// TestSequenceNumbersContinueAcrossWriters is the property that makes a gap in
// a trace file mean something. A backend restart, or a session reopened, used
// to build a fresh writer whose counter started at one again — so a file would
// hold 1,2,3,1,2 and a reader could not tell the repeat from a removed line.
func TestSequenceNumbersContinueAcrossWriters(t *testing.T) {
	org, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	var key []byte
	// Three writers over the same session, as three backend lifetimes.
	for lifetime := range 3 {
		w, err := NewWriter(dir, org.Recipient().String())
		if err != nil {
			t.Fatal(err)
		}
		sess := w.Session("s_test", testStart)
		if lifetime == 0 {
			if key, _, err = sess.Create("u_1", "", testStart); err != nil {
				t.Fatal(err)
			}
		}
		for range 2 {
			if err := sess.Append(key, Record{At: testStart, Session: "s_test", Kind: "prompt"}); err != nil {
				t.Fatal(err)
			}
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "2026", "s_test.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		var line struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		got = append(got, line.Seq)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7} // the header, then six records
	if len(got) != len(want) {
		t.Fatalf("lines = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sequence = %v, want %v", got, want)
		}
	}
}

// TestSessionFileFollowsTheSessionNotTheClock: a conversation that runs past
// New Year keeps writing to the file that holds its header and its key.
func TestSessionFileFollowsTheSessionNotTheClock(t *testing.T) {
	w, org, _ := newTestWriter(t)
	sess := w.Session("s_newyear", testStart)
	key, _, err := sess.Create("u_1", "", testStart)
	if err != nil {
		t.Fatal(err)
	}
	next := time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC)
	if err := sess.Append(key, Record{At: next, Session: "s_newyear", Kind: "prompt"}); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(filepath.Dir(sess.Path())); got != "2026" {
		t.Fatalf("session file year = %s, want the year it started", got)
	}
	data, _ := os.ReadFile(sess.Path())
	recs, err := Records(bytes.NewReader(data), org)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %v, err = %v", recs, err)
	}
}

func TestWriterRefusesToWorkWithoutAKey(t *testing.T) {
	if _, err := NewWriter(t.TempDir(), ""); err == nil {
		t.Fatal("a writer with no recipient must not exist")
	}
	if _, err := NewWriter(t.TempDir(), "not-an-age-key"); err == nil {
		t.Fatal("a writer with a bad recipient must not exist")
	}
	w, _, _ := newTestWriter(t)
	sess := w.Session("s_nokey", testStart)
	if err := sess.Append(nil, Record{Kind: "prompt"}); err == nil {
		t.Fatal("appending without a session key must fail, not write in the clear")
	}
}

func TestSealedStateRoundTrip(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"history":[{"role":"user","text":"SENTINEL-state"}]}`)
	sealed, err := SealState(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealedState(sealed) {
		t.Fatal("sealed state must be recognisable as sealed")
	}
	if bytes.Contains([]byte(sealed), []byte("SENTINEL-state")) {
		t.Fatal("plaintext survived into the sealed state")
	}
	if IsSealedState(string(plain)) {
		t.Fatal("a pre-encryption state row must not look sealed")
	}
	got, err := OpenState(key, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round trip = %s", got)
	}

	other, _ := NewKey()
	if _, err := OpenState(other, sealed); err == nil {
		t.Fatal("the wrong key opened the state")
	}
}

func TestSizeWalksTheStore(t *testing.T) {
	w, _, dir := newTestWriter(t)
	sess := w.Session("s_size", testStart)
	key, _, err := sess.Create("u_1", "", testStart)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(key, Record{Session: "s_size", Kind: "prompt"}); err != nil {
		t.Fatal(err)
	}
	if Size(dir) <= 0 {
		t.Fatal("a store with a session in it has a size")
	}
	if Size(filepath.Join(dir, "nope")) != 0 {
		t.Fatal("a missing store is zero, not an error")
	}
}
