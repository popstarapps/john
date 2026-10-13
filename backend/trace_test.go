package backend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/coder/websocket"
	"github.com/oliverkofoed/gokit/agentkit/llm"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/trace"
)

// traceHarness is a controlHarness with a trace store, plus the two identities
// that can read it: the organization's, held by an administrator outside the
// backend, and the developer's, held by their client.
type traceHarness struct {
	*controlHarness
	dir    string
	org    *age.X25519Identity
	user   *age.X25519Identity
	userID string
	token  string
}

func newTraceHarness(t *testing.T, fake *llmtest.Fake) *traceHarness {
	t.Helper()
	h := newControlHarness(t, fake)
	h.addModel("codegen-l", "internal/codegen-l", "internal")

	orgKey, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	userKey, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "traces")

	org, err := h.store.Org()
	if err != nil {
		t.Fatal(err)
	}
	org.TraceKey, org.TracePath = orgKey.Recipient().String(), dir
	if err := h.store.SaveOrg(org); err != nil {
		t.Fatal(err)
	}

	userID, token := h.user()
	return &traceHarness{controlHarness: h, dir: dir, org: orgKey, user: userKey,
		userID: userID, token: token}
}

// open runs a full client handshake, answering the backend's request for a
// session key exactly as the real client does — with the key, never with a
// conversation. unlock false plays a client that has lost its identity.
func (h *traceHarness) open(t *testing.T, m *protocol.SessionOpen, unlock bool) (*protocol.WSTransport, *protocol.SessionReady) {
	t.Helper()
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/api/v1/session",
		&websocket.DialOptions{HTTPHeader: bearer(h.token)})
	if err != nil {
		t.Fatal(err)
	}
	tr := protocol.NewWSTransport(c)
	if m.TraceKey == "" {
		m.TraceKey = h.user.Recipient().String()
	}
	if m.Protocol == 0 {
		m.Protocol = protocol.Version
	}
	tr.WriteFrame(ctx, mustEncode(m))

	for {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		frame, err := tr.ReadFrame(rctx)
		cancel()
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		msg, err := protocol.DecodeRaw(frame)
		if err != nil {
			t.Fatalf("handshake decode: %v", err)
		}
		switch reply := msg.(type) {
		case *protocol.SessionReady:
			return tr, reply
		case *protocol.SessionKeyRequest:
			answer := &protocol.SessionKey{Session: reply.Session}
			if unlock {
				key, err := trace.UnwrapKey(reply.Wrapped, h.user)
				if err != nil {
					t.Fatalf("client could not unwrap its own session key: %v", err)
				}
				answer.Key = base64.StdEncoding.EncodeToString(key)
			}
			tr.WriteFrame(ctx, mustEncode(answer))
		default:
			t.Fatalf("unexpected handshake message %T", msg)
		}
	}
}

// turn runs one prompt to completion over an attached connection.
func turn(t *testing.T, tr *protocol.WSTransport, id, text string) *protocol.Conn {
	t.Helper()
	conn := protocol.NewConn(protocol.Options{})
	msgs := make(chan protocol.Message, 64)
	go func() {
		for m := range conn.In() {
			msgs <- m
		}
	}()
	conn.Attach(tr, 0)
	conn.Send(context.Background(), &protocol.Prompt{Turn: id, Text: text})
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-msgs:
			if _, ok := m.(*protocol.TurnEnd); ok {
				return conn
			}
		case <-deadline:
			t.Fatal("turn did not end")
		}
	}
}

func (h *traceHarness) traceFile(t *testing.T, session string) []byte {
	t.Helper()
	row, err := h.store.SessionByID(session)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.dir, row.StartedAt.UTC().Format("2006"), session+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading trace: %v", err)
	}
	return data
}

