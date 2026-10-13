package client

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

func TestMentionsAreReadTheWayTheInputBoxWritesThem(t *testing.T) {
	got := mentions("look at @shot.png and @docs/spec.pdf, then @shot.png again — not an email@example.com")
	want := []string{"shot.png", "docs/spec.pdf"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mentions = %v, want %v", got, want)
	}
	for name, want := range map[string]string{
		"a.png": "image/png", "b.JPG": "image/jpeg", "c.jpeg": "image/jpeg",
		"d.gif": "image/gif", "e.webp": "image/webp", "f.pdf": "application/pdf",
		"main.go": "", "notes.txt": "", "png": "",
	} {
		if got := attachableType(name); got != want {
			t.Errorf("attachableType(%q) = %q, want %q", name, got, want)
		}
	}
}

// attachApp builds enough of a client to read attachments: a fake sandbox
// over a workspace, a UI that is never run (Send only queues), and a policy
// snapshot already installed.
func attachApp(t *testing.T, workdir string, snap *policy.Snapshot) *app {
	t.Helper()
	a := &app{
		ui: terminalui.New(terminalui.Options{}), sb: sandbox.NewFake(workdir),
		workdir: workdir, dirty: map[string]bool{},
	}
	if snap != nil {
		ps := newPolicyStore("http://backend.invalid", "token")
		ps.snap = snap
		a.np = &networkProxy{store: ps}
	}
	return a
}

func openSnapshot() *policy.Snapshot {
	return &policy.Snapshot{ExpiresAt: time.Now().Add(time.Hour)}
}

// TestAnAttachmentInTheWorkspaceIsReadThroughTheSandbox is the rule that makes
// this something other than a way around an exclusion policy: the file is read
// the way the read tool reads one, so a path the sandbox does not serve is a
// path a prompt cannot carry. It is proven by attaching a file that exists
// ONLY in the sandbox — an os.ReadFile here would find nothing.
func TestAnAttachmentInTheWorkspaceIsReadThroughTheSandbox(t *testing.T) {
	dir := t.TempDir()
	a := attachApp(t, dir, openSnapshot())
	png := []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 3}
	if err := a.sb.Write(context.Background(), "shot.png", png); err != nil {
		t.Fatal(err)
	}

	atts := a.attach(context.Background(), "what is wrong with @shot.png")
	if len(atts) != 1 {
		t.Fatalf("attachments = %d, want 1", len(atts))
	}
	if atts[0].Name != "shot.png" || atts[0].MimeType != "image/png" {
		t.Fatalf("attachment = %+v", atts[0])
	}
	if atts[0].Data != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("attachment carried the wrong bytes")
	}

	// And a file the sandbox does not serve is not attached, even when the
	// host has one at that path.
	if err := os.WriteFile(filepath.Join(dir, "hidden.png"), png, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.sb.Delete(context.Background(), "hidden.png"); err != nil {
		t.Fatal(err)
	}
	if atts := a.attach(context.Background(), "and @hidden.png"); len(atts) != 0 {
		t.Fatalf("a path the sandbox does not serve was attached: %+v", atts)
	}
}

// TestAFileOutsideTheWorkspaceIsTheDevelopersToSend: a screenshot lives in
// ~/Desktop, and refusing the common case would be refusing the feature.
// Nothing in the loop can ask for one — the path has to be typed.
func TestAFileOutsideTheWorkspaceIsTheDevelopersToSend(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "screenshot.png")
	if err := os.WriteFile(outside, []byte("not really a png"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := attachApp(t, t.TempDir(), openSnapshot())

	atts := a.attach(context.Background(), "look at @"+outside)
	if len(atts) != 1 || atts[0].Name != "screenshot.png" {
		t.Fatalf("attachments = %+v", atts)
	}
}

func TestAPromptCarriesNothingThePolicyRefuses(t *testing.T) {
	dir := t.TempDir()
	png := []byte{0x89, 'P', 'N', 'G'}

	cases := []struct {
		name string
		snap *policy.Snapshot
	}{
		{"attachments switched off", &policy.Snapshot{
			ExpiresAt: time.Now().Add(time.Hour), Attach: policy.AttachmentsOff}},
		{"the session redacts what reaches the model", &policy.Snapshot{
			ExpiresAt: time.Now().Add(time.Hour), ReplacementsActive: true}},
		// Fail closed: without a snapshot we cannot say what is allowed, and
		// the proxy blocks every decision-needing request for the same reason.
		{"no snapshot at all", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := attachApp(t, dir, c.snap)
			if err := a.sb.Write(context.Background(), "shot.png", png); err != nil {
				t.Fatal(err)
			}
			if atts := a.attach(context.Background(), "see @shot.png"); len(atts) != 0 {
				t.Fatalf("attached anyway: %+v", atts)
			}
		})
	}

	// And an administrator who allows them anyway is obeyed.
	a := attachApp(t, dir, &policy.Snapshot{ExpiresAt: time.Now().Add(time.Hour),
		Attach: policy.AttachmentsOn, ReplacementsActive: true})
	if err := a.sb.Write(context.Background(), "shot.png", png); err != nil {
		t.Fatal(err)
	}
	if atts := a.attach(context.Background(), "see @shot.png"); len(atts) != 1 {
		t.Fatalf("attachments = %+v, want the one the administrator allowed", atts)
	}
}

// TestAPromptWillNotCarryMoreThanItSaidItWould: the limit is the whole of one
// prompt, not one file, and it is the negotiated number rather than this
// build's own.
func TestAPromptWillNotCarryMoreThanItSaidItWould(t *testing.T) {
	dir := t.TempDir()
	a := attachApp(t, dir, openSnapshot())
	a.limits.MaxAttachmentBytes = 64

	small := make([]byte, 40)
	big := make([]byte, 100)
	for _, f := range []struct {
		path string
		data []byte
	}{{"small.png", small}, {"another.png", small}, {"big.png", big}} {
		if err := a.sb.Write(context.Background(), f.path, f.data); err != nil {
			t.Fatal(err)
		}
	}

	atts := a.attach(context.Background(), "@small.png @another.png")
	if len(atts) != 1 || atts[0].Name != "small.png" {
		t.Fatalf("attachments = %+v, want only the first to fit", atts)
	}
	if atts := a.attach(context.Background(), "@big.png"); len(atts) != 0 {
		t.Fatalf("a file over the limit was attached: %+v", atts)
	}
}

// TestAMentionOfAnythingElseIsStillJustAMention: the feature is for the files
// a read tool has nothing useful to say about. Everything else is unchanged.
func TestAMentionOfAnythingElseIsStillJustAMention(t *testing.T) {
	dir := t.TempDir()
	a := attachApp(t, dir, openSnapshot())
	if err := a.sb.Write(context.Background(), "main.go", []byte("package main")); err != nil {
		t.Fatal(err)
	}
	if atts := a.attach(context.Background(), "read @main.go please"); atts != nil {
		t.Fatalf("a source file was attached: %+v", atts)
	}
}
