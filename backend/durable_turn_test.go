package backend

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/oliverkofoed/gokit/agentkit"
	"github.com/oliverkofoed/gokit/agentkit/llm"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/store"
)

// restartable is a backend that can be stopped and started again on the same
// database, which is the only way to test what a restart does to a turn.
type restartable struct {
	t      *testing.T
	dbPath string
	srv    *Server
	stop   context.CancelFunc
	done   chan struct{}
}

func newRestartable(t *testing.T, fake *llmtest.Fake) *restartable {
	r := &restartable{t: t, dbPath: filepath.Join(t.TempDir(), "john.db")}
	r.start(fake)
	t.Cleanup(func() { r.shutdown() })
	return r
}

func (r *restartable) start(fake *llmtest.Fake) {
	r.t.Helper()
	srv, err := New(Config{
		DBPath:   r.dbPath,
		Listen:   "127.0.0.1:0",
		Version:  "test",
		DevIdP:   &devidp.Identity{Subject: "dev-1", Email: "oliver@example.com", Name: "Oliver K"},
		Streamer: fake,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	r.srv, r.stop, r.done = srv, cancel, done
}

// shutdown is the backend going away the way a deploy takes it away: sessions
// are hung up, whatever was in memory is gone, the database stays.
func (r *restartable) shutdown() {
	if r.srv == nil {
		return
	}
	r.stop()
	<-r.done
	r.srv.Close()
	r.srv = nil
}

func (r *restartable) restart(fake *llmtest.Fake) {
	r.t.Helper()
	r.shutdown()
	r.start(fake)
}

// setup writes the org and returns a machine token, once per database.
func (r *restartable) setup() string {
	r.t.Helper()
	st := r.srv.Store()
	_, priv, _ := ed25519.GenerateKey(nil)
	if err := st.SaveOrg(&store.Org{
		Name: "Test", OIDCIssuer: "http://" + r.srv.Addr() + "/devidp",
		OIDCClientID: "john", AdminEmail: "oliver@example.com", SetupComplete: true,
		SigningKey: base64.StdEncoding.EncodeToString(priv),
	}); err != nil {
		r.t.Fatal(err)
	}
	lm, _ := json.Marshal(llm.Model{ID: "codegen-l", Name: "internal/codegen-l",
		API: llm.OpenAIChat, Provider: "selfhosted", BaseURL: "https://llm.internal/v1"})
	if err := st.CreateModel(&store.Model{ID: "m_codegen", Name: "internal/codegen-l",
		Provider: "selfhosted", Hosting: "internal", ModelID: "codegen-l",
		LLMModel: string(lm)}); err != nil {
		r.t.Fatal(err)
	}
	u, err := st.UpsertSSOUser("dev-1", "oliver@example.com", "Oliver K", "oliver@example.com")
	if err != nil {
		r.t.Fatal(err)
	}
	tok, err := st.CreateMachineToken(u.ID, "test", time.Hour)
	if err != nil {
		r.t.Fatal(err)
	}
	return tok
}

// client is one connected protocol client over a fresh session.open.
type turnClient struct {
	t    *testing.T
	conn *protocol.Conn
	msgs chan protocol.Message
}

func (r *restartable) open(token string) (*turnClient, *protocol.SessionReady) {
	r.t.Helper()
	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws://"+r.srv.Addr()+"/api/v1/session",
		&websocket.DialOptions{HTTPHeader: bearer(token)})
	if err != nil {
		r.t.Fatal(err)
	}
	tr := protocol.NewWSTransport(c)
	tr.WriteFrame(ctx, mustEncode(&protocol.SessionOpen{
		Workspace: "/tmp/proj", WorkspaceKey: "wk1", Protocol: protocol.Version}))
	ready := readReady(r.t, tr)
	tc := &turnClient{t: r.t, conn: protocol.NewConn(protocol.Options{}), msgs: make(chan protocol.Message, 64)}
	go func() {
		for m := range tc.conn.In() {
			tc.msgs <- m
		}
	}()
	tc.conn.Attach(tr, ready.Ack)
	return tc, ready
}

// await returns the first message satisfying pred, failing on a deadline.
func (c *turnClient) await(what string, pred func(protocol.Message) bool) protocol.Message {
	c.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case m := <-c.msgs:
			if pred(m) {
				return m
			}
		case <-deadline:
			c.t.Fatalf("no %s arrived", what)
		}
	}
}

