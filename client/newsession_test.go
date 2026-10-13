//go:build !windows

package client

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// /new leaves the conversation for /resume and carries on in an empty one,
// with the context figure starting again from nothing.
func TestNewStartsAnotherConversation(t *testing.T) {
	s := newE2E(t, llmtest.Text("first answer"), llmtest.Text("second answer"))
	s.type_("hello\r")
	s.wait("first answer")
	before := s.app.wire.currentSession()

	s.type_("/new\r")
	s.wait("new session · the last one is in /resume")
	after := s.app.wire.currentSession()
	if after == "" || after == before {
		t.Fatalf("still in %s after /new", after)
	}
	s.app.mu.Lock()
	tokens := s.app.tokens
	s.app.mu.Unlock()
	if tokens != 0 {
		t.Fatalf("the new conversation shows %d tokens of context", tokens)
	}
	if row, err := s.srv.Store().SessionByID(before); err != nil || row.Status != "ended" {
		t.Fatalf("the conversation /new left = %+v, %v", row, err)
	}
}

// Opening on a conversation john had before says so, once, and how to leave
// it; a new one says nothing.
func TestCarryingOnASessionIsSaid(t *testing.T) {
	out := &lockedBuffer{}
	ui := terminalui.New(terminalui.Options{Mode: terminalui.ModePlain, Output: out, Input: strings.NewReader("")})
	a := newApp(ui, sandbox.NewFake(t.TempDir()), Options{BackendURL: "http://127.0.0.1:1", Token: "t"})
	done := make(chan struct{})
	go func() { ui.Run(context.Background()); close(done) }()

	a.carryingOn(&protocol.SessionReady{TurnsSoFar: 0})
	started := time.Now().Add(-time.Hour)
	a.carryingOn(&protocol.SessionReady{TurnsSoFar: 7, StartedAt: started.UTC().Format(time.RFC3339)})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if strings.Contains(flat(out.String()), "/new starts a new one") {
			break
		}
	}
	ui.Quit()
	<-done

	// Plain output wraps at the terminal's width; what was said is the words.
	got := flat(out.String())
	want := "carrying on this workspace's session from " + started.Local().Format("15:04") + " · 7 turns so far · /new starts a new one"
	if started.Local().Day() != time.Now().Day() {
		want = "carrying on this workspace's session from " + started.Local().Format("Mon 2 Jan 15:04")
	}
	if !strings.Contains(got, want) {
		t.Fatalf("said:\n%s\nwant %q", got, want)
	}
	if n := strings.Count(got, "carrying on"); n != 1 {
		t.Fatalf("said %d times, want once (a new session says nothing):\n%s", n, got)
	}
}

// lockedBuffer is a buffer the UI writes while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func flat(s string) string { return strings.Join(strings.Fields(s), " ") }
