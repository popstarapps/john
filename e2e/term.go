package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aymanbagabas/go-pty"

	"github.com/popstarapps/john/vt"
)

// term is one john running in a pseudo-terminal, read into an emulated screen.
//
// It is the real program on a real terminal, which is the point: the TUI draws
// what a developer would see, and the screen kept here is what goes in the
// report and in front of the reviewer. Nothing waits on the screen to know a
// turn is over — that is the trace's job, see trace.go — because a screen is
// a picture, and deciding from a picture when a program is idle is guessing.
type term struct {
	pty  pty.Pty
	cmd  *pty.Cmd
	raw  *os.File
	done chan struct{}
	err  error

	mu     sync.Mutex
	screen *vt.Terminal
	// text is everything john wrote with the escape sequences taken out. It
	// only grows, which the screen does not — a redraw of the live region
	// makes the screen shorter — so "has this appeared since then" is asked
	// of the text, by its length then.
	text strings.Builder
	// tail is the end of the last read, kept so a terminal query split across
	// two reads is still recognised.
	tail []byte
}

const termWidth, termHeight = 120, 40

func startTerm(bin, dir string, env []string, rawLog string, args ...string) (*term, error) {
	p, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("opening a pseudo-terminal: %w", err)
	}
	if err := p.Resize(termWidth, termHeight); err != nil {
		p.Close()
		return nil, err
	}
	raw, err := os.OpenFile(rawLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		p.Close()
		return nil, err
	}
	c := p.Command(bin, args...)
	c.Dir = dir
	c.Env = env
	if err := c.Start(); err != nil {
		p.Close()
		raw.Close()
		return nil, fmt.Errorf("starting %s: %w", bin, err)
	}
	t := &term{pty: p, cmd: c, raw: raw, done: make(chan struct{}), screen: vt.NewSize(termWidth, termHeight)}
	go t.read()
	go func() {
		t.err = c.Wait()
		close(t.done)
	}()
	return t, nil
}

func (t *term) read() {
	buf := make([]byte, 32<<10)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			t.raw.Write(chunk)
			t.mu.Lock()
			t.screen.Write(chunk)
			t.text.WriteString(escapes.ReplaceAllString(string(chunk), ""))
			seen := len(t.tail)
			scan := append(t.tail, chunk...)
			t.tail = append([]byte(nil), scan[max(0, len(scan)-16):]...)
			t.mu.Unlock()
			t.answerQueries(scan, seen)
		}
		if err != nil {
			return
		}
	}
}

// answerQueries replies to what a terminal is asked. The TUI asks for the
// background colour and the cursor position, and on Windows ConPTY asks for
// the cursor before it draws anything at all; a terminal that never answers
// leaves them waiting on a timeout, or for ever.
//
// Answers go in the order the questions were asked, as a terminal's do. The
// order is load-bearing: the colour query is followed by a cursor query whose
// answer means "that is everything", so answering the cursor first reads as a
// terminal that knows no colours — and the colour answer, arriving after,
// lands in whatever john reads next as if it had been typed.
// seen is how much of b was already scanned with the last read: a query that
// ends there was answered then.
func (t *term) answerQueries(b []byte, seen int) {
	type answer struct {
		at    int
		reply string
	}
	var answers []answer
	for q, reply := range map[string]string{
		"\x1b[6n":   "\x1b[1;1R",
		"\x1b]11;?": "\x1b]11;rgb:0000/0000/0000\x1b\\",
		"\x1b]10;?": "\x1b]10;rgb:ffff/ffff/ffff\x1b\\",
		"\x1b[c":    "\x1b[?62;22c",
		"\x1b[0c":   "\x1b[?62;22c",
	} {
		rest, off := b, 0
		for {
			i := bytes.Index(rest, []byte(q))
			if i < 0 {
				break
			}
			if off+i+len(q) > seen {
				answers = append(answers, answer{off + i, reply})
			}
			off += i + len(q)
			rest = rest[i+len(q):]
		}
	}
	sort.Slice(answers, func(i, j int) bool { return answers[i].at < answers[j].at })
	for _, a := range answers {
		t.pty.Write([]byte(a.reply))
	}
}

// Screen is everything drawn so far. The emulator grows rather than
// scrolling, so this is the whole session, not the last forty lines.
func (t *term) Screen() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.screen.String()
}

// Exited reports whether john has ended.
func (t *term) Exited() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// escapes matches the terminal control sequences john writes: CSI, OSC, and
// the two-byte ones.
var escapes = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][0-9A-Za-z]|\x1b[=>78DEHMc]`)

// Mark is a point in what john has written, for Since and WaitFor.
func (t *term) Mark() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.text.Len()
}

// Since is what john has written after mark, escape sequences removed.
func (t *term) Since(mark int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.text.String()[min(mark, t.text.Len()):]
}

// WaitFor waits until john writes s after mark, so text from earlier in the
// session does not count.
func (t *term) WaitFor(ctx context.Context, s string, mark int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		// From the start, the screen counts too: it is drawn, so a space
		// the renderer wrote as a cursor move is a space there.
		if strings.Contains(t.Since(mark), s) || (mark == 0 && strings.Contains(t.Screen(), s)) {
			return nil
		}
		if t.Exited() {
			return fmt.Errorf("john exited (%v) before showing %q", t.err, s)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%q did not appear within %s", s, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Type sends text the way a person types it, then Enter. Keys go one at a
// time: a burst arrives as one read and can look like a paste.
func (t *term) Type(s string) {
	for _, r := range s {
		t.pty.Write([]byte(string(r)))
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	t.pty.Write([]byte("\r"))
}

// Key sends one keypress, unadorned.
func (t *term) Key(k string) { t.pty.Write([]byte(k)) }

// WaitExit waits for john to end on its own.
func (t *term) WaitExit(timeout time.Duration) error {
	select {
	case <-t.done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("john was still running after %s", timeout)
	}
}

// Kill ends john however it is. It is the cleanup, not a step: a step that
// wants john to stop asks it to.
func (t *term) Kill() {
	if !t.Exited() && t.cmd.Process != nil {
		t.cmd.Process.Kill()
		select {
		case <-t.done:
		case <-time.After(10 * time.Second):
		}
	}
	t.pty.Close()
	t.raw.Close()
}
