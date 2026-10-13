//go:build !windows

package client

import (
	"strings"
	"testing"
	"time"

	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/sandbox"
)

// TestATurnIsPickedUpWhenTheBackendComesBack is the developer's half of
// durable turns, on a real terminal.
//
// A turn is in flight — the sandbox is running a command — when the backend
// is taken away and started again. Before this, the client sat on "thinking"
// for a turn that no longer existed anywhere: the session had been reopened,
// the old numbered stream was gone, and nothing was ever going to answer it.
// Now the backend picks the turn up from the conversation it had already
// written, and the developer watches it finish.
func TestATurnIsPickedUpWhenTheBackendComesBack(t *testing.T) {
	release := make(chan struct{})
	s := newE2E(t,
		llmtest.ToolCall("run", map[string]any{"cmd": "go test ./..."}),
		llmtest.Text("never reached: this backend does not come back"),
	)
	// The command hangs, so the turn is genuinely in flight when the backend
	// goes away rather than finished before the test can act.
	s.sb.ExecFn = func(cmd, cwd string) sandbox.ExecResult {
		<-release
		return sandbox.ExecResult{ExitCode: 0, Stdout: []byte("PASS\n")}
	}

	s.type_("run the tests\r")
	s.wait("go test ./...") // the call reached the sandbox

	s.restartBackend(llmtest.Text("picked the turn up after the restart"))
	close(release)

	s.wait("picked the turn up after the restart")
	s.wait("carrying on the turn that was running when the backend stopped")

	// The model really was asked again by the new backend, with the
	// conversation the old one had written.
	reqs := s.fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("the restarted backend never called the model")
	}
	var joined strings.Builder
	for _, m := range reqs[0].Messages {
		for _, b := range m.Blocks {
			joined.WriteString(b.Text)
			joined.WriteString("\n")
		}
	}
	if !strings.Contains(joined.String(), "run the tests") {
		t.Fatalf("the resumed turn lost the conversation:\n%s", joined.String())
	}
}

// TestTheUIStopsThinkingWhenItsTurnDoesNotComeBack is the same moment with
// nothing to resume: the session is reopened and no turn is picked up, so the
// spinner has to stop and say why rather than think forever.
func TestTheUIStopsThinkingWhenItsTurnDoesNotComeBack(t *testing.T) {
	release := make(chan struct{})
	s := newE2E(t,
		llmtest.ToolCall("run", map[string]any{"cmd": "go test ./..."}),
		llmtest.Text("unused"),
	)
	s.sb.ExecFn = func(cmd, cwd string) sandbox.ExecResult {
		<-release
		return sandbox.ExecResult{ExitCode: 0}
	}
	s.type_("run the tests\r")
	s.wait("go test ./...")

	// The turn's own record is dropped, so the backend that comes back has a
	// conversation and no turn — a crash before the marker was written, or a
	// turn somebody ended elsewhere.
	session := s.app.wire.currentSession()
	if err := s.srv.Store().SetSessionTurn(session, ""); err != nil {
		t.Fatal(err)
	}
	s.restartBackend(llmtest.Text("unused"))
	close(release)

	s.wait("did not come with the reconnect")
	deadline := time.Now().Add(3 * time.Second * timeScale)
	for time.Now().Before(deadline) {
		if !strings.Contains(s.screen(), "thinking") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the UI is still thinking about a turn that is not coming back:\n%s", s.screen())
}
