//go:build !windows

package terminalui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/popstarapps/john/vt"
)

// session drives a real UI over a real pty, into an emulated terminal.
//
// Everything else in this file's tests is checked against the model directly,
// which proves the logic but not the drawing. This proves the drawing: the
// bytes that leave the program are interpreted by something that wraps, scrolls
// and re-flows the way a terminal does.
type session struct {
	t    *testing.T
	ui   *UI
	term *vt.Terminal

	ptmx *os.File
	tty  *os.File
	done chan error
	raw  *bytes.Buffer

	mu sync.Mutex
}

func newSession(t *testing.T, opts Options, width, height int) *session {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	if err := pty.Setsize(tty, &pty.Winsize{Rows: uint16(height), Cols: uint16(width)}); err != nil {
		t.Fatalf("setsize: %v", err)
	}

	opts.Input = tty
	opts.Output = tty
	opts.Mode = ModeInteractive
	if opts.Theme == nil {
		opts.Theme = testTheme()
	}

	s := &session{
		t: t, term: vt.New(width), ui: New(opts),
		ptmx: ptmx, tty: tty, done: make(chan error, 1),
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.term.Write(buf[:n])
				if s.raw != nil {
					s.raw.Write(buf[:n])
				}
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { s.done <- s.ui.Run(context.Background()) }()

	t.Cleanup(func() {
		s.ui.Quit()
		select {
		case <-s.done:
		case <-time.After(3 * time.Second):
			t.Log("ui did not stop")
		}
		// Only the master: the program owns the slave and closes it on the
		// way out, and closing it twice is a race on the same file.
		ptmx.Close()
	})
	s.settle()
	return s
}

// settle waits for the UI to draw whatever it has been told about.
func (s *session) settle() {
	time.Sleep(120 * time.Millisecond)
}

func (s *session) send(events ...Event) {
	s.ui.Send(events...)
	s.settle()
}

func (s *session) type_(keys string) {
	s.ptmx.WriteString(keys)
	s.settle()
}

func (s *session) resize(width, height int) {
	s.mu.Lock()
	s.term.Resize(width)
	s.mu.Unlock()
	if err := pty.Setsize(s.tty, &pty.Winsize{Rows: uint16(height), Cols: uint16(width)}); err != nil {
		s.t.Fatalf("resize: %v", err)
	}
	// The test process is not the pty's session leader, so the kernel will not
	// deliver SIGWINCH for us.
	syscall.Kill(os.Getpid(), syscall.SIGWINCH)
	s.settle()
}

// recordRaw starts keeping every byte the program writes, for the tests that
// care whether something reached the terminal at all rather than where it
// landed on screen.
func (s *session) recordRaw() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = &bytes.Buffer{}
}

func (s *session) rawString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.raw == nil {
		return ""
	}
	return s.raw.String()
}

// awaitRaw waits for text to appear anywhere in the program's output.
func (s *session) awaitRaw(text string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(s.rawString(), text) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (s *session) screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.String()
}

func (s *session) count(text string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.Count(text)
}

func (s *session) bells() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.Bells()
}

func (s *session) mustContain(what ...string) {
	s.t.Helper()
	screen := s.screen()
	for _, w := range what {
		if !strings.Contains(screen, w) {
			s.t.Fatalf("screen is missing %q:\n%s", w, screen)
		}
	}
}

// A whole turn, drawn by the real renderer into a real terminal.
func TestPTYSessionDraws(t *testing.T) {
	s := newSession(t, Options{Placeholder: "ask john to do something"}, 90, 24)
	s.send(
		Notice{Text: "sandbox resumed"},
		Status{Workdir: "~/src/payments", Model: "internal/codegen-l"},
	)
	s.mustContain("sandbox resumed", "ask john to do something", "~/src/payments")

	s.send(ToolStart{ID: "1", Verb: "run", Target: "go test ./billing/..."})
	s.mustContain("go test ./billing/...")
	s.send(ToolEnd{ID: "1", Status: ToolFail, Meta: "FAIL"})
	s.mustContain("FAIL")

	// Typing reaches the input box.
	s.type_("fix the retry")
	s.mustContain("fix the retry")

	// And submitting commits it to the transcript, where it stays put.
	s.type_("\r")
	if got := s.count("fix the retry"); got != 1 {
		t.Fatalf("the prompt should appear once, found %d:\n%s", got, s.screen())
	}
}

