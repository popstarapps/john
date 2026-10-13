package backend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/oliverkofoed/gokit/agentkit/llm"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/store"
)

// controlHarness is a backend with a scripted model and a directly-driven
// protocol client, for exercising the in-loop controls without a terminal.
type controlHarness struct {
	t     *testing.T
	srv   *Server
	fake  *llmtest.Fake
	store *store.Store
	conn  *protocol.Conn
	msgs  chan protocol.Message
}

func newControlHarness(t *testing.T, fake *llmtest.Fake) *controlHarness {
	t.Helper()
	srv, err := New(Config{
		DBPath:   filepath.Join(t.TempDir(), "john.db"),
		Listen:   "127.0.0.1:0",
		Version:  "test",
		DevIdP:   &devidp.Identity{Subject: "dev-1", Email: "oliver@example.com", Name: "Oliver K"},
		Streamer: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })

	_, priv, _ := ed25519.GenerateKey(nil)
	if err := srv.Store().SaveOrg(&store.Org{
		Name: "Test", OIDCIssuer: "http://" + srv.Addr() + "/devidp",
		OIDCClientID: "john", AdminEmail: "oliver@example.com", SetupComplete: true,
		SigningKey: base64.StdEncoding.EncodeToString(priv),
	}); err != nil {
		t.Fatal(err)
	}
	return &controlHarness{t: t, srv: srv, fake: fake, store: srv.Store()}
}

func (h *controlHarness) addModel(id, name, hosting string) {
	h.t.Helper()
	lm, _ := json.Marshal(llm.Model{ID: id, Name: name, API: llm.OpenAIChat,
		Provider: "selfhosted", BaseURL: "https://llm.internal/v1", Cost: llm.Cost{Input: 1, Output: 2}})
	if err := h.store.CreateModel(&store.Model{
		ID: "m_" + id, Name: name, Provider: "selfhosted", Hosting: hosting,
		ModelID: id, LLMModel: string(lm),
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *controlHarness) user() (userID, token string) {
	h.t.Helper()
	u, err := h.store.UpsertSSOUser("dev-1", "oliver@example.com", "Oliver K", "oliver@example.com")
	if err != nil {
		h.t.Fatal(err)
	}
	tok, err := h.store.CreateMachineToken(u.ID, "test", time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	return u.ID, tok
}

// connect opens a session and returns once session.ready arrives.
func (h *controlHarness) connect(token string) *protocol.SessionReady {
	h.t.Helper()
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/api/v1/session",
		&websocket.DialOptions{HTTPHeader: bearer(token)})
	if err != nil {
		h.t.Fatal(err)
	}
	tr := protocol.NewWSTransport(c)
	tr.WriteFrame(ctx, mustEncode(&protocol.SessionOpen{
		Workspace: "/tmp/proj", WorkspaceKey: "wk1", Protocol: protocol.Version}))
	frame, err := tr.ReadFrame(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	msg, _ := protocol.DecodeRaw(frame)
	ready, ok := msg.(*protocol.SessionReady)
	if !ok {
		h.t.Fatalf("expected ready, got %T: %+v", msg, msg)
	}
	h.conn = protocol.NewConn(protocol.Options{})
	h.msgs = make(chan protocol.Message, 64)
	go func() {
		for m := range h.conn.In() {
			h.msgs <- m
		}
	}()
	h.conn.Attach(tr, ready.Ack)
	return ready
}

func (h *controlHarness) send(m protocol.Message) { h.conn.Send(context.Background(), m) }

// waitFor collects messages until one satisfies pred or the deadline passes.
// It answers tool.calls automatically with the given results.
func (h *controlHarness) drive(prompt string, toolResults map[string]*protocol.ToolResult) (endReason string, seen []protocol.Message) {
	return h.drivePrompt(&protocol.Prompt{Turn: "t1", Text: prompt}, toolResults)
}

// drivePrompt is drive for a prompt that carries more than text.
func (h *controlHarness) drivePrompt(p *protocol.Prompt, toolResults map[string]*protocol.ToolResult) (endReason string, seen []protocol.Message) {
	h.send(p)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-h.msgs:
			seen = append(seen, m)
			switch v := m.(type) {
			case *protocol.ToolCall:
				res := toolResults[v.Tool]
				if res == nil {
					res = &protocol.ToolResult{Status: "ok"}
				}
				res.ID = v.ID
				h.send(res)
			case *protocol.TurnEnd:
				return v.Reason, seen
			}
		case <-deadline:
			h.t.Fatalf("turn did not end; saw %d messages", len(seen))
		}
	}
}

func bearer(token string) map[string][]string {
	return map[string][]string{"Authorization": {"Bearer " + token}}
}

func mustEncode(m protocol.Message) []byte {
	data, _ := protocol.EncodeRaw(m)
	return data
}

// TestReplacementsReachTheWire proves a secret never reaches the model: the
// replacement runs on the backend, on everything on its way to a provider.
func TestReplacementsReachTheWire(t *testing.T) {
	fake := llmtest.New(llmtest.Text("I see ACCT-1 in the code."))
	h := newControlHarness(t, fake)
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	userID, token := h.user()

	// A replacement policy in a baseline group, so it applies to the user.
	g, _ := h.store.CreatePolicyGroup("redaction", "", true, userID)
	spec, _ := json.Marshal(map[string]any{"kind": "replacement",
		"pattern": `ACCT-\d+`, "replacement": "ACCT-#"})
	h.store.CreatePolicy(g.ID, "redact accounts", "", spec, userID)

	h.connect(token)
	reason, _ := h.drive("please look at ACCT-4491 and ACCT-4491 again and ACCT-7000", nil)
	if reason != "completed" {
		t.Fatalf("turn reason = %s", reason)
	}

	reqs := fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("model was never called")
	}
	for _, r := range reqs {
		for _, m := range r.Messages {
			for _, b := range m.Blocks {
				if strings.Contains(b.Text, "ACCT-4491") || strings.Contains(b.Text, "ACCT-7000") {
					t.Fatalf("a real account number reached the model: %q", b.Text)
				}
			}
		}
	}
	// And the alias is stable: the same value became the same alias both times.
	joined := ""
	for _, m := range reqs[0].Messages {
		for _, b := range m.Blocks {
			joined += b.Text
		}
	}
	if strings.Count(joined, "ACCT-1") < 2 {
		t.Fatalf("stable alias not applied: %q", joined)
	}
}