// TestSessionIsUnreadableToTheBackendAtRest is the claim the whole design
// makes, tested against everything the backend actually persists: the trace
// file and the sessions row. A prompt's text must be recoverable with a
// private key and with nothing else.
func TestSessionIsUnreadableToTheBackendAtRest(t *testing.T) {
	h := newTraceHarness(t, llmtest.New(llmtest.Text("SENTINEL-answer-from-the-model")))

	tr, ready := h.open(t, &protocol.SessionOpen{Workspace: "/tmp/proj", WorkspaceKey: "wk1"}, true)
	conn := turn(t, tr, "t1", "SENTINEL-prompt-rotate-the-billing-key")
	conn.Close()

	// Nothing the backend kept holds the conversation in the clear.
	data := h.traceFile(t, ready.Session)
	for _, sentinel := range []string{"SENTINEL-prompt-rotate-the-billing-key", "SENTINEL-answer-from-the-model"} {
		if bytes.Contains(data, []byte(sentinel)) {
			t.Fatalf("%q is in the trace file in the clear", sentinel)
		}
	}
	row, err := h.store.SessionByID(ready.Session)
	if err != nil {
		t.Fatal(err)
	}
	if !trace.IsSealedState(row.State) {
		t.Fatal("the agent state was stored unsealed")
	}
	if strings.Contains(row.State, "SENTINEL") {
		t.Fatal("the agent state holds the conversation in the clear")
	}
	dbBytes, err := os.ReadFile(h.srv.cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(dbBytes, []byte("SENTINEL-prompt-rotate-the-billing-key")) {
		t.Fatal("the prompt is in the database in the clear")
	}

	// Both key holders read the same file; nobody else does.
	for name, identity := range map[string]age.Identity{"organization": h.org, "developer": h.user} {
		records, err := trace.Records(bytes.NewReader(data), identity)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var found bool
		for _, rec := range records {
			if rec.Kind == "prompt" && strings.Contains(string(rec.Payload), "SENTINEL-prompt-rotate-the-billing-key") {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s could not read the prompt back out of %d records", name, len(records))
		}
	}
	stranger, _ := age.GenerateX25519Identity()
	if _, err := trace.Records(bytes.NewReader(data), stranger); err == nil {
		t.Fatal("an unrelated identity read the trace")
	}
}

// TestResumeNeedsTheClientsKey is the resume handshake end to end: a backend
// that has forgotten the key cannot read its own record of the conversation
// until the client hands the key back, and then the model sees the history.
func TestResumeNeedsTheClientsKey(t *testing.T) {
	fake := llmtest.New(llmtest.Text("first"), llmtest.Text("second"))
	h := newTraceHarness(t, fake)

	tr, ready := h.open(t, &protocol.SessionOpen{Workspace: "/tmp/proj", WorkspaceKey: "wk1"}, true)
	conn := turn(t, tr, "t1", "remember SENTINEL-across-restarts")
	conn.Close()
	tr.Close()

	// Everything the backend held in memory is gone; only the database and
	// the trace store remain, and neither can be read without a key.
	h.srv.hub.shutdownAll()

	tr2, ready2 := h.open(t, &protocol.SessionOpen{Workspace: "/tmp/proj", WorkspaceKey: "wk1"}, true)
	if ready2.Session != ready.Session {
		t.Fatalf("resumed into a different session: %s != %s", ready2.Session, ready.Session)
	}
	conn2 := turn(t, tr2, "t2", "and now?")
	conn2.Close()

	// The model saw the earlier exchange, so the state really was unsealed.
	requests := fake.Requests()
	if len(requests) < 2 {
		t.Fatalf("model saw %d requests", len(requests))
	}
	last := requests[len(requests)-1]
	var sawHistory bool
	for _, msg := range last.Messages {
		for _, b := range msg.Blocks {
			if b.Type == llm.BlockText && strings.Contains(b.Text, "SENTINEL-across-restarts") {
				sawHistory = true
			}
		}
	}
	if !sawHistory {
		t.Fatal("the resumed turn did not carry the earlier conversation to the model")
	}
}

// TestAClientThatCannotUnlockGetsAFreshSession: a developer whose identity is
// gone must not be dropped into a conversation that has silently lost its
// history — the backend starts a new one instead.
func TestAClientThatCannotUnlockGetsAFreshSession(t *testing.T) {
	h := newTraceHarness(t, llmtest.New(llmtest.Text("one"), llmtest.Text("two")))

	tr, ready := h.open(t, &protocol.SessionOpen{Workspace: "/tmp/proj", WorkspaceKey: "wk1"}, true)
	conn := turn(t, tr, "t1", "the first conversation")
	conn.Close()
	tr.Close()
	h.srv.hub.shutdownAll()

	tr2, ready2 := h.open(t, &protocol.SessionOpen{Workspace: "/tmp/proj", WorkspaceKey: "wk1"}, false)
	defer tr2.Close()
	if ready2.Session == ready.Session {
		t.Fatal("a client with no key was let into a session it cannot unlock")
	}
	row, err := h.store.SessionByID(ready2.Session)
	if err != nil {
		t.Fatal(err)
	}
	if row.Turns != 0 {
		t.Fatalf("the fresh session already has %d turns", row.Turns)
	}
}

// TestAdminGrantOpensASessionForANewKey is the recovery path an admin has
// when a developer loses their machine: unwrap with the organization key,
// re-wrap for the new one, and the backend appends it without ever holding a
// key that opens anything.
func TestAdminGrantOpensASessionForANewKey(t *testing.T) {
	h := newTraceHarness(t, llmtest.New(llmtest.Text("hello")))

	tr, ready := h.open(t, &protocol.SessionOpen{Workspace: "/tmp/proj", WorkspaceKey: "wk1"}, true)
	conn := turn(t, tr, "t1", "SENTINEL-granted-later")
	conn.Close()
	tr.Close()

	replacement, _ := age.GenerateX25519Identity()
	data := h.traceFile(t, ready.Session)
	if _, err := trace.Records(bytes.NewReader(data), replacement); err == nil {
		t.Fatal("the replacement key read the session before it was granted")
	}

	// What the console does in the browser, with the organization key.
	var header trace.Header
	firstLine, _, _ := strings.Cut(string(data), "\n")
	if err := json.Unmarshal([]byte(firstLine), &header); err != nil {
		t.Fatal(err)
	}
	orgCopy, ok := header.Key(trace.OrgRecipient)
	if !ok {
		t.Fatal("no organization copy in the header")
	}
	key, err := trace.UnwrapKey(orgCopy, h.org)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := trace.WrapKey(key, replacement.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"userId": h.userID, "wrapped": wrapped})
	req, err := http.NewRequest("POST", "http://"+h.srv.Addr()+"/api/v1/traces/"+ready.Session+"/grant",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grant returned %d", resp.StatusCode)
	}

	records, err := trace.Records(bytes.NewReader(h.traceFile(t, ready.Session)), replacement)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, rec := range records {
		if strings.Contains(string(rec.Payload), "SENTINEL-granted-later") {
			found = true
		}
	}
	if !found {
		t.Fatal("the granted key still cannot read the session")
	}
}

// TestNoTraceStoreMeansNoEncryption: a backend with no trace key configured
// still runs sessions, and stores their state in the clear. Nothing here
// pretends otherwise, and the console reports the absence. Requiring a trace
// key at setup would delete this path; see trace/readme.md.
func TestNoTraceStoreMeansNoEncryption(t *testing.T) {
	h := newControlHarness(t, llmtest.New(llmtest.Text("fine")))
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	_, token := h.user()

	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/api/v1/session",
		&websocket.DialOptions{HTTPHeader: bearer(token)})
	if err != nil {
		t.Fatal(err)
	}
	tr := protocol.NewWSTransport(c)
	tr.WriteFrame(ctx, mustEncode(&protocol.SessionOpen{
		Workspace: "/tmp/proj", WorkspaceKey: "wk1", Protocol: protocol.Version}))
	ready := readReady(t, tr)
	conn := turn(t, tr, "t1", "no trace store here")
	conn.Close()

	row, err := h.store.SessionByID(ready.Session)
	if err != nil {
		t.Fatal(err)
	}
	if trace.IsSealedState(row.State) {
		t.Fatal("state was sealed with no trace store to hold the key")
	}
	if row.State == "" {
		t.Fatal("state was not stored at all")
	}
}

