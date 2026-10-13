// Package terminalui renders the john command-line session.
//
// The package owns the terminal and nothing else. It knows how to draw a
// transcript, ask for a decision and read a prompt; it knows nothing about
// sandboxes, policies, models or diffs beyond the shapes it is handed. The
// application drives it through two typed streams:
//
//	ui := terminalui.New(terminalui.Options{})
//	go func() {
//	    ui.Send(terminalui.Notice{Text: "sandbox resumed"})
//	    for act := range ui.Actions() {
//	        switch a := act.(type) {
//	        case terminalui.UserPrompt:
//	            ...
//	        }
//	    }
//	}()
//	err := ui.Run(ctx)
//
// Events go in through Send, actions come out through Actions, and Run owns the
// terminal until it returns. Because the boundary is two channels of plain
// values, the whole UI can be driven by a script — see cmd/demo — and the
// application can be tested without a terminal at all.
//
// The transcript is written to the terminal's real scrollback: finished output
// is printed, never repainted, so it stays selectable, copyable and searchable
// with the terminal's own tools. Only the live region at the bottom — the
// in-flight turn, the input box, an approval or a diff review — is redrawn. The
// alternate screen is never used and the mouse is never captured, both for the
// same reason: they would take text selection away from the user.
package terminalui

import (
	"context"
	"errors"
	"os"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// ErrNotInteractive is returned by Run when the application asks for a decision
// that cannot be made without a terminal and Options.NonInteractive is
// NonInteractiveError.
var ErrNotInteractive = errors.New("terminalui: interactive decision required without a terminal")

// ErrClosed is returned by Run when it is called on a UI that has already run.
var ErrClosed = errors.New("terminalui: already run")

// UI is a terminal session. Create one with New, drive it with Send and
// Actions, and give it the terminal with Run.
type UI struct {
	opts Options

	events  *queue[Event]
	actions *actionQueue

	mu      sync.Mutex
	started bool
}

// New creates a UI. It does not touch the terminal; Run does.
func New(opts Options) *UI {
	opts.applyDefaults()
	return &UI{
		opts:    opts,
		events:  newQueue[Event](),
		actions: newActionQueue(opts.ActionBuffer),
	}
}

// Send hands an event to the UI. It never blocks and is safe from any
// goroutine. Events sent before Run are queued and drawn as soon as the UI
// starts; events sent after it returns are discarded.
func (u *UI) Send(events ...Event) {
	for _, ev := range events {
		u.events.push(ev)
	}
}

// Actions returns the stream of things the developer did. The channel is closed
// after Run returns and its backlog has been delivered, so ranging over it is a
// complete application loop.
func (u *UI) Actions() <-chan Action { return u.actions.ch }

// Quit asks the UI to shut down, as if the developer had pressed Ctrl+D. It is
// equivalent to Send(Quit{}).
func (u *UI) Quit() { u.Send(Quit{}) }

// Interactive reports whether Run will use the interactive renderer. It is
// useful for deciding whether to ask questions at all.
func (u *UI) Interactive() bool { return interactiveMode(&u.opts) }

// Run gives the terminal to the UI and blocks until the session ends: the
// developer quit, the application sent Quit, or ctx was cancelled.
//
// The terminal is always restored before Run returns, including on panic.
func (u *UI) Run(ctx context.Context) error {
	u.mu.Lock()
	if u.started {
		u.mu.Unlock()
		return ErrClosed
	}
	u.started = true
	u.mu.Unlock()

	defer func() {
		u.events.close()
		u.actions.stop()
	}()

	// Windows needs its console told to speak UTF-8 before anything is drawn.
	restore, utf8 := prepareConsole(u.opts.Output)
	defer restore()
	if u.opts.Theme == nil && (!utf8 || legacyConsole()) {
		// Nothing to draw box-drawing characters with: use the glyph set that
		// works everywhere rather than a screen of replacement characters.
		u.opts.ASCII = true
	}

	if !interactiveMode(&u.opts) {
		return u.runPlain(ctx)
	}
	return u.runInteractive(ctx)
}

func (u *UI) runInteractive(ctx context.Context) error {
	renderer := lipgloss.NewRenderer(u.opts.Output)
	// Ask the terminal what it looks like exactly once, here, before anything
	// starts reading keys. Every later query would race the input reader and
	// could be answered into the user's prompt.
	renderer.SetColorProfile(renderer.ColorProfile())
	renderer.SetHasDarkBackground(renderer.HasDarkBackground())
	if u.opts.Theme == nil {
		u.opts.Theme = themeFromRenderer(renderer)
		if u.opts.ASCII {
			u.opts.Theme.SetASCII()
		}
	}

	md := newMarkdownRenderer(!u.opts.PlainAssistantText, u.opts.ASCII, renderer)
	// Measure before starting, so the first frame is drawn at the real size
	// rather than at a guess that is corrected a moment later.
	width, height := terminalSize(u.opts.Output)
	model := newUIModel(&u.opts, u.actions, md, width, height)

	// A Windows console's keys are read here rather than by Bubble Tea,
	// which drops characters typed as more than one key; see input_windows.go.
	input := u.opts.Input
	keys, ownKeys := newConsoleKeys(u.opts.Input, u.opts.Output)
	if ownKeys {
		input = nil
		defer keys.close()
	}

	opts := []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithInput(input),
		tea.WithOutput(u.opts.Output),
		// Focus reporting, so a turn that ends while the developer is in
		// another window can say so, and one that ends in front of them stays
		// quiet.
		tea.WithReportFocus(),
		// No alt screen and no mouse capture: scrollback and text selection
		// stay the terminal's.
	}
	prog := tea.NewProgram(model, opts...)
	if ownKeys {
		go keys.run(prog.Send)
	}

	// Pump queued events into the program until the queue closes.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			ev, ok := u.events.take()
			if !ok {
				return
			}
			prog.Send(eventMsg{ev: ev})
		}
	}()

	_, err := prog.Run()

	u.events.close()
	<-pumpDone

	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// interactiveMode decides which renderer Run will use.
func interactiveMode(o *Options) bool {
	switch o.Mode {
	case ModeInteractive:
		return true
	case ModePlain:
		return false
	}
	return isTerminal(o.Output) && isTerminal(o.Input)
}

// terminalSize returns the size of w, or zeroes when it is not a terminal.
func terminalSize(w any) (int, int) {
	f, ok := w.(*os.File)
	if !ok {
		return 0, 0
	}
	width, height, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, 0
	}
	return width, height
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