// TestAliasInEditIsBlocked proves the v1 answer to the replacement open
// question: an edit whose arguments carry an alias is refused as an error the
// model sees, never sent to the client.
func TestAliasInEditIsBlocked(t *testing.T) {
	fake := llmtest.New(
		// The prompt (carrying the real value) mints the alias on this first
		// call; the model then tries to edit a line containing the alias.
		llmtest.ToolCall("edit", map[string]any{"path": "a.go", "content": "// keep ACCT-1 here\n"}),
		llmtest.Text("I could not edit that line."),
	)
	h := newControlHarness(t, fake)
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	userID, token := h.user()
	g, _ := h.store.CreatePolicyGroup("redaction", "", true, userID)
	spec, _ := json.Marshal(map[string]any{"kind": "replacement",
		"pattern": `ACCT-\d+`, "replacement": "ACCT-#"})
	h.store.CreatePolicy(g.ID, "redact", "", spec, userID)

	h.connect(token)
	// Prompt carries the real value so the alias is minted this turn.
	_, seen := h.drive("summarize the file that has ACCT-5000 in it, then edit it", nil)

	// The client must never have been asked to run that edit.
	for _, m := range seen {
		if tc, ok := m.(*protocol.ToolCall); ok && tc.Tool == "edit" {
			t.Fatal("an edit touching redacted content reached the client")
		}
	}
	// And the model was told, as an error it can reason about.
	reqs := fake.Requests()
	blocked := false
	for _, r := range reqs {
		for _, m := range r.Messages {
			if m.IsError {
				for _, b := range m.Blocks {
					if strings.Contains(b.Text, "redacted content") {
						blocked = true
					}
				}
			}
		}
	}
	if !blocked {
		t.Fatal("the model should have seen the edit blocked as redacted")
	}
}

