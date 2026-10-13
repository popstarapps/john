package terminalui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The boundary is the product: two channels of plain values and a Run that owns
// the terminal. These are the promises made about it.

func TestRunOnlyOnce(t *testing.T) {
	ui := New(Options{Output: io.Discard, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	ui.Send(Quit{})
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ui.Run(context.Background()); err != ErrClosed {
		t.Fatalf("a second Run should refuse, got %v", err)
	}
}

func TestActionsCloseAfterRun(t *testing.T) {
	ui := New(Options{Output: io.Discard, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	ui.Send(Quit{})
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-ui.Actions():
		if ok {
			t.Fatal("no action should arrive after Run")
		}
	case <-time.After(time.Second):
		t.Fatal("the actions channel should be closed after Run")
	}
}

// Send is safe from anywhere, at any time, including times when nobody is
// listening any more.
func TestSendIsAlwaysSafe(t *testing.T) {
	var buf bytes.Buffer
	ui := New(Options{Output: &buf, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})

	ui.Send(Notice{Text: "before Run"}) // queued
	ui.Send(Quit{})
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "before Run") {
		t.Fatalf("an event sent before Run was dropped: %q", buf.String())
	}

	// After Run, Send is a no-op rather than a panic or a block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			ui.Send(Notice{Text: "after Run"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked after Run returned")
	}
}

func TestSendFromManyGoroutines(t *testing.T) {
	var buf bytes.Buffer
	ui := New(Options{Output: &buf, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			for j := 0; j < 50; j++ {
				ui.Send(Notice{Text: "from " + itoa(n)})
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	ui.Send(Quit{})
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "from "); n != 400 {
		t.Fatalf("want 400 notices, got %d", n)
	}
}

func TestInteractiveDependsOnTheTerminal(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want bool
	}{
		{"a buffer is not a terminal", Options{Output: io.Discard, Input: strings.NewReader("")}, false},
		{"forced plain", Options{Output: io.Discard, Mode: ModePlain}, false},
		{"forced interactive", Options{Output: io.Discard, Mode: ModeInteractive}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := New(c.opts).Interactive(); got != c.want {
				t.Fatalf("Interactive() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestOptionDefaultsAndClamping(t *testing.T) {
	o := Options{}
	o.applyDefaults()
	if o.Input == nil || o.Output == nil {
		t.Fatal("stdin and stdout are the defaults")
	}
	if o.Placeholder == "" || o.BusyPlaceholder == "" {
		t.Fatal("both placeholders have defaults")
	}
	if o.HistoryLimit <= 0 || o.PasteLines <= 0 || o.BellAfter <= 0 || o.ActionBuffer <= 0 {
		t.Fatalf("missing defaults: %+v", o)
	}
	if o.MaxModalPercent != 60 || o.MaxLiveLines != 12 {
		t.Fatalf("modal/live defaults changed: %d, %d", o.MaxModalPercent, o.MaxLiveLines)
	}

	// A modal may not take the whole screen, nor a useless sliver of it.
	for _, in := range []int{1, 5, 19} {
		o := Options{MaxModalPercent: in}
		o.applyDefaults()
		if o.MaxModalPercent != 20 {
			t.Fatalf("%d should clamp up to 20, got %d", in, o.MaxModalPercent)
		}
	}
	for _, in := range []int{91, 100, 1000} {
		o := Options{MaxModalPercent: in}
		o.applyDefaults()
		if o.MaxModalPercent != 90 {
			t.Fatalf("%d should clamp down to 90, got %d", in, o.MaxModalPercent)
		}
	}
}

// New must not touch the terminal: the theme is built inside Run, because
// building one asks the terminal a question and the answer would arrive while
// the application is still starting.
func TestNewBuildsNoTheme(t *testing.T) {
	o := Options{Output: io.Discard, Input: strings.NewReader("")}
	o.applyDefaults()
	if o.Theme != nil {
		t.Fatal("applyDefaults must not build a theme")
	}
}

func TestDefaultChoicesHaveExactlyOneWayOut(t *testing.T) {
	for name, choices := range map[string][]Choice{
		"approval": DefaultApprovalOptions(),
		"confirm":  DefaultConfirmChoices(),
	} {
		t.Run(name, func(t *testing.T) {
			cancels, keys := 0, map[rune]bool{}
			for _, c := range choices {
				if c.Cancel {
					cancels++
				}
				if c.Key == 0 || c.Label == "" || c.Decision == "" {
					t.Fatalf("incomplete choice: %#v", c)
				}
				if keys[c.Key] {
					t.Fatalf("two choices answer to %q", c.Key)
				}
				keys[c.Key] = true
			}
			if cancels != 1 {
				t.Fatalf("want exactly one cancelling choice, got %d", cancels)
			}
		})
	}
}

func TestASCIISymbolsAreAllASCII(t *testing.T) {
	th := themeFromRenderer(lipgloss.NewRenderer(io.Discard))
	th.SetASCII()
	sym := th.Symbols
	for _, s := range []string{
		sym.User, sym.Assistant, sym.Tool, sym.Fail, sym.Blocked, sym.Skipped,
		sym.TreeMid, sym.TreeEnd, sym.Selected, sym.Unchecked, sym.Cursor,
		sym.Queued, sym.Alert, sym.BarFull, sym.BarEmpty, sym.Enter, sym.Ellipsis,
		sym.Dash, sym.Dot, sym.Add, sym.Remove, sym.BoxTL, sym.BoxTR, sym.BoxBL,
		sym.BoxBR, sym.BoxH, sym.BoxV, sym.BoxLT, sym.BoxRT, sym.Thumb, sym.Track,
		sym.ArrowLR, sym.ArrowUD, sym.Backspace,
	} {
		for _, r := range s {
			if r > 127 {
				t.Fatalf("ASCII glyph set contains %q in %q", r, s)
			}
		}
	}
	for _, f := range sym.Spinner {
		for _, r := range f {
			if r > 127 {
				t.Fatalf("ASCII spinner contains %q", r)
			}
		}
	}
}

func TestUnicodeSymbolsAreComplete(t *testing.T) {
	// Every glyph the renderers reach for has to exist in both sets, or a
	// terminal gets a blank where a marker should be.
	u, a := UnicodeSymbols(), ASCIISymbols()
	pairs := map[string][2]string{
		"User": {u.User, a.User}, "Assistant": {u.Assistant, a.Assistant},
		"Tool": {u.Tool, a.Tool}, "Fail": {u.Fail, a.Fail},
		"TreeMid": {u.TreeMid, a.TreeMid}, "TreeEnd": {u.TreeEnd, a.TreeEnd},
		"Queued": {u.Queued, a.Queued}, "Alert": {u.Alert, a.Alert},
		"BarFull": {u.BarFull, a.BarFull}, "BarEmpty": {u.BarEmpty, a.BarEmpty},
		"Thumb": {u.Thumb, a.Thumb}, "ArrowLR": {u.ArrowLR, a.ArrowLR},
		"ArrowUD": {u.ArrowUD, a.ArrowUD}, "Backspace": {u.Backspace, a.Backspace},
		"Enter": {u.Enter, a.Enter}, "Dash": {u.Dash, a.Dash},
	}
	for name, p := range pairs {
		if p[0] == "" || p[1] == "" {
			t.Fatalf("%s is missing from one of the glyph sets: %q / %q", name, p[0], p[1])
		}
	}
	if len(u.Spinner) == 0 || len(a.Spinner) == 0 {
		t.Fatal("both glyph sets need a spinner")
	}
}

func TestVerbAndStatusStyles(t *testing.T) {
	th := themeFromRenderer(lipgloss.NewRenderer(io.Discard))
	// A verb nobody configured still renders.
	if got := th.verbStyle("teleport").Render("x"); got != "x" {
		t.Fatalf("unknown verbs should fall back, got %q", got)
	}
	for _, st := range []ToolStatus{ToolOK, ToolFail, ToolBlocked, ToolSkipped} {
		glyph, _ := th.statusGlyph(st)
		if glyph == "" {
			t.Fatalf("status %d has no glyph", st)
		}
	}
}

func TestSyntaxStyleFollowsTheTerminal(t *testing.T) {
	dark := lipgloss.NewRenderer(io.Discard)
	dark.SetColorProfile(termenv.TrueColor)
	dark.SetHasDarkBackground(true)
	if got := syntaxStyleFor(dark); got != "github-dark" {
		t.Fatalf("dark terminal: %q", got)
	}
	if got := syntaxFormatterFor(dark); got != "terminal16m" {
		t.Fatalf("truecolor formatter: %q", got)
	}

	light := lipgloss.NewRenderer(io.Discard)
	light.SetColorProfile(termenv.ANSI256)
	light.SetHasDarkBackground(false)
	if got := syntaxStyleFor(light); got != "github" {
		t.Fatalf("light terminal: %q", got)
	}
	if got := syntaxFormatterFor(light); got != "terminal256" {
		t.Fatalf("256-colour formatter: %q", got)
	}

	none := lipgloss.NewRenderer(io.Discard)
	none.SetColorProfile(termenv.Ascii)
	if syntaxStyleFor(none) != "" || syntaxFormatterFor(none) != "" {
		t.Fatal("no colour, no highlighting")
	}
}

func TestFormatTokens(t *testing.T) {
	cases := map[int]string{0: "0", 42: "42", 999: "999", 1000: "1k", 6100: "6.1k",
		64_700: "64.7k", 200_000: "200k", 999_000: "999k", 1_000_000: "1M", 2_500_000: "2.5M"}
	for in, want := range cases {
		if got := formatTokens(in); got != want {
			t.Fatalf("formatTokens(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestQueueRefusesWorkAfterClose(t *testing.T) {
	q := newQueue[int]()
	if !q.push(1) {
		t.Fatal("a live queue accepts work")
	}
	q.close()
	if q.push(2) {
		t.Fatal("a closed queue accepts nothing")
	}
	// Closing twice is not an error, and what was pushed is still delivered.
	q.close()
	if v, ok := q.take(); !ok || v != 1 {
		t.Fatalf("take: %v, %v", v, ok)
	}
	if _, ok := q.take(); ok {
		t.Fatal("a drained, closed queue is done")
	}
}

func TestActionQueueDeliversBacklogThenCloses(t *testing.T) {
	a := newActionQueue(1)
	for i := 0; i < 20; i++ {
		a.push(UserPrompt{Text: itoa(i)})
	}
	a.stop()
	n := 0
	for act := range a.ch {
		if got := act.(UserPrompt).Text; got != itoa(n) {
			t.Fatalf("out of order at %d: %s", n, got)
		}
		n++
	}
	if n != 20 {
		t.Fatalf("want the whole backlog, got %d", n)
	}
}