// The regression test for the resize bug: a frame drawn at one width, then the
// window shrunk, must not leave a copy of the input box behind.
func TestPTYResizeLeavesNoDebris(t *testing.T) {
	s := newSession(t, Options{Placeholder: "ask john to do something"}, 120, 24)
	s.send(
		Notice{Text: "sandbox resumed"},
		Status{
			Workdir: "~/src/payments", Model: "internal/codegen-l",
			Sandbox: "sandbox ready", Tokens: 6100,
		},
	)
	s.mustContain("ask john to do something")

	for _, w := range []int{80, 60, 100, 45, 120} {
		s.resize(w, 24)
		s.send(Notice{Text: "still here"}) // force a redraw
		if got := s.count("ask john to do something"); got != 1 {
			t.Fatalf("after resizing to %d columns the input box appears %d times:\n%s",
				w, got, s.screen())
		}
	}
}

// Nothing the UI draws may fill the terminal's last column, because that is
// what makes the terminal re-flow a row the renderer thinks it owns.
func TestPTYNothingTouchesTheLastColumn(t *testing.T) {
	for _, width := range []int{60, 90, 120} {
		s := newSession(t, Options{Placeholder: "ask john to do something"}, width, 24)
		s.send(
			Status{
				Workdir: "~/src/payments", Model: "internal/codegen-l",
				Sandbox: "sandbox ready", Tokens: 6100, Alert: "backend unreachable",
			},
			Busy{Label: "thinking"},
			Thinking{Text: strings.Repeat("a long thought that runs on and on ", 6)},
			ToolStart{ID: "1", Verb: "run", Target: strings.Repeat("go test ./billing/... ", 8)},
		)
		for _, l := range strings.Split(s.screen(), "\n") {
			if len([]rune(l)) >= width {
				t.Fatalf("@%d a line reaches the last column (%d): %q", width, len([]rune(l)), l)
			}
		}
	}
}

func TestPTYApprovalDecision(t *testing.T) {
	s := newSession(t, Options{Placeholder: "ask"}, 100, 30)
	s.send(ApprovalRequest{
		ID: "r1", Method: "POST", URL: "https://telemetry.vendor.io/v1/events",
		Headers: []Header{{Name: "content-type", Value: "application/json"}},
		Body:    `{"event":"build.finished"}`,
		Verdict: "ask developer", Policy: "policy group staging-writes",
	})
	s.mustContain("APPROVAL REQUIRED", "telemetry.vendor.io", "allow once", "reject")

	s.type_("a")
	select {
	case act := <-s.ui.Actions():
		d, ok := act.(ApprovalDecision)
		if !ok || d.Decision != DecisionAllowOnce {
			t.Fatalf("want allow-once, got %#v", act)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no decision came back:\n%s", s.screen())
	}
}

// The bell has to reach the terminal, and only when it should.
func TestPTYBellReachesTheTerminal(t *testing.T) {
	s := newSession(t, Options{Placeholder: "ask", Bell: BellAlways, BellAfter: time.Millisecond}, 90, 24)
	s.send(Busy{Label: "thinking"})
	time.Sleep(20 * time.Millisecond)
	s.send(AssistantMessage{Text: "done"}, Idle{})
	if s.bells() == 0 {
		t.Fatal("expected the terminal bell to ring at the end of the turn")
	}
}

func TestPTYQuietWhenBellDisabled(t *testing.T) {
	s := newSession(t, Options{Placeholder: "ask", Bell: BellNever}, 90, 24)
	s.send(Busy{Label: "thinking"})
	time.Sleep(20 * time.Millisecond)
	s.send(AssistantMessage{Text: "done"}, Idle{})
	if got := s.bells(); got != 0 {
		t.Fatalf("expected silence, got %d bells", got)
	}
}

// The last thing an application says has to survive the exit.
//
// `john apply` ends by reporting what it wrote and quitting immediately after,
// with nothing in between to give the renderer a chance to draw. That closing
// line used to be lost about half the time: the transcript is committed with
// tea.Println, which only queues lines inside the renderer, and the flush that
// would have written them out is skipped when the final frame is empty. Which
// of the two happened first was decided by a 60fps ticker.
//
// Several attempts, because the bug this pins was a race that a single run
// would pass through half the time.
func TestPTYFinalOutputSurvivesAnImmediateQuit(t *testing.T) {
	const meta = "3 files -> ~/src/app"
	for attempt := 0; attempt < 8; attempt++ {
		t.Run(fmt.Sprintf("attempt-%d", attempt), func(t *testing.T) {
			s := newSession(t, Options{}, 90, 24)
			s.recordRaw()
			s.send(ToolStart{ID: "apply", Verb: "apply", Target: "3 files"})

			// Deliberately no settle between these two — that is the race.
			s.ui.Send(ToolEnd{ID: "apply", Status: ToolOK, Meta: meta}, Quit{})

			if !s.awaitRaw(meta, 3*time.Second) {
				t.Fatalf("the closing line never reached the terminal\nraw output:\n%q", s.rawString())
			}
		})
	}
}