// TestATurnSurvivesTheBackendRestarting is the whole of the durability claim.
// A turn is in flight with a tool call outstanding, the backend is stopped and
// started again — a deploy, a crash, a machine rebooting — and the developer's
// next connection picks the turn up rather than waiting for an answer that can
// no longer arrive.
func TestATurnSurvivesTheBackendRestarting(t *testing.T) {
	r := newRestartable(t, llmtest.New(
		llmtest.ToolCall("run", map[string]any{"cmd": "go test ./..."}),
		llmtest.Text("never reached — this backend does not come back"),
	))
	token := r.setup()

	c1, ready := r.open(token)
	session := ready.Session
	c1.conn.Send(context.Background(), &protocol.Prompt{Turn: "t7", Text: "run the tests"})
	c1.await("tool.call", func(m protocol.Message) bool {
		_, ok := m.(*protocol.ToolCall)
		return ok
	})

	// The session is running a turn, and the database says so.
	row, err := r.srv.Store().SessionByID(session)
	if err != nil {
		t.Fatal(err)
	}
	if row.TurnID != "t7" {
		t.Fatalf("running turn = %q, want t7", row.TurnID)
	}

	// --- the backend goes away mid-turn and comes back ---
	r.restart(llmtest.New(llmtest.Text("picked up where we left off")))

	// The backend that left must not have told anyone the turn ended: it had
	// not, and a client shown "interrupted" would stop waiting for it.
	for drained := false; !drained; {
		select {
		case m := <-c1.msgs:
			if end, ok := m.(*protocol.TurnEnd); ok {
				t.Fatalf("the stopping backend ended the turn (%q) instead of leaving it to be picked up", end.Reason)
			}
		default:
			drained = true
		}
	}

	c2, ready2 := r.open(token)
	if ready2.Session != session {
		t.Fatalf("reopened a different conversation: %s != %s", ready2.Session, session)
	}
	begin := c2.await("turn.begin", func(m protocol.Message) bool {
		_, ok := m.(*protocol.TurnBegin)
		return ok
	}).(*protocol.TurnBegin)
	if begin.Turn != "t7" {
		t.Fatalf("resumed turn = %q, want the one that was running", begin.Turn)
	}

	end := c2.await("turn.end", func(m protocol.Message) bool {
		_, ok := m.(*protocol.TurnEnd)
		return ok
	}).(*protocol.TurnEnd)
	if end.Reason != "completed" {
		t.Fatalf("resumed turn ended %q", end.Reason)
	}

	// And the session is idle again: nothing is left claiming to be running.
	row, err = r.srv.Store().SessionByID(session)
	if err != nil {
		t.Fatal(err)
	}
	if row.TurnID != "" {
		t.Fatalf("the finished turn is still recorded as running: %q", row.TurnID)
	}
}