// TestKeywordNarrowing proves the order that is the control: a keyword in the
// outbound content holds the request, the developer is asked, and only then
// does anything reach a model.
func TestKeywordNarrowing(t *testing.T) {
	fake := llmtest.New(llmtest.Text("Working on the C++ bridge."))
	h := newControlHarness(t, fake)
	h.addModel("general", "external/general", "external")
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	userID, token := h.user()

	// Assign both models, then a restrict rule that narrows to the internal
	// one on the keyword "C++".
	g, _ := h.store.CreatePolicyGroup("payments", "", true, userID)
	assign, _ := json.Marshal(map[string]any{"kind": "model-routing", "mode": "assign",
		"modelIds": []string{"m_general", "m_codegen-l"}, "keywords": []string{}})
	h.store.CreatePolicy(g.ID, "assign", "", assign, userID)
	restrict, _ := json.Marshal(map[string]any{"kind": "model-routing", "mode": "restrict",
		"modelIds": []string{"m_codegen-l"}, "keywords": []string{"C++"}})
	h.store.CreatePolicy(g.ID, "restrict", "", restrict, userID)

	h.connect(token)

	h.send(&protocol.Prompt{Turn: "t1", Text: "help me with this C++ code"})
	var narrow *protocol.ModelNarrow
	deadline := time.After(10 * time.Second)
	// The first thing back must be the narrow, before any model text —
	// nothing reached a provider yet.
	for narrow == nil {
		select {
		case m := <-h.msgs:
			switch v := m.(type) {
			case *protocol.ModelNarrow:
				narrow = v
			case *protocol.MessageDelta, *protocol.MessageStart:
				t.Fatal("model output arrived before narrowing — the keyword leaked")
			}
		case <-deadline:
			t.Fatal("no model.narrow arrived")
		}
	}
	if len(fake.Requests()) != 0 {
		t.Fatal("a model was called before the developer chose — narrowing must come first")
	}
	if len(narrow.Models) != 1 || narrow.Models[0].ID != "m_codegen-l" {
		t.Fatalf("narrow offered %+v, want just the internal model", narrow.Models)
	}

	// Pick the internal model; the turn then continues on it.
	h.send(&protocol.ModelChoice{ID: narrow.ID, Model: "m_codegen-l"})
	for {
		select {
		case m := <-h.msgs:
			if end, ok := m.(*protocol.TurnEnd); ok {
				if end.Reason != "completed" {
					t.Fatalf("turn ended %s", end.Reason)
				}
				// It ran on the internal model.
				reqs := fake.Requests()
				if len(reqs) == 0 || reqs[len(reqs)-1].Model.ID != "codegen-l" {
					t.Fatalf("continued on the wrong model: %+v", reqs)
				}
				// The session is recorded as narrowed.
				sessions, _ := h.store.FindActiveSession(userID, "wk1")
				if sessions == nil || !sessions.Narrowed {
					t.Fatal("session not recorded as narrowed")
				}
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatal("turn did not complete after the model choice")
		}
	}
}

// TestPolicySnapshotSignedAndFetchable proves the proxy's supply line: the
// snapshot verifies against the pinned key and carries the user's policies.
func TestPolicySnapshotSignedAndFetchable(t *testing.T) {
	fake := llmtest.New()
	h := newControlHarness(t, fake)
	userID, token := h.user()
	g, _ := h.store.CreatePolicyGroup("net", "", true, userID)
	spec, _ := json.Marshal(map[string]any{"kind": "network", "methods": []string{"GET"},
		"scheme": "https", "host": "proxy.golang.org", "path": "/*",
		"headerMatches": []any{}, "queryMatches": []any{},
		"decision": "always-allow", "maxBodyBytes": nil})
	h.store.CreatePolicy(g.ID, "allow goproxy", "", spec, userID)

	signed, err := h.srv.buildSnapshot(&store.User{ID: userID})
	if err != nil {
		t.Fatal(err)
	}
	org, _ := h.store.Org()
	priv, _ := signingKey(org)
	snap, err := verifyWith(signed, priv)
	if err != nil {
		t.Fatalf("snapshot did not verify: %v", err)
	}
	if len(snap.Network) != 1 || snap.Network[0].Spec.Host != "proxy.golang.org" {
		t.Fatalf("snapshot network = %+v", snap.Network)
	}
	_ = token
}

// A keyword rule on its own narrows when its word is seen and changes nothing
// before. Written into an organization with no routing policy, it used to
// count as the first one — and every developer opening a session was told no
// model was assigned to them.
func TestAKeywordRuleAloneLeavesEveryModel(t *testing.T) {
	fake := llmtest.New(llmtest.Text("Hello."))
	h := newControlHarness(t, fake)
	h.addModel("general", "external/general", "external")
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	userID, token := h.user()

	g, _ := h.store.CreatePolicyGroup("everyone", "", true, userID)
	restrict, _ := json.Marshal(map[string]any{"kind": "model-routing", "mode": "restrict",
		"modelIds": []string{"m_codegen-l"}, "keywords": []string{"C++"}})
	h.store.CreatePolicy(g.ID, "restrict", "", restrict, userID)

	h.connect(token)
	h.send(&protocol.Prompt{Turn: "t1", Text: "hello"})
	for {
		select {
		case m := <-h.msgs:
			switch v := m.(type) {
			case *protocol.Error:
				t.Fatalf("the session failed: %s: %s", v.Code, v.Message)
			case *protocol.TurnEnd:
				if v.Reason != "completed" {
					t.Fatalf("turn ended %s", v.Reason)
				}
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the turn did not complete")
		}
	}
}

// A developer is told when an access request they filed is decided, in the
// session they have open. A rejection writes no policy, so nothing else
// reached them: their next attempt was blocked exactly as before.
func TestTheRequesterIsToldTheirRequestWasDecided(t *testing.T) {
	h := newControlHarness(t, llmtest.New())
	h.addModel("general", "external/general", "external")
	_, token := h.user()
	h.connect(token)

	do := func(path string, body, out any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", "http://"+h.srv.Addr()+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			msg, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST %s: %s: %s", path, resp.Status, msg)
		}
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
	}
	var filed struct{ ID, Ref string }
	do("/api/v1/access-requests", map[string]any{
		"title": "Access to example.org", "blocked": "GET https://example.org/",
		"spec": map[string]any{"kind": "network", "methods": []string{"GET"}, "scheme": "https",
			"host": "example.org", "port": nil, "path": "/*", "headerMatches": []any{},
			"queryMatches": []any{}, "decision": "always-allow", "maxBodyBytes": nil},
	}, &filed)
	do("/api/v1/access-requests/"+filed.ID+"/decision", map[string]any{"status": "rejected"}, nil)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-h.msgs:
			if d, ok := m.(*protocol.AccessDecision); ok {
				if d.Ref != filed.Ref || d.Status != "rejected" || d.Host != "example.org" {
					t.Fatalf("told %+v, want %s rejected for example.org", d, filed.Ref)
				}
				return
			}
		case <-deadline:
			t.Fatal("the requester's session was not told the request was decided")
		}
	}
}