// testRecipient is a real age public key, used wherever a test needs setup to
// accept one. A placeholder will not do: setup validates the recipient, so
// that a typo fails at the form rather than silently leaving a backend with
// no trace writer and every appearance of working.
const testRecipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"

// setupOrg posts a setup payload and returns the status and the org written.
func setupOrg(t *testing.T, payload map[string]any) (int, *store.Org) {
	t.Helper()
	srv, err := New(Config{
		DBPath: filepath.Join(t.TempDir(), "john.db"), Listen: "127.0.0.1:0",
		Version: "test", Setup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })

	token := srv.SetupURL[strings.Index(srv.SetupURL, "token=")+len("token="):]
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", "http://"+srv.Addr()+"/api/v1/setup", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Setup-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	org, _ := srv.Store().Org()
	return resp.StatusCode, org
}

func basePayload() map[string]any {
	return map[string]any{
		"orgName": "Acme", "issuer": "https://acme.okta.com",
		"clientId": "john-console", "adminEmail": "admin@acme.example",
	}
}

// TestSetupAlwaysEndsWithATraceKey is what makes the encryption unconditional.
// An operator who has never heard of age still gets a real key: one is minted
// at startup, printed to their terminal, and used when they leave the field
// empty. There is no path through setup that produces an organization
// recording its sessions in the clear.
func TestSetupAlwaysEndsWithATraceKey(t *testing.T) {
	status, org := setupOrg(t, basePayload())
	if status != http.StatusOK {
		t.Fatalf("setup with no trace key = %d, want it to use the generated one", status)
	}
	if org.TraceKey == "" {
		t.Fatal("setup completed with no trace key")
	}
	if _, err := age.ParseX25519Recipient(org.TraceKey); err != nil {
		t.Fatalf("the generated trace key is not a recipient: %v", err)
	}
	if org.TracePath == "" {
		t.Fatal("a trace key with nowhere to write is the same problem elsewhere")
	}

	// And the private half is nowhere the backend could reach it.
	db, err := os.ReadFile(filepath.Join(filepath.Dir(org.TracePath), "john.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(db, []byte("AGE-SECRET-KEY-")) {
		t.Fatal("a private key reached the database")
	}
}

// TestSetupKeepsAKeyTheOperatorBrought: pasting your own recipient wins, and
// the generated one is discarded.
func TestSetupKeepsAKeyTheOperatorBrought(t *testing.T) {
	p := basePayload()
	p["traceKey"] = testRecipient
	dir := filepath.Join(t.TempDir(), "traces")
	p["tracePath"] = dir

	status, org := setupOrg(t, p)
	if status != http.StatusOK {
		t.Fatalf("setup = %d", status)
	}
	if org.TraceKey != testRecipient {
		t.Fatalf("trace key = %q, want the one the operator pasted", org.TraceKey)
	}
	if org.TracePath != dir {
		t.Fatalf("trace path = %q", org.TracePath)
	}
}

// TestSetupRefusesATraceDirectoryItCannotWrite: a path the backend cannot
// write is the same silent failure as a bad key, found at the first session
// instead of at setup.
func TestSetupRefusesATraceDirectoryItCannotWrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := basePayload()
	p["tracePath"] = filepath.Join(file, "traces")
	if status, org := setupOrg(t, p); status != http.StatusBadRequest || (org != nil && org.SetupComplete) {
		t.Fatalf("setup with an unwritable trace directory = %d", status)
	}
}

