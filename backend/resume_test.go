package backend

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/protocol"
)

// TestFullStackParkAndResume is the end-to-end proof of disconnection recovery:
// a turn is in flight with a tool call outstanding, the transport dies, and a
// fresh transport resumes the SAME session — the tool result the client sent
// while disconnected is delivered, and the turn completes. This is the flow the
// protocol package unit-tests at the frame level, here exercised through the
// real hub, agent loop, and websocket.
func TestFullStackParkAndResume(t *testing.T) {
	h := newControlHarness(t, llmtest.New(
		llmtest.ToolCall("run", map[string]any{"cmd": "go test ./..."}),
		llmtest.Text("resumed-ok"),
	))
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	_, token := h.user()

	ctx := context.Background()
	dial := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/api/v1/session",
			&websocket.DialOptions{HTTPHeader: bearer(token)})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// --- open the session on the first transport ---
	c1 := dial()
	tr1 := protocol.NewWSTransport(c1)
	tr1.WriteFrame(ctx, mustEncode(&protocol.SessionOpen{
		Workspace: "/tmp/proj", WorkspaceKey: "wk1", Protocol: protocol.Version}))
	ready := readReady(t, tr1)
	session := ready.Session

	conn := protocol.NewConn(protocol.Options{})
	msgs := make(chan protocol.Message, 64)
	go func() {
		for m := range conn.In() {
			msgs <- m
		}
	}()
	conn.Attach(tr1, ready.Ack)

	// --- start a turn; wait for the tool call, do not answer it ---
	conn.Send(ctx, &protocol.Prompt{Turn: "t1", Text: "run the tests"})
	var call *protocol.ToolCall
	for call == nil {
		select {
		case m := <-msgs:
			if tc, ok := m.(*protocol.ToolCall); ok {
				call = tc
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no tool.call arrived before the drop")
		}
	}

	// --- the transport dies mid-turn; the backend parks the turn ---
	c1.Close(websocket.StatusAbnormalClosure, "wifi dropped")
	conn.Detach()

	// While disconnected, the client answers the tool call. The Conn buffers
	// it for replay.
	conn.Send(ctx, &protocol.ToolResult{ID: call.ID, Status: "ok", Exit: 0, Stdout: "PASS\n"})

	// --- resume on a fresh transport ---
	c2 := dial()
	tr2 := protocol.NewWSTransport(c2)
	tr2.WriteFrame(ctx, mustEncode(&protocol.SessionResume{
		Session: session, WorkspaceKey: "wk1", Ack: conn.RecvSeq()}))
	ready2 := readReady(t, tr2)
	if ready2.Session != session {
		t.Fatalf("resume returned a different session: %s != %s", ready2.Session, session)
	}
	conn.Attach(tr2, ready2.Ack) // flushes the buffered tool.result

	// --- the turn continues to completion on the resumed connection ---
	var text string
	for {
		select {
		case m := <-msgs:
			switch v := m.(type) {
			case *protocol.MessageDelta:
				text += v.Text
			case *protocol.TurnEnd:
				if v.Reason != "completed" {
					t.Fatalf("turn ended %s after resume", v.Reason)
				}
				if text != "resumed-ok" {
					t.Fatalf("assistant text after resume = %q", text)
				}
				// The model saw the tool result — the loop really continued.
				reqs := h.fake.Requests()
				if len(reqs) != 2 {
					t.Fatalf("model calls = %d, want 2 (the resumed turn continued)", len(reqs))
				}
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatal("turn did not complete after resume")
		}
	}
}

// TestResumeUnknownSessionIsStale: resuming a session the backend never had is
// refused, so the client opens a fresh one.
func TestResumeUnknownSessionIsStale(t *testing.T) {
	h := newControlHarness(t, llmtest.New())
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	_, token := h.user()

	ctx := context.Background()
	c, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/api/v1/session",
		&websocket.DialOptions{HTTPHeader: bearer(token)})
	if err != nil {
		t.Fatal(err)
	}
	tr := protocol.NewWSTransport(c)
	tr.WriteFrame(ctx, mustEncode(&protocol.SessionResume{Session: "s_nonexistent", WorkspaceKey: "wk1"}))
	frame, err := tr.ReadFrame(ctx)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := protocol.DecodeRaw(frame)
	if _, ok := msg.(*protocol.SessionStale); !ok {
		t.Fatalf("resume of unknown session = %T, want session.stale", msg)
	}
}

func readReady(t *testing.T, tr *protocol.WSTransport) *protocol.SessionReady {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frame, err := tr.ReadFrame(ctx)
	if err != nil {
		t.Fatalf("read ready: %v", err)
	}
	msg, err := protocol.DecodeRaw(frame)
	if err != nil {
		t.Fatalf("decode ready: %v", err)
	}
	ready, ok := msg.(*protocol.SessionReady)
	if !ok {
		t.Fatalf("expected session.ready, got %T: %+v", msg, msg)
	}
	return ready
}

// TestAResumeFromAnotherBuildIsRefused is a backend upgraded under a running
// session: the client comes back on its old build, and is told to update
// rather than taken back to carry on mismatched. A client too old to say what
// it is cannot be told apart from a development build, and is let through as
// before.
func TestAResumeFromAnotherBuildIsRefused(t *testing.T) {
	clientsFS = fstest.MapFS{"john-darwin-arm64.gz": {Data: gzBytes(make([]byte, 64))}}
	t.Cleanup(func() { clientsFS = nil })
	h := newControlHarness(t, llmtest.New())
	h.srv.cfg.Version = "0.5.2"
	_, token := h.user()

	resume := func(client protocol.ClientInfo) protocol.Message {
		ctx := context.Background()
		c, _, err := websocket.Dial(ctx, "ws://"+h.srv.Addr()+"/api/v1/session",
			&websocket.DialOptions{HTTPHeader: bearer(token)})
		if err != nil {
			t.Fatal(err)
		}
		tr := protocol.NewWSTransport(c)
		defer tr.Close()
		tr.WriteFrame(ctx, mustEncode(&protocol.SessionResume{
			Session: "s_from_before", WorkspaceKey: "wk1", Client: client}))
		frame, err := tr.ReadFrame(ctx)
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := protocol.DecodeRaw(frame)
		return msg
	}

	msg := resume(protocol.ClientInfo{Version: "0.4.1", OS: "darwin/arm64"})
	mm, ok := msg.(*protocol.VersionMismatch)
	if !ok {
		t.Fatalf("resume from 0.4.1 = %T, want version.mismatch", msg)
	}
	if mm.Required != "0.5.2" || mm.Download != "/releases/john-darwin-arm64" {
		t.Fatalf("mismatch = %+v", mm)
	}

	if msg := resume(protocol.ClientInfo{}); func() bool { _, ok := msg.(*protocol.VersionMismatch); return ok }() {
		t.Fatal("a client that did not say its version was refused on resume")
	}
}