// Commands the model asks for together run one after another, in the order it
// wrote them: the second is not sent to the client until the first has
// answered. Run at once, "apt-get update" and then "apt-get install" started
// in the same millisecond, and the install found nothing to install.
func TestCommandsInOneReplyRunInOrder(t *testing.T) {
	call := func(cmd string) llm.Block {
		args, _ := json.Marshal(map[string]any{"cmd": cmd})
		return llm.Block{Type: llm.BlockToolCall, Name: "run", Args: args}
	}
	fake := llmtest.New(
		llmtest.Blocks(llm.StopToolUse, call("apt-get update"), call("apt-get install -y nodejs")),
		llmtest.Text("Installed."),
	)
	h := newControlHarness(t, fake)
	h.addModel("general", "external/general", "external")
	_, token := h.user()
	h.connect(token)

	h.send(&protocol.Prompt{Turn: "t1", Text: "install node"})
	var order []string
	var pending atomic.Int32
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-h.msgs:
			switch v := m.(type) {
			case *protocol.ToolCall:
				var args struct{ Cmd string }
				json.Unmarshal(v.Args, &args)
				order = append(order, args.Cmd)
				if pending.Add(1) > 1 {
					t.Fatalf("%q was sent while another command was still running", args.Cmd)
				}
				// Answer a moment later, so a call sent alongside it would
				// have arrived first.
				go func(id string) {
					time.Sleep(200 * time.Millisecond)
					pending.Add(-1)
					h.send(&protocol.ToolResult{ID: id, Status: "ok"})
				}(v.ID)
			case *protocol.TurnEnd:
				if want := []string{"apt-get update", "apt-get install -y nodejs"}; strings.Join(order, "|") != strings.Join(want, "|") {
					t.Fatalf("ran %q, want %q", order, want)
				}
				return
			}
		case <-deadline:
			t.Fatalf("the turn did not end; ran %q", order)
		}
	}
}