// TestATurnCutOffMidCommandCarriesOn: the stop that leaves a turn behind is
// usually a stop in the middle of a command, and a conversation whose last
// word is a request for tools is one no model will carry on. The call is
// answered as interrupted, the turn carries on, and the model is told what
// happened. Before, it was announced as carried on and then ended as lost.
func TestATurnCutOffMidCommandCarriesOn(t *testing.T) {
	r := newRestartable(t, llmtest.New(llmtest.Text("unused")))
	token := r.setup()

	// Open once to get a conversation, then write over its state with the
	// shape a stop mid-command leaves: an assistant message that asked for a
	// tool, with the process killed before anything answered it.
	c1, ready := r.open(token)
	_ = c1
	st := agentkit.State{Messages: []llm.Message{
		llm.UserText("run the tests"),
		{Role: llm.RoleAssistant, Model: "codegen-l", Time: time.Now(),
			StopReason: llm.StopToolUse,
			Blocks: []llm.Block{{Type: llm.BlockToolCall, ID: "call_1", Name: "run",
				Args: json.RawMessage(`{"cmd":"go test ./..."}`)}}},
	}}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	// Written while the backend is down, as the stop left it: written while
	// it ran, the backend's own state for the open session went over it on
	// the way out.
	r.shutdown()
	db, err := store.Open(r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSessionState(ready.Session, string(raw), "m_codegen"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSessionTurn(ready.Session, "t9"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	fake := llmtest.New(llmtest.Text("the tests were interrupted; running them again"))
	r.start(fake)

	c2, _ := r.open(token)
	var cutOff string
	end := c2.await("turn.end", func(m protocol.Message) bool {
		if n, ok := m.(*protocol.Notice); ok && strings.Contains(n.Text, "cut off") {
			cutOff = n.Text
		}
		_, ok := m.(*protocol.TurnEnd)
		return ok
	}).(*protocol.TurnEnd)
	if end.Turn != "t9" || end.Reason != "completed" {
		t.Fatalf("turn.end = %+v, want t9 completed", end)
	}
	// And the developer is told which command, since the screen that showed
	// it running went with the process that stopped.
	if !strings.Contains(cutOff, "go test ./...") {
		t.Fatalf("no notice named the cut-off command (got %q)", cutOff)
	}

	// The model was asked to carry on with the call answered as interrupted.
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d, want 1", len(reqs))
	}
	var told string
	for _, m := range reqs[0].Messages {
		if m.Role == llm.RoleToolResult && m.ToolCallID == "call_1" {
			for _, b := range m.Blocks {
				told += b.Text
			}
		}
	}
	if !strings.Contains(told, "interrupted") {
		t.Fatalf("the model was not told the call was interrupted: %q", told)
	}
	row, err := r.srv.Store().SessionByID(ready.Session)
	if err != nil {
		t.Fatal(err)
	}
	if row.TurnID != "" {
		t.Fatalf("the finished turn is still recorded as running: %q", row.TurnID)
	}
}

// TestAnIdleSessionIsNotHandedATurnItNeverAskedFor: the resume is driven by a
// recorded turn, so a conversation that ended cleanly reopens silently.
func TestAnIdleSessionIsNotHandedATurnItNeverAskedFor(t *testing.T) {
	r := newRestartable(t, llmtest.New(llmtest.Text("done"), llmtest.Text("done again")))
	token := r.setup()

	c1, ready := r.open(token)
	c1.conn.Send(context.Background(), &protocol.Prompt{Turn: "t1", Text: "hello"})
	c1.await("turn.end", func(m protocol.Message) bool {
		_, ok := m.(*protocol.TurnEnd)
		return ok
	})

	r.restart(llmtest.New(llmtest.Text("unused")))
	c2, ready2 := r.open(token)
	if ready2.Session != ready.Session {
		t.Fatalf("reopened a different conversation")
	}
	select {
	case m := <-c2.msgs:
		if _, ok := m.(*protocol.TurnBegin); ok {
			t.Fatalf("a finished turn was resumed")
		}
	case <-time.After(time.Second):
	}
}

// A session reopened after a restart says how far its turns have got, so the
// client numbers on from there: it started again at t1, and the trace of one
// conversation held two t1s the backend could not tell apart.
func TestAReopenedSessionSaysHowManyTurnsItHasHad(t *testing.T) {
	r := newRestartable(t, llmtest.New(llmtest.Text("done")))
	token := r.setup()

	c1, ready := r.open(token)
	if ready.TurnsSoFar != 0 {
		t.Fatalf("a new session has had %d turns", ready.TurnsSoFar)
	}
	c1.conn.Send(context.Background(), &protocol.Prompt{Turn: "t3", Text: "hello"})
	c1.await("turn.end", func(m protocol.Message) bool {
		_, ok := m.(*protocol.TurnEnd)
		return ok
	})

	r.restart(llmtest.New(llmtest.Text("unused")))
	_, ready2 := r.open(token)
	if ready2.Session != ready.Session {
		t.Fatalf("reopened a different conversation")
	}
	if ready2.TurnsSoFar != 3 {
		t.Fatalf("the reopened session says %d turns so far, after t3", ready2.TurnsSoFar)
	}
}

// Only the calls with no result are answered, each right after the results
// its message did get, so the conversation stays in the order a model reads.
func TestOnlyUnansweredCallsAreAnswered(t *testing.T) {
	call := func(id string) llm.Block {
		return llm.Block{Type: llm.BlockToolCall, ID: id, Name: "run", Args: json.RawMessage(`{}`)}
	}
	in := []llm.Message{
		llm.UserText("go"),
		{Role: llm.RoleAssistant, Blocks: []llm.Block{call("a"), call("b")}},
		llm.ToolResultMessage("a", "run", false, llm.TextBlock("ok")),
		llm.UserText("and then"),
	}
	out, _ := answerInterruptedCalls(in)
	var got []string
	for _, m := range out {
		switch {
		case m.Role == llm.RoleToolResult && m.IsError:
			got = append(got, "interrupted:"+m.ToolCallID)
		case m.Role == llm.RoleToolResult:
			got = append(got, "result:"+m.ToolCallID)
		default:
			got = append(got, string(m.Role))
		}
	}
	if want := "user assistant result:a interrupted:b user"; strings.Join(got, " ") != want {
		t.Fatalf("got %s, want %s", strings.Join(got, " "), want)
	}
	if again, _ := answerInterruptedCalls(out); len(again) != len(out) {
		t.Fatal("answering an answered conversation added to it")
	}
}

// A session narrowed by a keyword stays narrowed when it is opened again
// after the backend restarts — a crash, or john started anew — and is not
// asked a second time. The keyword is still in its history, and the rule that
// had fired was forgotten with the process: it asked again on the turn the
// session carried on, with nobody yet at the keyboard to answer, and held
// that turn until it was aborted.
func TestANarrowedSessionIsNotAskedAgainAfterARestart(t *testing.T) {
	r := newRestartable(t, llmtest.New(llmtest.Text("ready")))
	token := r.setup()
	st := r.srv.Store()
	u, err := st.UpsertSSOUser("dev-1", "oliver@example.com", "Oliver K", "oliver@example.com")
	if err != nil {
		t.Fatal(err)
	}
	g, _ := st.CreatePolicyGroup("everyone", "", true, u.ID)
	spec, _ := json.Marshal(map[string]any{"kind": "model-routing", "mode": "restrict",
		"modelIds": []string{"m_codegen"}, "keywords": []string{"NIGHTJAR"}})
	if _, err := st.CreatePolicy(g.ID, "narrow", "", spec, u.ID); err != nil {
		t.Fatal(err)
	}

	c1, ready := r.open(token)
	c1.conn.Send(context.Background(), &protocol.Prompt{Turn: "t1", Text: "this is project NIGHTJAR"})
	narrow := c1.await("model.narrow", func(m protocol.Message) bool {
		_, ok := m.(*protocol.ModelNarrow)
		return ok
	}).(*protocol.ModelNarrow)
	c1.conn.Send(context.Background(), &protocol.ModelChoice{ID: narrow.ID, Model: "m_codegen"})
	c1.await("turn.end", func(m protocol.Message) bool {
		_, ok := m.(*protocol.TurnEnd)
		return ok
	})

	r.restart(llmtest.New(llmtest.Text("still here")))

	c2, ready2 := r.open(token)
	if ready2.Session != ready.Session {
		t.Fatalf("reopened a different conversation: %s != %s", ready2.Session, ready.Session)
	}
	c2.conn.Send(context.Background(), &protocol.Prompt{Turn: "t2", Text: "and again"})
	c2.await("turn.end", func(m protocol.Message) bool {
		switch v := m.(type) {
		case *protocol.ModelNarrow:
			t.Fatalf("the reopened session was asked to narrow again: %+v", v)
		case *protocol.TurnEnd:
			if v.Reason != "completed" {
				t.Fatalf("the turn ended %q", v.Reason)
			}
			return true
		}
		return false
	})
}

// A conversation begun before john's instructions changed runs on the new ones
// once it is reopened. The system prompt is saved with the conversation, and
// resuming it used to bring the old one back, deploy after deploy.
func TestAReopenedSessionRunsOnTodaysSystemPrompt(t *testing.T) {
	r := newRestartable(t, llmtest.New(llmtest.Text("done")))
	token := r.setup()

	c1, ready := r.open(token)
	c1.conn.Send(context.Background(), &protocol.Prompt{Turn: "t1", Text: "hello"})
	c1.await("turn.end", func(m protocol.Message) bool {
		_, ok := m.(*protocol.TurnEnd)
		return ok
	})

	fake := llmtest.New(llmtest.Text("again"))
	r.restart(fake)

	// The conversation as an older john saved it. Planted after the restart,
	// because a stopping backend checkpoints every session it holds.
	row, err := r.srv.Store().SessionByID(ready.Session)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal([]byte(row.State), &state); err != nil {
		t.Fatalf("stored state is not plain JSON here: %v", err)
	}
	state["system"] = "the instructions from before the deploy"
	old, _ := json.Marshal(state)
	if err := r.srv.Store().SaveSessionState(row.ID, string(old), row.ModelID); err != nil {
		t.Fatal(err)
	}

	c2, _ := r.open(token)
	c2.conn.Send(context.Background(), &protocol.Prompt{Turn: "t2", Text: "and again"})
	c2.await("turn.end", func(m protocol.Message) bool {
		_, ok := m.(*protocol.TurnEnd)
		return ok
	})
	reqs := fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("the reopened session asked no model")
	}
	if got := reqs[0].System; got != systemPrompt {
		t.Fatalf("the reopened session ran on %q", got)
	}
	if len(reqs[0].Messages) < 3 {
		t.Fatalf("the conversation did not carry over: %d messages", len(reqs[0].Messages))
	}
}

// openWith sends the given session.open and returns what the backend answered.
func openWith(t *testing.T, addr, token string, m *protocol.SessionOpen) *protocol.SessionReady {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), "ws://"+addr+"/api/v1/session",
		&websocket.DialOptions{HTTPHeader: bearer(token)})
	if err != nil {
		t.Fatal(err)
	}
	tr := protocol.NewWSTransport(c)
	t.Cleanup(func() { tr.Close() })
	m.Protocol = protocol.Version
	tr.WriteFrame(context.Background(), mustEncode(m))
	return readReady(t, tr)
}

