//go:build !windows

package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/popstarapps/john/terminalui"
	"github.com/popstarapps/john/vt"
)

// The demo is the first thing a newcomer runs, so it is worth knowing that it
// still runs. This is a smoke test, not a second copy of the package's suite:
// boot, a turn and its decisions, an apply, and a slash command.

type demoSession struct {
	t    *testing.T
	term *vt.Terminal
	ptmx *os.File
	mu   sync.Mutex
}

func newDemoSession(t *testing.T) *demoSession {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 40, Cols: 110}); err != nil {
		t.Fatalf("setsize: %v", err)
	}

	ui := terminalui.New(terminalui.Options{
		Input: tty, Output: tty, Mode: terminalui.ModeInteractive,
		Placeholder:    "ask john to do something",
		FileCompletion: true,
		Commands:       []terminalui.Command{{Name: "apply"}, {Name: "model"}, {Name: "help"}, {Name: "quit"}},
	})
	d := newDemo(ui, true)

	s := &demoSession{t: t, term: vt.New(110), ptmx: ptmx}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.term.Write(buf[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	d.boot()
	go d.loop(ctx)
	done := make(chan error, 1)
	go func() { done <- ui.Run(ctx) }()

	t.Cleanup(func() {
		// Quit first and let the program stop on its own: cancelling the
		// context out from under it races Bubble Tea's own shutdown against
		// its input reader.
		ui.Quit()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		cancel()
		// Only the master: the program owns the slave and closes it on the
		// way out, and closing it twice is a race on the same file.
		ptmx.Close()
	})
	s.wait("ask john to do something")
	return s
}

func (s *demoSession) screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.String()
}

func (s *demoSession) wait(text string) {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.screen(), text) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %q:\n%s", text, s.screen())
}

func (s *demoSession) type_(keys string) {
	s.t.Helper()
	if _, err := s.ptmx.WriteString(keys); err != nil {
		s.t.Fatalf("write: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
}

// turn runs the scripted turn to its end, answering what it asks.
func (s *demoSession) turn() {
	s.t.Helper()
	s.type_("fix the retry\r")
	s.wait("read billing/client.go")
	// The routing policy narrows the session before the file reaches the
	// model, and the turn carries on, on the model it leaves.
	s.wait("CONTINUE ON")
	s.wait(`keyword "PCI-SCOPE"`)
	s.type_("\r")
	s.wait("internal/codegen-l")
	// Only an ask-developer match asks. A request no policy matches is
	// blocked without a question, so a demo that asked about one would be
	// showing a product that does not exist.
	s.wait("APPROVAL REQUIRED")
	s.wait("matched an ask-developer policy")
	s.type_("a")
	s.wait("exit 1")
	// A host no policy covers is blocked without a question about the
	// request; what is asked is whether to request a policy for it.
	s.wait("REQUEST A POLICY")
	s.type_("\r")
	s.wait("REQ-117 filed for sum.golang.org")
	s.wait("429 is now retryable")
	// The summary streams in, and the turn is over only once all of it has.
	// A command typed before then races the turn's end, which closes anything
	// still open: an /apply started meanwhile was drawn as skipped.
	s.wait("run /apply to review")
}

func TestDemoRunsATurn(t *testing.T) {
	s := newDemoSession(t)
	s.wait("signed in as")
	s.turn()
	s.type_("/model\r")
	s.wait("switched by a routing policy")
}

func TestDemoApplies(t *testing.T) {
	s := newDemoSession(t)
	s.type_("/apply\r")
	s.wait("nothing to apply")

	s.turn()
	s.type_("/apply\r")
	s.wait("not selected (coverage/)")
	s.wait("billing/client.go")
	s.type_("a")
	s.wait("APPLY TO WORKING DIRECTORY")
	s.wait("Write 2 files")
	s.type_("y")
	s.wait("2 files → payments")
	s.wait("REQ-117 approved · sum.golang.org")
}

func TestDemoHelp(t *testing.T) {
	s := newDemoSession(t)
	s.type_("/help\r")
	s.wait("Commands")
}

func TestSplitKeepingSpace(t *testing.T) {
	got := splitKeepingSpace("one two  three")
	if strings.Join(got, "") != "one two  three" {
		t.Fatalf("splitting must not lose anything: %q", got)
	}
	for _, part := range got {
		if part == "" {
			t.Fatalf("empty fragment in %q", got)
		}
	}
}