// A policy written while a session is open applies in it from the next
// request: a keyword rule narrows, a replacement rewrites. They were read
// once, when the session opened, so a rule added mid-session was not
// enforced in it while the screen said "policies updated".
func TestAPolicyWrittenMidSessionAppliesInIt(t *testing.T) {
	fake := llmtest.New(llmtest.Text("first"), llmtest.Text("second"))
	h := newControlHarness(t, fake)
	h.addModel("general", "external/general", "external")
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	userID, token := h.user()
	h.connect(token)

	// A turn before any policy, so the session is open and running.
	if reason, _ := h.drive("hello", nil); reason != "completed" {
		t.Fatalf("first turn ended %s", reason)
	}

	g, _ := h.store.CreatePolicyGroup("everyone", "", true, userID)
	restrict, _ := json.Marshal(map[string]any{"kind": "model-routing", "mode": "restrict",
		"modelIds": []string{"m_codegen-l"}, "keywords": []string{"NIGHTJAR"}})
	h.store.CreatePolicy(g.ID, "narrow", "", restrict, userID)
	replace, _ := json.Marshal(map[string]any{"kind": "replacement",
		"pattern": `ACCT-\d+`, "replacement": "ACCT-#"})
	h.store.CreatePolicy(g.ID, "redact", "", replace, userID)

	// The session reloads what it enforces before it sends each refresh, so
	// both refreshes having arrived is the two policies having been read.
	// Prompting sooner raced the reload, and lost every time on Linux once
	// creating the empty group stopped sending a refresh of its own.
	for refreshes := 0; refreshes < 2; {
		select {
		case m := <-h.msgs:
			if _, ok := m.(*protocol.PolicyRefresh); ok {
				refreshes++
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the session was not told about the new policies")
		}
	}

	h.send(&protocol.Prompt{Turn: "t2", Text: "NIGHTJAR account ACCT-4417"})
	narrowed := false
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case m := <-h.msgs:
			switch v := m.(type) {
			case *protocol.ModelNarrow:
				narrowed = true
				h.send(&protocol.ModelChoice{ID: v.ID, Model: "m_codegen-l"})
			case *protocol.TurnEnd:
				done = true
			}
		case <-deadline:
			t.Fatal("the second turn did not end")
		}
	}
	if !narrowed {
		t.Fatal("a keyword rule written mid-session did not narrow it")
	}
	reqs := fake.Requests()
	last := reqs[len(reqs)-1]
	for _, m := range last.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "ACCT-4417") {
				t.Fatalf("a replacement written mid-session was not applied: %q reached the model", b.Text)
			}
		}
	}
}