// A reopened session says when it began and how full its context is, so the
// client can say it is carrying on and show the figure before the next turn.
// /new ends it, keeps it for /resume, and starts an empty one.
func TestAReopenedSessionSaysWhereItStandsAndNewStartsAnother(t *testing.T) {
	r := newRestartable(t, llmtest.New(llmtest.Text("done")))
	token := r.setup()

	c1, ready := r.open(token)
	c1.conn.Send(context.Background(), &protocol.Prompt{Turn: "t1", Text: "hello"})
	c1.await("turn.end", func(m protocol.Message) bool {
		_, ok := m.(*protocol.TurnEnd)
		return ok
	})

	r.restart(llmtest.New())
	_, again := r.open(token)
	if again.Session != ready.Session || again.TurnsSoFar != 1 {
		t.Fatalf("reopened %s after %d turns, want %s after 1", again.Session, again.TurnsSoFar, ready.Session)
	}
	if _, err := time.Parse(time.RFC3339, again.StartedAt); err != nil {
		t.Fatalf("started_at = %q", again.StartedAt)
	}
	if again.Context == 0 {
		t.Fatal("a reopened session with a turn behind it reports no context used")
	}

	fresh := openWith(t, r.srv.Addr(), token, &protocol.SessionOpen{
		Workspace: "/tmp/proj", WorkspaceKey: "wk1", New: true})
	if fresh.Session == ready.Session || fresh.TurnsSoFar != 0 || fresh.Context != 0 {
		t.Fatalf("/new opened %+v, want an empty conversation", fresh)
	}
	old, err := r.srv.Store().SessionByID(ready.Session)
	if err != nil || old.Status != "ended" {
		t.Fatalf("the conversation /new left is %+v, %v; want it ended and kept", old, err)
	}
	if fresh.Model.ID != again.Model.ID {
		t.Fatalf("/new started on %s, want the model the last session was on", fresh.Model.ID)
	}
}