// TestSetupRefusesAKeyThatIsNotOne: a typo must fail here, loudly. Accepting
// it would leave a backend with no trace writer and every appearance of
// working — the failure would only surface as an empty Traces page, long
// after the sessions it should have recorded.
func TestSetupRefusesAKeyThatIsNotOne(t *testing.T) {
	for _, bad := range []string{"age1qqq…", "not-a-key", "AGE-SECRET-KEY-1XYZ"} {
		p := basePayload()
		p["traceKey"] = bad
		status, org := setupOrg(t, p)
		if status != http.StatusBadRequest {
			t.Fatalf("setup with trace key %q = %d, want 400", bad, status)
		}
		if org != nil && org.SetupComplete {
			t.Fatalf("setup with trace key %q wrote an organization", bad)
		}
	}
}

// A request held by a keyword said nothing, so the trace does not record it
// as an assistant message that said nothing. It did, just before the
// model.narrow, as though the model had answered with silence.
func TestAHeldRequestLeavesNoEmptyMessageInTheTrace(t *testing.T) {
	h := newTraceHarness(t, llmtest.New(llmtest.Text("ready")))
	userID, _ := h.controlHarness.user()
	g, _ := h.store.CreatePolicyGroup("everyone", "", true, userID)
	spec, _ := json.Marshal(map[string]any{"kind": "model-routing", "mode": "restrict",
		"modelIds": []string{"m_codegen-l"}, "keywords": []string{"NIGHTJAR"}})
	if _, err := h.store.CreatePolicy(g.ID, "narrow", "", spec, userID); err != nil {
		t.Fatal(err)
	}

	tr, ready := h.open(t, &protocol.SessionOpen{Workspace: "/tmp/proj", WorkspaceKey: "wk1"}, true)
	conn := protocol.NewConn(protocol.Options{})
	msgs := make(chan protocol.Message, 64)
	go func() {
		for m := range conn.In() {
			msgs <- m
		}
	}()
	conn.Attach(tr, 0)
	conn.Send(context.Background(), &protocol.Prompt{Turn: "t1", Text: "project NIGHTJAR"})
	for done := false; !done; {
		select {
		case m := <-msgs:
			switch v := m.(type) {
			case *protocol.ModelNarrow:
				conn.Send(context.Background(), &protocol.ModelChoice{ID: v.ID, Model: "m_codegen-l"})
			case *protocol.TurnEnd:
				done = true
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the turn did not end")
		}
	}
	conn.Close()

	records, err := trace.Records(bytes.NewReader(h.traceFile(t, ready.Session)), h.org)
	if err != nil {
		t.Fatal(err)
	}
	said := 0
	for _, rec := range records {
		if rec.Kind != "assistant.message" {
			continue
		}
		var p struct{ Text string }
		json.Unmarshal(rec.Payload, &p)
		if p.Text == "" {
			t.Fatalf("an empty assistant message is in the trace: %s", rec.Payload)
		}
		said++
	}
	if said != 1 {
		t.Fatalf("%d assistant messages in the trace, want the one the model said", said)
	}
}