// A policy refresh says what changed, and an approval reaches its requester
// before the refresh its policy sends. Every change used to arrive as the
// same bare nudge, so the client said "policies updated" when a model was
// added, and said it again under every approval it had just announced.
func TestAPolicyRefreshSaysWhatChanged(t *testing.T) {
	h := newControlHarness(t, llmtest.New())
	h.addModel("general", "external/general", "external")
	h.addModel("spare", "external/spare", "external")
	userID, token := h.user()
	h.connect(token)

	g, _ := h.store.CreatePolicyGroup("everyone", "", true, userID)
	spec, _ := json.Marshal(map[string]any{"kind": "network", "methods": []string{"GET"}, "scheme": "https",
		"host": "example.org", "port": nil, "path": "/*", "headerMatches": []any{},
		"queryMatches": []any{}, "decision": "always-allow", "maxBodyBytes": nil})
	r, err := h.store.CreateAccessRequest(userID, "", "Access to example.org", "", "", nil, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"status": "approved", "groupId": g.ID})
	req, _ := http.NewRequest("POST", "http://"+h.srv.Addr()+"/api/v1/access-requests/"+r.ID+"/decision", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approving: %s", resp.Status)
	}

	next := func() protocol.Message {
		t.Helper()
		select {
		case m := <-h.msgs:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("nothing arrived")
			return nil
		}
	}
	if d, ok := next().(*protocol.AccessDecision); !ok || d.Status != "approved" {
		t.Fatalf("first message after the approval = %+v, want the decision", d)
	}
	if p, ok := next().(*protocol.PolicyRefresh); !ok || strings.Join(p.What, ",") != "network" {
		t.Fatalf("second message after the approval = %+v, want a refresh for network", p)
	}

	if err := h.store.DeleteModel("m_spare"); err != nil {
		t.Fatal(err)
	}
	if p, ok := next().(*protocol.PolicyRefresh); !ok || strings.Join(p.What, ",") != "models" {
		t.Fatalf("after a model was deleted = %+v, want a refresh for models", p)
	}
}

// A client asks for its own developer's access requests as "me", which is
// all it can: it does not know their id. In standalone the developer is the
// administrator, and an administrator's list is otherwise everyone's.
func TestAnAdministratorCanListTheirOwnAccessRequests(t *testing.T) {
	h := newControlHarness(t, llmtest.New())
	userID, token := h.user()
	other, err := h.store.UpsertSSOUser("dev-2", "deb@example.com", "Deb", "deb@example.com")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(map[string]any{"kind": "network", "host": "example.org"})
	mine, _ := h.store.CreateAccessRequest(userID, "", "mine", "", "", nil, spec, nil)
	h.store.CreateAccessRequest(other.ID, "", "theirs", "", "", nil, spec, nil)

	req, _ := http.NewRequest("GET", "http://"+h.srv.Addr()+"/api/v1/access-requests?requesterId=me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []struct{ Ref string }
	json.NewDecoder(resp.Body).Decode(&got)
	if len(got) != 1 || got[0].Ref != mine.Ref {
		t.Fatalf("requesterId=me listed %+v, want only %s", got, mine.Ref)
	}
}

// A command the developer stops with Esc is told to the model as that. The
// bare context error reached it as "context canceled", and after Esc on
// `sleep 300` the model told the developer the command had timed out.
func TestTheModelIsToldTheDeveloperStoppedACommand(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"cmd": "sleep 300; echo finished"})
	fake := llmtest.New(
		llmtest.Blocks(llm.StopToolUse, llm.Block{Type: llm.BlockToolCall, Name: "run", Args: args}),
		llmtest.Text("It was stopped."),
	)
	h := newControlHarness(t, fake)
	h.addModel("general", "external/general", "external")
	_, token := h.user()
	h.connect(token)

	h.send(&protocol.Prompt{Turn: "t1", Text: "run the sleep"})
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case m := <-h.msgs:
			switch m.(type) {
			case *protocol.ToolCall:
				// Not answered: the developer presses Esc while it runs.
				h.send(&protocol.Interrupt{Turn: "t1"})
			case *protocol.TurnEnd:
				done = true
			}
		case <-deadline:
			t.Fatal("the interrupted turn did not end")
		}
	}

	if reason, _ := h.drivePrompt(&protocol.Prompt{Turn: "t2", Text: "what happened?"}, nil); reason != "completed" {
		t.Fatalf("the next turn ended %s", reason)
	}
	reqs := fake.Requests()
	var told string
	for _, m := range reqs[len(reqs)-1].Messages {
		if m.Role != llm.RoleToolResult {
			continue
		}
		for _, b := range m.Blocks {
			told += b.Text
		}
	}
	if !strings.Contains(told, "the developer pressed Esc") || strings.Contains(told, "context canceled") {
		t.Fatalf("the model was told %q about the stopped command", told)
	}
}
