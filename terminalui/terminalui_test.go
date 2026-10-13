package terminalui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// testTheme builds a theme that renders no escape sequences, so tests can
// assert on text instead of colors.
func testTheme() *Theme {
	t := themeFromRenderer(lipgloss.NewRenderer(io.Discard))
	// The newline hint is the one glyph in a frame that differs by platform, so
	// a golden recorded on one machine would not match on another. Pin it, the
	// way the spinner and the terminal size in newTestModel are pinned;
	// TestTheNewlineHint covers the rule this pins away.
	t.Symbols.Newline = "alt+⏎"
	return t
}

// TestTheNewlineHint is what testTheme pins away: the hint has to name a key
// that actually reaches john on the platform the hint is drawn on.
func TestTheNewlineHint(t *testing.T) {
	for name, s := range map[string]Symbols{"unicode": UnicodeSymbols(), "ascii": ASCIISymbols()} {
		t.Run(name, func(t *testing.T) {
			if s.Newline == "" {
				t.Fatal("no newline hint: the status line would name no way to make one")
			}
			if runtime.GOOS == "windows" && strings.Contains(s.Newline, "alt+") {
				t.Errorf("the hint names %q, which the Windows console takes as its fullscreen toggle before john ever sees it", s.Newline)
			}
		})
	}
}

func testOptions(out io.Writer) Options {
	o := Options{
		Output:         out,
		Input:          strings.NewReader(""),
		Theme:          testTheme(),
		FileCompletion: true,
		// Pinned, so a test never depends on which animation a turn drew.
		// The tests that care about the random draw ask for it explicitly.
		BusySpinner: SpinnerBraille,
		BusyEffect:  EffectNone,
		Commands: []Command{
			{Name: "diff", Description: "review changes"},
			{Name: "apply", Description: "apply changes"},
			{Name: "model", Description: "show model"},
		},
	}
	o.applyDefaults()
	return o
}

func newTestModel(t *testing.T, width, height int) (*uiModel, *actionQueue) {
	t.Helper()
	o := testOptions(io.Discard)
	q := newActionQueue(64)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 80, 24)
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return m, q
}

func drain(q *actionQueue, want int, timeout time.Duration) []Action {
	var got []Action
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case a, ok := <-q.ch:
			if !ok {
				return got
			}
			got = append(got, a)
		case <-deadline:
			return got
		}
	}
	return got
}

func send(m *uiModel, events ...Event) {
	for _, ev := range events {
		m.Update(eventMsg{ev: ev})
	}
}

func press(m *uiModel, keys ...string) {
	for _, k := range keys {
		m.Update(keyMsg(k))
	}
}

// keyMsg builds the tea.KeyMsg for a key name the way a terminal would deliver
// it. Only the forms the tests use are handled.
func keyMsg(name string) tea.KeyMsg {
	switch name {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

func typeText(m *uiModel, s string) {
	for _, r := range s {
		if r == ' ' {
			m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
			continue
		}
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestSubmitEmitsPrompt(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	typeText(m, "fix the retry")
	press(m, "enter")

	acts := drain(q, 1, time.Second)
	if len(acts) != 1 {
		t.Fatalf("want 1 action, got %d", len(acts))
	}
	p, ok := acts[0].(UserPrompt)
	if !ok || p.Text != "fix the retry" {
		t.Fatalf("want UserPrompt{fix the retry}, got %#v", acts[0])
	}
	if m.input.value() != "" {
		t.Fatalf("input not cleared: %q", m.input.value())
	}
}

func TestSlashCommandParsed(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	typeText(m, "/apply billing/client.go")
	press(m, "enter")

	acts := drain(q, 1, time.Second)
	c, ok := acts[0].(SlashCommand)
	if !ok {
		t.Fatalf("want SlashCommand, got %#v", acts[0])
	}
	if c.Name != "apply" || c.Args != "billing/client.go" {
		t.Fatalf("bad parse: %#v", c)
	}
}

// Enter must send what was typed, not silently swap in a completion the
// developer never chose.
func TestEnterSubmitsOverUntouchedPopup(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	typeText(m, "/mod")
	if !m.input.pop.active || len(m.input.pop.items) == 0 {
		t.Fatal("expected the slash popup to be open")
	}
	press(m, "enter")

	acts := drain(q, 1, time.Second)
	c, ok := acts[0].(SlashCommand)
	if !ok || c.Raw != "/mod" {
		t.Fatalf("want the typed text submitted, got %#v", acts[0])
	}
}

func TestPopupAcceptedAfterArrow(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	typeText(m, "/m")
	press(m, "down") // wraps to the first item and marks the choice deliberate
	press(m, "enter")
	if got := m.input.value(); got != "/model " {
		t.Fatalf("want the completion inserted, got %q", got)
	}
}

func TestFileCompletionRequestAndInsert(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	typeText(m, "look at @bil")

	// One request per keystroke; the last one is the live query.
	acts := drain(q, 4, 500*time.Millisecond)
	if len(acts) == 0 {
		t.Fatal("no completion request")
	}
	req, ok := acts[len(acts)-1].(CompletionRequest)
	if !ok || req.Kind != CompletionFile || req.Query != "bil" {
		t.Fatalf("want CompletionRequest{file, bil}, got %#v", acts)
	}
	send(m, Completions{Token: req.Token, Items: []Completion{{Value: "billing/client.go"}}})
	press(m, "tab")
	if got := m.input.value(); got != "look at @billing/client.go " {
		t.Fatalf("bad insertion: %q", got)
	}
}

func TestStaleCompletionsIgnored(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	typeText(m, "@bil")
	send(m, Completions{Token: 9999, Items: []Completion{{Value: "nope.go"}}})
	for _, it := range m.input.pop.items {
		if it.Value == "nope.go" {
			t.Fatal("a stale completion answer was accepted")
		}
	}
}

func TestHistoryNavigation(t *testing.T) {
	o := testOptions(io.Discard)
	o.History = []string{"first", "second"}
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 80, 24)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	press(m, "up")
	if m.input.value() != "second" {
		t.Fatalf("want second, got %q", m.input.value())
	}
	press(m, "up")
	if m.input.value() != "first" {
		t.Fatalf("want first, got %q", m.input.value())
	}
	press(m, "down", "down")
	if m.input.value() != "" {
		t.Fatalf("want the draft back, got %q", m.input.value())
	}
}

func TestEscInterruptsWhenBusy(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, Busy{Label: "thinking"})
	press(m, "esc")

	acts := drain(q, 1, time.Second)
	if len(acts) != 1 {
		t.Fatalf("want an Interrupt, got %#v", acts)
	}
	if _, ok := acts[0].(Interrupt); !ok {
		t.Fatalf("want Interrupt, got %#v", acts[0])
	}
}

func TestApprovalDecision(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, ApprovalRequest{
		ID:      "r1",
		Method:  "POST",
		URL:     "https://telemetry.vendor.io/v1/events",
		Headers: []Header{{Name: "content-type", Value: "application/json"}},
		Body:    `{"a":1}`,
		Verdict: "ask developer",
	})
	if m.approval == nil {
		t.Fatal("approval inspector did not open")
	}
	// Keys go to the inspector, not the input box.
	press(m, "j", "a")

	acts := drain(q, 1, time.Second)
	d, ok := acts[0].(ApprovalDecision)
	if !ok || d.ID != "r1" || d.Decision != DecisionAllowOnce {
		t.Fatalf("want allow-once for r1, got %#v", acts[0])
	}
	if m.approval != nil {
		t.Fatal("inspector stayed open after a decision")
	}
	if m.input.value() != "" {
		t.Fatalf("modal keys leaked into the input: %q", m.input.value())
	}
}

func TestApprovalEscRejects(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, ApprovalRequest{ID: "r1", Method: "GET", URL: "https://x/y"})
	press(m, "esc")

	acts := drain(q, 1, time.Second)
	d, ok := acts[0].(ApprovalDecision)
	if !ok || d.Decision != DecisionReject {
		t.Fatalf("dismissing must reject, got %#v", acts[0])
	}
}

func TestApprovalCancelledByApplication(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, ApprovalRequest{ID: "r1", Method: "GET", URL: "https://x/y"})
	send(m, CancelApproval{ID: "r1", Reason: "timed out"})
	if m.approval != nil {
		t.Fatal("inspector should have closed")
	}
	if acts := drain(q, 1, 100*time.Millisecond); len(acts) != 0 {
		t.Fatalf("a withdrawn approval must not produce a decision, got %#v", acts)
	}
}

func TestDiffReviewApplySelected(t *testing.T) {
	m, q := newTestModel(t, 100, 40)
	send(m, DiffReview{
		ID:    "d1",
		Title: "apply",
		Files: []DiffFile{
			{Path: "a.go", Added: 3, Selected: true, Hunks: []DiffHunk{{Header: "@@", Lines: []DiffLine{{Kind: DiffAdd, Text: "x"}}}}},
			{Path: "b.go", Added: 1},
		},
	})
	if m.diff == nil {
		t.Fatal("diff viewer did not open")
	}
	// Move to b.go and select it too.
	press(m, "j", "space", "a")

	acts := drain(q, 1, time.Second)
	d, ok := acts[0].(DiffDecision)
	if !ok || !d.Apply {
		t.Fatalf("want an apply decision, got %#v", acts[0])
	}
	if strings.Join(d.Paths, ",") != "a.go,b.go" {
		t.Fatalf("want both files in order, got %v", d.Paths)
	}
}

func TestDiffReviewCancel(t *testing.T) {
	m, q := newTestModel(t, 100, 40)
	send(m, DiffReview{ID: "d1", Files: []DiffFile{{Path: "a.go", Selected: true}}})
	press(m, "q")

	acts := drain(q, 1, time.Second)
	d, ok := acts[0].(DiffDecision)
	if !ok || d.Apply || len(d.Paths) != 0 {
		t.Fatalf("cancel must apply nothing, got %#v", acts[0])
	}
}

// Nothing may be applied by accident: with no file selected, "a" is inert.
func TestDiffApplyNeedsSelection(t *testing.T) {
	m, q := newTestModel(t, 100, 40)
	send(m, DiffReview{ID: "d1", Files: []DiffFile{{Path: "a.go"}}})
	press(m, "a")
	if acts := drain(q, 1, 100*time.Millisecond); len(acts) != 0 {
		t.Fatalf("want no decision, got %#v", acts)
	}
	if m.diff == nil {
		t.Fatal("viewer should stay open")
	}
}

func TestCtrlCClearsThenQuits(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	typeText(m, "half a thought")
	press(m, "ctrl+c")
	if m.input.value() != "" {
		t.Fatal("first ctrl+c should clear the input")
	}
	press(m, "ctrl+c")
	if m.quitting {
		t.Fatal("one ctrl+c on an empty prompt must not quit")
	}
	press(m, "ctrl+c")
	if !m.quitting {
		t.Fatal("two ctrl+c in a row should quit")
	}
}

func TestTranscriptCommitsInOrder(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	// Two tools open at once; the second finishes first. The transcript must
	// still commit them in the order they started.
	send(m,
		ToolStart{ID: "1", Verb: "run", Target: "go test ./..."},
		ToolStart{ID: "2", Verb: "read", Target: "go.mod"},
		ToolEnd{ID: "2", Status: ToolOK},
	)
	if len(m.pending) != 2 {
		t.Fatalf("the finished tool must wait its turn, pending=%d", len(m.pending))
	}
	send(m, ToolEnd{ID: "1", Status: ToolFail, Meta: "FAIL"})
	if len(m.pending) != 0 {
		t.Fatalf("both tools should have committed, pending=%d", len(m.pending))
	}
}

func TestIdleClosesForgottenTools(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, Busy{}, ToolStart{ID: "1", Verb: "run", Target: "sleep 100"}, Idle{})
	if len(m.pending) != 0 {
		t.Fatalf("Idle must not leave the queue wedged, pending=%d", len(m.pending))
	}
}

func TestLiveRegionShowsRunningWork(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, Busy{Label: "thinking"}, Thinking{Text: "reading the retry policy"},
		ToolStart{ID: "1", Verb: "run", Target: "go test ./billing/..."})
	view := m.View()
	for _, want := range []string{"go test ./billing/...", "thinking", "esc to interrupt", "reading the retry policy"} {
		if !strings.Contains(view, want) {
			t.Fatalf("live region missing %q:\n%s", want, view)
		}
	}
}

func TestModalHeightIsCapped(t *testing.T) {
	m, _ := newTestModel(t, 100, 40)
	lines := make([]DiffLine, 500)
	for i := range lines {
		lines[i] = DiffLine{Kind: DiffAdd, Text: "line"}
	}
	send(m, DiffReview{ID: "d1", Files: []DiffFile{
		{Path: "big.go", Added: 500, Selected: true, Hunks: []DiffHunk{{Header: "@@", Lines: lines}}},
	}})
	got := len(strings.Split(m.View(), "\n"))
	if got > 40 {
		t.Fatalf("the live region overflowed the terminal: %d lines", got)
	}
	if got < 10 {
		t.Fatalf("the viewer collapsed to %d lines", got)
	}
}

// Tool output is untrusted. An escape sequence in a test's output must not be
// able to repaint the screen.
func TestControlCharactersAreStripped(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, ToolStart{ID: "1", Verb: "run", Target: "go test"},
		ToolDetail{ID: "1", Text: "evil\x1b[2J\x1b[Hcleared your screen"},
		ToolEnd{ID: "1", Status: ToolOK})
	b := &block{kind: kindTool, verb: "run", target: "go test", done: true,
		details: []string{"evil\x1b[2J\x1b[Hcleared"}}
	out := strings.Join(b.render(m.th, m.md, 100, "*"), "\n")
	if strings.Contains(out, "\x1b[2J") {
		t.Fatalf("escape sequence survived: %q", out)
	}
}

func TestPlainRendererWritesReadableLog(t *testing.T) {
	var buf bytes.Buffer
	o := Options{Output: &buf, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()}
	ui := New(o)
	ui.Send(
		Notice{Text: "sandbox resumed"},
		ToolStart{ID: "1", Verb: "run", Target: "go test ./..."},
		ToolDetail{ID: "1", Text: "--- FAIL: TestRetry"},
		ToolEnd{ID: "1", Status: ToolFail, Meta: "FAIL"},
		AssistantMessage{Text: "the retry now backs off"},
		Quit{},
	)
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"sandbox resumed", "run go test ./...", "--- FAIL: TestRetry", "the retry now backs off"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plain output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("plain output must contain no escape sequences:\n%q", out)
	}
}

func TestPlainRendererRejectsApprovals(t *testing.T) {
	var buf bytes.Buffer
	ui := New(Options{Output: &buf, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	ui.Send(ApprovalRequest{ID: "r1", Method: "POST", URL: "https://telemetry.vendor.io/v1/events"})

	done := make(chan error, 1)
	go func() { done <- ui.Run(context.Background()) }()

	select {
	case act := <-ui.Actions():
		d, ok := act.(ApprovalDecision)
		if !ok || d.Decision != DecisionReject || d.Reason == "" {
			t.Fatalf("want a reasoned rejection, got %#v", act)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no decision from the plain renderer")
	}
	ui.Quit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPlainRendererCanFailClosed(t *testing.T) {
	var buf bytes.Buffer
	ui := New(Options{
		Output: &buf, Input: strings.NewReader(""), Mode: ModePlain,
		NonInteractive: NonInteractiveError, Theme: testTheme(),
	})
	ui.Send(ApprovalRequest{ID: "r1", Method: "POST", URL: "https://x/y"})
	err := ui.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "interactive decision required") {
		t.Fatalf("want ErrNotInteractive, got %v", err)
	}
}

func TestRunReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ui := New(Options{Output: io.Discard, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := ui.Run(ctx); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if _, ok := <-ui.Actions(); ok {
		t.Fatal("actions channel should be closed after Run")
	}
}

func TestActionsPreserveOrder(t *testing.T) {
	q := newActionQueue(4)
	const n = 500
	go func() {
		for i := 0; i < n; i++ {
			q.push(UserPrompt{Text: itoa(i)})
		}
		q.stop()
	}()
	i := 0
	for act := range q.ch {
		if got := act.(UserPrompt).Text; got != itoa(i) {
			t.Fatalf("out of order at %d: %s", i, got)
		}
		i++
	}
	if i != n {
		t.Fatalf("want %d actions, got %d", n, i)
	}
}

func TestSendBeforeRunIsNotLost(t *testing.T) {
	var buf bytes.Buffer
	ui := New(Options{Output: &buf, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	ui.Send(Notice{Text: "queued before Run"}, Quit{})
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "queued before Run") {
		t.Fatalf("event queued before Run was dropped: %q", buf.String())
	}
}

func TestWrapAndTruncate(t *testing.T) {
	lines := wrapPlain("the quick brown fox jumps", 10)
	for _, l := range lines {
		if visibleWidth(l) > 10 {
			t.Fatalf("line too wide: %q", l)
		}
	}
	if got := truncPlain("abcdefghij", 5, "…"); visibleWidth(got) > 5 {
		t.Fatalf("truncate overflowed: %q", got)
	}
	// A word longer than the width must still be broken, not overflow.
	for _, l := range wrapPlain(strings.Repeat("x", 40), 8) {
		if visibleWidth(l) > 8 {
			t.Fatalf("long word overflowed: %q", l)
		}
	}
}

func TestTriggerDetection(t *testing.T) {
	o := testOptions(io.Discard)
	p := newPromptInput(&o)
	p.setWidth(80)

	p.ta.SetValue("/dif")
	kind, _, col, q, ok := p.detectTrigger(&o)
	if !ok || kind != CompletionSlash || col != 0 || q != "dif" {
		t.Fatalf("slash trigger: ok=%v kind=%v col=%d q=%q", ok, kind, col, q)
	}

	// A slash mid-sentence is just a slash.
	p.ta.SetValue("look in src/ for it")
	if _, _, _, _, ok := p.detectTrigger(&o); ok {
		t.Fatal("a mid-line slash must not open the command popup")
	}

	p.ta.SetValue("check @bill")
	kind, _, col, q, ok = p.detectTrigger(&o)
	if !ok || kind != CompletionFile || col != 6 || q != "bill" {
		t.Fatalf("file trigger: ok=%v kind=%v col=%d q=%q", ok, kind, col, q)
	}

	// An email address is not a mention.
	p.ta.SetValue("mail oliver@example.com")
	if _, _, _, _, ok := p.detectTrigger(&o); ok {
		t.Fatal("an @ inside a word must not trigger file completion")
	}
}

// The proxy can buffer several requests at once. Each one is owed exactly one
// decision, so a second request must queue rather than replace the first.
func TestConcurrentApprovalsQueue(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m,
		ApprovalRequest{ID: "r1", Method: "GET", URL: "https://a/1"},
		ApprovalRequest{ID: "r2", Method: "GET", URL: "https://b/2"},
	)
	if m.approval == nil || m.approval.req.ID != "r1" {
		t.Fatal("the first request should be on screen")
	}
	press(m, "a")
	if m.approval == nil || m.approval.req.ID != "r2" {
		t.Fatalf("the queued request should have opened, got %#v", m.approval)
	}
	press(m, "r")

	acts := drain(q, 2, time.Second)
	if len(acts) != 2 {
		t.Fatalf("want a decision for each request, got %#v", acts)
	}
	first := acts[0].(ApprovalDecision)
	second := acts[1].(ApprovalDecision)
	if first.ID != "r1" || first.Decision != DecisionAllowOnce {
		t.Fatalf("bad first decision: %#v", first)
	}
	if second.ID != "r2" || second.Decision != DecisionReject {
		t.Fatalf("bad second decision: %#v", second)
	}
}

func TestQueuedRequestCanBeWithdrawn(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m,
		ApprovalRequest{ID: "r1", Method: "GET", URL: "https://a/1"},
		ApprovalRequest{ID: "r2", Method: "GET", URL: "https://b/2"},
		CancelApproval{ID: "r2", Reason: "upstream timed out"},
	)
	press(m, "a")
	if m.approval != nil {
		t.Fatal("a withdrawn request must not open")
	}
	if acts := drain(q, 2, 200*time.Millisecond); len(acts) != 1 {
		t.Fatalf("want one decision, got %#v", acts)
	}
}

// richState puts the UI into a state that exercises every part of the frame.
func richState(m *uiModel) {
	send(m,
		Status{Workdir: "~/src/payments", Model: "internal/codegen-l", Sandbox: "sandbox ready", Tokens: 6100},
		Busy{Label: "thinking"},
		Thinking{Text: "the 429 branch returns early instead of backing off, so nothing is retried"},
		ToolStart{ID: "1", Verb: "run", Target: "go test ./billing/... -run TestRetry429 -count=1 -v"},
		ToolDetail{ID: "1", Text: "--- FAIL: TestRetry429 (0.02s) client_test.go:88: gave up after 1 attempt"},
	)
}

// Every line the UI draws must stay one column short of the terminal. A line
// that fills the last column makes the terminal mark the row as wrapped, and a
// later resize then re-flows it and strands a copy of the frame on screen.
func TestFrameNeverFillsTerminalWidth(t *testing.T) {
	states := map[string]func(*uiModel){
		"idle":   func(m *uiModel) { send(m, Status{Workdir: "~/src/payments", Model: "internal/codegen-l"}) },
		"busy":   richState,
		"typing": func(m *uiModel) { richState(m); typeText(m, "explain the retry policy in billing and ledger") },
		"popup":  func(m *uiModel) { richState(m); typeText(m, "/") },
		"queued": func(m *uiModel) {
			richState(m)
			for _, q := range []string{"also cover the 503 path", strings.Repeat("a long follow-up ", 12)} {
				typeText(m, q)
				press(m, "enter")
			}
		},
		"approval": func(m *uiModel) { richState(m); send(m, sampleApproval()) },
		"diff":     func(m *uiModel) { richState(m); send(m, sampleDiffReview()) },
		"model":    func(m *uiModel) { richState(m); send(m, sampleModelChoice()) },
	}
	for name, setup := range states {
		for _, width := range []int{24, 40, 60, 80, 100, 120, 200} {
			for _, height := range []int{10, 24, 40} {
				m, _ := newTestModel(t, width, height)
				setup(m)
				lines := strings.Split(m.View(), "\n")
				for i, l := range lines {
					if w := visibleWidth(l); w >= width {
						t.Fatalf("%s @%dx%d: line %d is %d columns wide, terminal is %d: %q",
							name, width, height, i, w, width, l)
					}
				}
				if len(lines) >= height {
					t.Fatalf("%s @%dx%d: frame is %d lines tall", name, width, height, len(lines))
				}
			}
		}
	}
}

// Committed transcript lines go through the same rule: a wrapped-flag row in
// scrollback re-flows on resize too.
func TestCommittedLinesNeverFillTerminalWidth(t *testing.T) {
	for _, width := range []int{24, 40, 80, 120} {
		m, _ := newTestModel(t, width, 30)
		blocks := []*block{
			{kind: kindUser, text: strings.Repeat("fix the retry in billing/client.go ", 6), done: true},
			{kind: kindNotice, text: strings.Repeat("sandbox resumed and the toolchain cache was warm ", 3), done: true},
			{kind: kindTool, verb: "run", target: strings.Repeat("go test ./billing/... ", 8),
				meta: "FAIL · 1 test", done: true, details: []string{strings.Repeat("client_test.go:88 gave up ", 6)}},
		}
		for _, b := range blocks {
			for _, l := range b.render(m.th, m.md, m.frameWidth(), "*") {
				if w := visibleWidth(l); w >= width {
					t.Fatalf("@%d: committed line is %d columns: %q", width, w, l)
				}
			}
		}
	}
}

// Resizing must re-render everything at the new width, including views that
// cache their layout.
func TestResizeRelaysEverything(t *testing.T) {
	m, _ := newTestModel(t, 120, 40)
	richState(m)
	send(m, sampleApproval())
	_ = m.View()

	for _, size := range []struct{ w, h int }{{60, 20}, {200, 50}, {30, 12}, {90, 30}} {
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		lines := strings.Split(m.View(), "\n")
		for i, l := range lines {
			if w := visibleWidth(l); w >= size.w {
				t.Fatalf("after resize to %dx%d: line %d is %d columns: %q", size.w, size.h, i, w, l)
			}
		}
		if len(lines) >= size.h {
			t.Fatalf("after resize to %dx%d: frame is %d lines", size.w, size.h, len(lines))
		}
	}
}

// The input box pads its rows out to whatever width it is given, so it must be
// given the frame width and re-sized on every resize.
func TestInputBoxWidthFollowsResize(t *testing.T) {
	m, _ := newTestModel(t, 120, 30)
	for _, w := range []int{60, 200, 24} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		for _, l := range strings.Split(m.input.view(), "\n") {
			if got := visibleWidth(l); got >= w {
				t.Fatalf("input row is %d columns at width %d: %q", got, w, l)
			}
		}
	}
}

func TestNarrowTerminalDoesNotPanic(t *testing.T) {
	for _, w := range []int{1, 5, 12, 20} {
		m, _ := newTestModel(t, w, 6)
		richState(m)
		send(m, sampleApproval())
		_ = m.View()
		send(m, CancelApproval{ID: "r1"}, sampleDiffReview())
		_ = m.View()
	}
}

func sampleApproval() ApprovalRequest {
	return ApprovalRequest{
		ID: "r1", Method: "POST", URL: "https://telemetry.vendor.io/v1/events?project=payments&host=sandbox",
		Headers: []Header{
			{Name: "content-type", Value: "application/json"},
			{Name: "authorization", Value: strings.Repeat("Bearer sk-live-x", 8)},
		},
		Body:    strings.Repeat(`{"event":"build.finished","duration_ms":31402}`, 4),
		Verdict: "ask developer", Policy: "policy group staging-writes",
	}
}

func sampleDiffReview() DiffReview {
	lines := make([]DiffLine, 40)
	for i := range lines {
		lines[i] = DiffLine{Kind: DiffAdd, Text: strings.Repeat("if resp.StatusCode == http.StatusTooManyRequests { ", 3)}
	}
	return DiffReview{
		ID: "d1", Title: "apply to ~/src/payments",
		Files: []DiffFile{{Path: strings.Repeat("billing/", 8) + "client.go", Added: 40, Selected: true,
			Hunks: []DiffHunk{{Header: "@@ -18,10 +18,38 @@ func (c *Client) do(req *http.Request) (*http.Response, error)", Lines: lines}}}},
	}
}

func sampleModelChoice() ModelChoice {
	return ModelChoice{
		ID: "mc1", Detail: strings.Repeat("payments-restricted ", 4),
		Reason: `keyword "C++" seen in ` + strings.Repeat("billing/", 8) + "bridge.cc",
		Policy: "this session continues on an internally hosted model",
		Models: []ModelOption{
			{ID: "m1", Name: "internal/codegen-l", Detail: strings.Repeat("128k context ", 6), Internal: true},
			{ID: "m2", Name: "internal/codegen-s", Detail: "32k context", Internal: true},
			{ID: "m3", Name: "vendor/frontier-xl", Detail: "external provider"},
		},
	}
}

// The picker is the one box with no safe answer: the session has already
// narrowed, so declining has to mean the turn does not run rather than
// "carry on with whatever was routed before".
func TestModelPickerChoosesAndCancels(t *testing.T) {
	m, q := newTestModel(t, 100, 30)

	send(m, sampleModelChoice())
	press(m, "down")
	press(m, "enter")
	dec, ok := drain(q, 1, time.Second)[0].(ModelDecision)
	if !ok || dec.ModelID != "m2" || dec.Cancelled {
		t.Fatalf("enter on the second row should pick m2, got %#v", dec)
	}
	if m.picker != nil {
		t.Fatal("the box should have closed")
	}

	send(m, sampleModelChoice())
	press(m, "esc")
	dec = drain(q, 1, time.Second)[0].(ModelDecision)
	if !dec.Cancelled || dec.ModelID != "" {
		t.Fatalf("esc should cancel the turn, got %#v", dec)
	}
}

// A developer who knows the list should not have to walk it.
func TestModelPickerDigitKeysPick(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, sampleModelChoice())
	press(m, "3")
	dec := drain(q, 1, time.Second)[0].(ModelDecision)
	if dec.ModelID != "m3" {
		t.Fatalf("3 should pick the third model, got %q", dec.ModelID)
	}
	// A digit past the end of the list is not a decision.
	send(m, sampleModelChoice())
	press(m, "9")
	if acts := drain(q, 1, 50*time.Millisecond); len(acts) != 0 {
		t.Fatalf("9 with three models should do nothing, got %#v", acts)
	}
}

// The cursor stays on a model row: the reason above it is prose, not an
// option, and Enter on it would answer nothing.
func TestModelPickerCursorStaysOnModels(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, sampleModelChoice())
	m.View()
	for i := 0; i < 6; i++ {
		press(m, "up")
	}
	if k := m.picker.rowKind(m.picker.cursor); k != pickerModel {
		t.Fatal("up walked the cursor off the model rows")
	}
	for i := 0; i < 9; i++ {
		press(m, "down")
	}
	if k := m.picker.rowKind(m.picker.cursor); k != pickerModel {
		t.Fatal("down walked the cursor past the model rows")
	}
	if got := m.picker.selected(); got != 2 {
		t.Fatalf("down should stop on the last model, got index %d", got)
	}
}

func TestApprovalDefaultsToTheSafeChoice(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, sampleApproval())
	press(m, "enter")

	acts := drain(q, 1, time.Second)
	d, ok := acts[0].(ApprovalDecision)
	if !ok || d.Decision != DecisionReject {
		t.Fatalf("enter on an untouched approval must reject, got %#v", acts[0])
	}
}

func TestApprovalButtonsNavigateWithArrows(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, sampleApproval())
	// Options are allow-once, reject, request-access; the cursor starts on
	// reject, so one step left is allow-once.
	press(m, "left", "enter")

	acts := drain(q, 1, time.Second)
	d, ok := acts[0].(ApprovalDecision)
	if !ok || d.Decision != DecisionAllowOnce {
		t.Fatalf("want allow-once after arrowing left, got %#v", acts[0])
	}
}

func TestApprovalSelectionWraps(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, sampleApproval())
	start := m.approval.sel
	for range m.approval.choices {
		press(m, "right")
	}
	if m.approval.sel != start {
		t.Fatalf("a full lap should return to %d, got %d", start, m.approval.sel)
	}
}

// Scrolling and choosing must not fight over the same keys.
func TestApprovalArrowsScrollAndChooseSeparately(t *testing.T) {
	// A short terminal, so the request is taller than the box and can scroll.
	m, _ := newTestModel(t, 100, 16)
	send(m, sampleApproval())
	sel, off := m.approval.sel, m.approval.offset
	press(m, "down")
	if m.approval.sel != sel {
		t.Fatal("down must scroll, not change the selection")
	}
	if m.approval.offset == off {
		t.Fatal("down must scroll the request")
	}
	off = m.approval.offset
	press(m, "right")
	if m.approval.offset != off {
		t.Fatal("right must choose, not scroll")
	}
}

// A box has to agree with itself: every row the same width, or the borders come
// apart. Both modal views are drawn the same way, so both are checked.
func TestBoxesAreRectangular(t *testing.T) {
	views := map[string]Event{
		"approval": sampleApproval(),
		"diff":     sampleDiffReview(),
		"model":    sampleModelChoice(),
	}
	for name, ev := range views {
		for _, width := range []int{40, 60, 100, 160} {
			m, _ := newTestModel(t, width, 30)
			send(m, ev)
			checkBox(t, m, name, width)
		}
	}
}

func checkBox(t *testing.T, m *uiModel, name string, width int) {
	t.Helper()
	{
		var widths []int
		for _, l := range strings.Split(m.View(), "\n") {
			if strings.ContainsAny(l, m.th.Symbols.BoxV+m.th.Symbols.BoxTL+m.th.Symbols.BoxBL+m.th.Symbols.BoxLT) {
				widths = append(widths, visibleWidth(l))
			}
		}
		if len(widths) < 4 {
			t.Fatalf("%s @%d: expected a box, found %d border rows", name, width, len(widths))
		}
		for _, w := range widths {
			if w != widths[0] {
				t.Fatalf("%s @%d: ragged box, rows are %v", name, width, widths)
			}
		}
		if widths[0] != m.frameWidth() {
			t.Fatalf("%s @%d: box is %d wide, frame is %d", name, width, widths[0], m.frameWidth())
		}
	}
}

// The input box is exactly the frame's width on every row, whatever its edges
// say, and with colour on it paints no background: it sits on the terminal's.
func TestInputBoxFitsAndIsUnpainted(t *testing.T) {
	for _, width := range []int{24, 40, 80, 120} {
		m, _ := newTestModel(t, width, 30)
		send(m, Status{Workdir: "~/src/payments/services/billing", Model: "internal/codegen-l",
			Sandbox: "3 unapplied · /apply", Tokens: 6100, Window: 200_000}, Busy{})
		for _, l := range m.inputBox() {
			if got := visibleWidth(l); got != m.frameWidth() {
				t.Fatalf("@%d: box row is %d columns, frame is %d: %q", width, got, m.frameWidth(), stripANSI(l))
			}
		}
	}

	o := testOptions(io.Discard)
	o.Theme = colorTheme()
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	send(m, Status{Workdir: "~/src/payments", Model: "internal/codegen-l", Tokens: 6100})
	box := strings.Join(m.inputBox(), "\n")
	if !strings.Contains(box, "\x1b[") {
		t.Fatalf("expected a styled box, got %q", box)
	}
	for _, seq := range []string{"48;2;", "48;5;"} {
		if strings.Contains(box, seq) {
			t.Fatalf("the input box must paint no background, found %q in %q", seq, box)
		}
	}

}

// Actions stack under the prompt that caused them, indented and without gaps;
// an answer gets a blank line on either side.
func TestActionsStackUnderPrompt(t *testing.T) {
	m, _ := newTestModel(t, 100, 40)
	typeText(m, "fix the retry")
	press(m, "enter")
	send(m,
		Busy{Label: "thinking"},
		ToolStart{ID: "1", Verb: "edit", Target: "billing/client.go"},
		ToolStart{ID: "2", Verb: "run", Target: "go test ./..."},
		AssistantMessage{Text: "done"},
		ToolStart{ID: "3", Verb: "read", Target: "go.mod"},
	)
	if m.liveGap() {
		t.Fatal("the first action should sit directly under the prompt")
	}
	var got []string
	for _, l := range m.liveLines(40) {
		got = append(got, strings.TrimRight(ansi.Strip(l), " "))
	}
	want := []string{
		"  · edit billing/client.go",
		"  · run go test ./...",
		"",
		"  → done",
		"",
		"  · read go.mod",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// After the turn, the next block is not nested.
	send(m, Idle{}, Notice{Text: "sandbox paused"})
	if m.last == nil || m.last.text != "sandbox paused" || m.last.nested {
		t.Fatal("a notice after the turn ended should not be nested under its prompt")
	}
}

// A scroll position and a scroll hint are promises. Neither is made when the
// whole request is already on screen — that is what makes j/k look broken when
// it is working perfectly.
func TestApprovalPromisesScrollingOnlyWhenThereIsMore(t *testing.T) {
	tall, _ := newTestModel(t, 100, 60) // the request fits
	send(tall, sampleApproval())
	tall.approval.layout(boxInnerWidth(tall.frameWidth()))
	h := tall.modalHeight(len(tall.approval.lines), approvalChrome)
	if tall.approval.scrollable(h) {
		t.Fatalf("expected the request to fit in %d rows, it has %d lines", h, len(tall.approval.lines))
	}
	if got := tall.approval.scrollInfo(h); got != "" {
		t.Fatalf("no scroll position should be shown when everything fits, got %q", got)
	}
	if strings.Contains(tall.approval.hints(h), "scroll") {
		t.Fatal("no scroll hint should be offered when everything fits")
	}
	if tall.th.scrollbar(0, h, len(tall.approval.lines)) != nil {
		t.Fatal("no scrollbar should be drawn when everything fits")
	}

	short, _ := newTestModel(t, 100, 18) // the request does not fit
	send(short, sampleApproval())
	short.approval.layout(boxInnerWidth(short.frameWidth()))
	h = short.modalHeight(len(short.approval.lines), approvalChrome)
	if !short.approval.scrollable(h) {
		t.Fatalf("expected the request to overflow %d rows", h)
	}
	if got := short.approval.scrollInfo(h); !strings.Contains(got, "of") {
		t.Fatalf("want a range like \"1–8 of 12\", got %q", got)
	}
	if !strings.Contains(short.approval.hints(h), "scroll") {
		t.Fatal("want a scroll hint when the request overflows")
	}
	if bar := short.th.scrollbar(0, h, len(short.approval.lines)); len(bar) != h {
		t.Fatalf("want one scrollbar glyph per visible row, got %d for %d rows", len(bar), h)
	}
}

func TestApprovalScrollsWithJK(t *testing.T) {
	m, _ := newTestModel(t, 100, 18)
	send(m, sampleApproval())
	m.approval.layout(boxInnerWidth(m.frameWidth()))
	h := m.modalHeight(len(m.approval.lines), approvalChrome)
	if !m.approval.scrollable(h) {
		t.Skip("request fits; nothing to scroll")
	}
	press(m, "j", "j")
	if m.approval.offset != 2 {
		t.Fatalf("j should scroll down, offset is %d", m.approval.offset)
	}
	press(m, "k")
	if m.approval.offset != 1 {
		t.Fatalf("k should scroll up, offset is %d", m.approval.offset)
	}
	// The thumb must move with the content.
	top := m.th.scrollbar(0, h, len(m.approval.lines))
	bottom := m.th.scrollbar(len(m.approval.lines)-h, h, len(m.approval.lines))
	if top[0] == bottom[0] && top[len(top)-1] == bottom[len(bottom)-1] {
		t.Fatal("the scrollbar thumb does not move")
	}
}

// The title row is one row: it has to be the same width as the box it sits in,
// whatever the request says.
func TestTitleRowFitsTheBox(t *testing.T) {
	headers := []boxHeader{
		{Label: "APPROVAL REQUIRED", Detail: "POST https://telemetry.vendor.io/v1/events", Right: "1–8 of 12"},
		{Label: "REVIEW CHANGES", Detail: "apply to ~/src/payments"},
		{Label: "X"},
	}
	for _, width := range []int{30, 50, 100, 200} {
		m, _ := newTestModel(t, width, 30)
		for _, h := range headers {
			title := m.th.boxTitle(h, m.frameWidth())
			if got := visibleWidth(title); got != m.frameWidth() {
				t.Fatalf("@%d: title row is %d columns, frame is %d: %q", width, got, m.frameWidth(), title)
			}
		}
	}
}

// The keys that drive the box live in the box, beside the buttons they drive.
func TestApprovalHintsRideAlongsideTheButtons(t *testing.T) {
	m, _ := newTestModel(t, 120, 30)
	send(m, sampleApproval())
	view := m.View()

	var buttonRow string
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "allow once") {
			buttonRow = l
		}
	}
	if buttonRow == "" {
		t.Fatal("no button row")
	}
	for _, want := range []string{"choose", "confirm"} {
		if !strings.Contains(buttonRow, want) {
			t.Fatalf("button row is missing %q: %q", want, buttonRow)
		}
	}
	// Esc still rejects, but it is not advertised next to the decision.
	if strings.Contains(view, "esc") {
		t.Fatalf("esc should not be offered as a way out of the decision:\n%s", view)
	}
	// And it drops out entirely when the terminal cannot hold both.
	narrow, _ := newTestModel(t, 46, 30)
	send(narrow, sampleApproval())
	for _, l := range strings.Split(narrow.View(), "\n") {
		if strings.Contains(l, "allow once") && strings.Contains(l, "choose") {
			t.Fatalf("hints should be dropped when there is no room: %q", l)
		}
	}
}

func TestToolDetailGutterIsEmpty(t *testing.T) {
	m, _ := newTestModel(t, 80, 30)
	b := &block{kind: kindTool, verb: "run", target: "go test", done: true,
		details: []string{"first", "second"}}
	lines := b.render(m.th, m.md, 78, "*")
	if len(lines) != 3 {
		t.Fatalf("want a head and two details, got %q", lines)
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "  ") {
			t.Fatalf("detail lines start in an empty gutter, got %q", l)
		}
		if strings.ContainsAny(l, m.th.Symbols.BoxV) {
			t.Fatalf("no rule should run down the gutter, got %q", l)
		}
	}
}

func TestLogoOpensTheSession(t *testing.T) {
	o := testOptions(io.Discard)
	o.Logo = true
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	if len(m.pending) != 1 || m.pending[0].kind != kindArt {
		t.Fatalf("the logo should be queued ahead of everything, got %d blocks", len(m.pending))
	}
	// It must be at the head even when the application sends events first.
	send(m, Notice{Text: "sandbox resumed"})
	if len(m.pending) != 0 {
		t.Fatalf("both blocks should have committed, %d pending", len(m.pending))
	}

	for _, width := range []int{12, 17, 30, 60, 100, 200} {
		for _, l := range LogoArt(m.th, width) {
			if got := visibleWidth(l); got > width {
				t.Fatalf("@%d: logo line is %d columns: %q", width, got, l)
			}
		}
	}
	// Narrow terminals get a name, not a wrapped mess.
	if got := LogoArt(m.th, 12); len(got) != 1 {
		t.Fatalf("want a one-line logo at 12 columns, got %d lines", len(got))
	}
}

// The diff viewer is drawn like the approval box, and says the same kinds of
// things in the same places.
func TestDiffViewerWearsTheBox(t *testing.T) {
	m, _ := newTestModel(t, 110, 30)
	send(m, sampleDiffReview())
	view := m.View()
	lines := strings.Split(view, "\n")

	var title, buttons, bottom string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "REVIEW CHANGES"):
			title = l
		case strings.Contains(l, "apply selected"):
			buttons = l
		case strings.HasPrefix(l, m.th.Symbols.BoxBL):
			bottom = l
		}
	}
	if title == "" {
		t.Fatalf("no title row:\n%s", view)
	}
	if !strings.Contains(title, m.th.Symbols.BoxH) {
		t.Fatalf("the title row should be ruled out to the edge: %q", title)
	}
	if buttons == "" || !strings.Contains(buttons, "cancel") {
		t.Fatalf("no button row:\n%s", view)
	}
	// Totals ride on the bottom border so they stay put while hunks scroll.
	if !strings.Contains(bottom, "selected") {
		t.Fatalf("the summary should sit on the bottom border: %q", bottom)
	}
}

// colorTheme is a theme that actually emits colour, for the handful of
// assertions that are about styling rather than layout.
func colorTheme() *Theme {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	r.SetHasDarkBackground(true)
	return themeFromRenderer(r)
}

// The apply button is inert until something is selected, and says so.
func TestDiffApplyButtonReflectsSelection(t *testing.T) {
	v := newDiffView(DiffReview{ID: "d1", Files: []DiffFile{{Path: "a.go", Added: 1}}}, colorTheme(), nil)
	v.layout(80)
	off := v.buttonRow(80, 10)
	v.sel[0] = true
	v.dirty = true
	v.layout(80)
	on := v.buttonRow(80, 10)
	if off == on {
		t.Fatal("the apply button should change once something is selected")
	}
	if !strings.Contains(off, "apply selected") || !strings.Contains(on, "apply selected") {
		t.Fatalf("both states should still name the action:\n%q\n%q", off, on)
	}
}

// The title row carries no background: inside a drawn box, a filled bar reads
// as a second frame.
func TestTitleRowIsRuledNotPainted(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, sampleApproval())
	for _, l := range strings.Split(m.View(), "\n") {
		if !strings.Contains(l, "APPROVAL REQUIRED") {
			continue
		}
		if !strings.Contains(l, m.th.Symbols.BoxH) {
			t.Fatalf("the title row should be filled with the border rule: %q", l)
		}
		return
	}
	t.Fatal("no title row found")
}

// The full badge stays compact and puts both brand accents on its middle row.
func TestLogoIsCompactBadge(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	art := LogoArt(m.th, 100)
	if len(art) != 3 {
		t.Fatalf("the logo badge should be three rows tall, got %d", len(art))
	}
	if want := " │  >_  john.  │"; art[1] != want {
		t.Fatalf("badge middle row = %q, want %q", art[1], want)
	}
	for i, row := range art {
		if got := visibleWidth(row); got != logoWidth+1 {
			t.Fatalf("row %d is %d columns, want %d: %q", i, got, logoWidth+1, row)
		}
	}

	colored := LogoArt(colorTheme(), 100)[1]
	for _, accent := range []string{">_", "."} {
		if !strings.Contains(colored, colorTheme().LogoAccent.Render(accent)) {
			t.Fatalf("middle row does not render %q with the logo accent: %q", accent, colored)
		}
	}
}

// The input field sits in a box, with a blank row above it.
func TestInputFieldIsBoxed(t *testing.T) {
	for _, width := range []int{40, 80, 140} {
		m, _ := newTestModel(t, width, 30)
		send(m, Status{Workdir: "~/src/payments"})
		lines := strings.Split(m.View(), "\n")

		top := -1
		for i, l := range lines {
			if strings.HasPrefix(l, m.th.Symbols.BoxTL) {
				top = i
				break
			}
		}
		if top < 0 {
			t.Fatalf("@%d: no box around the input:\n%s", width, strings.Join(lines, "\n"))
		}
		if top == 0 || strings.TrimSpace(lines[top-1]) != "" {
			t.Fatalf("@%d: want a blank row above the box, got %q", width, lines[top-1])
		}
		if len(lines) != top+3 {
			t.Fatalf("@%d: want the box to end the frame: %q", width, lines[top:])
		}
		if !strings.Contains(lines[top+1], m.th.Symbols.User) {
			t.Fatalf("@%d: want the input inside the box, got %q", width, lines[top+1])
		}
		if !strings.HasPrefix(lines[top+2], m.th.Symbols.BoxBL) {
			t.Fatalf("@%d: want the box closed under the input, got %q", width, lines[top+2])
		}
		for _, l := range lines[top:] {
			if visibleWidth(l) != m.frameWidth() {
				t.Fatalf("@%d: box row is %d columns, frame is %d", width, visibleWidth(l), m.frameWidth())
			}
		}
	}
}

// The completion popup belongs to the input, so it sits inside its box.
func TestCompletionPopupSitsInsideTheBox(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	typeText(m, "/")
	lines := strings.Split(m.View(), "\n")

	var top, input int = -1, -1
	for i, l := range lines {
		if top < 0 && strings.HasPrefix(l, m.th.Symbols.BoxTL) {
			top = i
		}
		if strings.Contains(l, "/diff") && strings.Contains(l, "review changes") {
			input = i
		}
	}
	if top < 0 || input < 0 {
		t.Fatalf("expected a box and a popup:\n%s", strings.Join(lines, "\n"))
	}
	if input < top || !strings.HasPrefix(lines[input], m.th.Symbols.BoxV) {
		t.Fatal("the popup should be inside the box")
	}
}

// Everything in the live region is separated the same way the transcript is:
// one blank line between elements, none inside one.
func TestLiveRegionSeparatesElements(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m,
		ToolStart{ID: "1", Verb: "run", Target: "go test ./billing/..."},
		Busy{Label: "thinking"},
		Thinking{Text: "the 429 branch returns early"},
	)
	lines := strings.Split(m.View(), "\n")

	var tool, spinner, think = -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.Contains(l, "go test ./billing/..."):
			tool = i
		case strings.Contains(stripANSI(l), "thinking"):
			spinner = i
		case strings.Contains(l, "429 branch returns early"):
			think = i
		}
	}
	if tool < 0 || spinner < 0 || think < 0 {
		t.Fatalf("expected a tool, a spinner and a thought:\n%s", strings.Join(lines, "\n"))
	}
	// Separate elements: a blank line between them.
	if spinner != tool+2 || strings.TrimSpace(lines[tool+1]) != "" {
		t.Fatalf("want a blank line between the tool and the spinner:\n%s", strings.Join(lines, "\n"))
	}
	// One element: the thought hangs off the spinner, no blank.
	if think != spinner+1 {
		t.Fatalf("the thought belongs to the spinner, not apart from it:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[think], m.th.Symbols.TreeEnd) {
		t.Fatalf("the thought should hang off the spinner: %q", lines[think])
	}
	// And the whole live region is kept off the box below it.
	for i, l := range lines {
		if strings.HasPrefix(l, m.th.Symbols.BoxTL) {
			if strings.TrimSpace(lines[i-1]) != "" {
				t.Fatalf("want a blank line above the box, got %q", lines[i-1])
			}
			break
		}
	}
}

// A modal view is an element too: it does not butt against the work above it.
func TestModalIsSeparatedFromLiveWork(t *testing.T) {
	m, _ := newTestModel(t, 100, 40)
	send(m, ToolStart{ID: "1", Verb: "run", Target: "go test ./..."}, sampleApproval())
	lines := strings.Split(m.View(), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, m.th.Symbols.BoxTL) {
			if i == 0 || strings.TrimSpace(lines[i-1]) != "" {
				t.Fatalf("want a blank line above the box, got %q:\n%s", lines[i-1], strings.Join(lines, "\n"))
			}
			return
		}
	}
	t.Fatal("no box drawn")
}

// ASCII mode has to be all the way ASCII. It is what a Windows console that
// cannot be switched to UTF-8 falls back to, and a single stray box-drawing
// character there is a replacement glyph in the middle of the frame.
func TestASCIIModeEmitsOnlyASCII(t *testing.T) {
	o := testOptions(io.Discard)
	o.ASCII = true
	o.Placeholder = "ask john to do something"
	o.Theme = themeFromRenderer(lipgloss.NewRenderer(io.Discard))
	o.Theme.SetASCII()
	q := newActionQueue(64)
	m := newUIModel(&o, q, newMarkdownRenderer(true, true, lipgloss.NewRenderer(io.Discard)), 100, 30)

	check := func(what string, lines []string) {
		t.Helper()
		for _, l := range lines {
			for _, r := range l {
				if r > 127 {
					t.Fatalf("%s: non-ASCII %q in %q", what, r, l)
				}
			}
		}
	}

	check("logo", LogoArt(m.th, 100))
	check("logo (narrow)", LogoArt(m.th, 30))

	send(m,
		Status{Workdir: "~/src/payments", Model: "internal/codegen-l", Tokens: 6100},
		Busy{Label: "thinking"},
		Thinking{Text: "the retry policy"},
		ToolStart{ID: "1", Verb: "run", Target: "go test ./..."},
		ToolDetail{ID: "1", Text: "--- FAIL: TestRetry429"},
	)
	check("live region", strings.Split(m.View(), "\n"))

	// A tall request and a tall diff, so the scrollbars and ranges show up too.
	send(m, sampleApproval())
	check("approval", strings.Split(m.View(), "\n"))
	press(m, "esc")
	send(m, sampleDiffReview())
	check("diff", strings.Split(m.View(), "\n"))
	press(m, "q")
	send(m, sampleModelChoice())
	check("model picker", strings.Split(m.View(), "\n"))
	press(m, "q")

	// Committed blocks, including markdown.
	for _, b := range []*block{
		{kind: kindUser, text: "fix the retry", done: true},
		{kind: kindNotice, text: "sandbox resumed", level: NoticeWarn, done: true},
		{kind: kindTool, verb: "run", target: "go test", done: true, status: ToolFail,
			details: []string{"--- FAIL"}},
	} {
		check("transcript", b.render(m.th, m.md, 98, "*"))
	}
	md := &block{kind: kindAssistant, done: true}
	md.sb.WriteString("**done**\n\n- one\n- two\n\n```go\nx := 1\n```\n")
	check("markdown", md.render(m.th, m.md, 98, "*"))
}

// Typing while the agent works queues the prompt instead of racing the turn or
// interrupting it.
func TestPromptsQueueWhileBusy(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, Busy{Label: "thinking"})

	typeText(m, "also cover the 503 path")
	press(m, "enter")
	typeText(m, "then run the linter")
	press(m, "enter")

	if acts := drain(q, 1, 150*time.Millisecond); len(acts) != 0 {
		t.Fatalf("nothing should be sent while busy, got %#v", acts)
	}
	if len(m.queued) != 2 {
		t.Fatalf("want two queued prompts, got %v", m.queued)
	}
	// They are visible, and they are not in the transcript yet.
	view := m.View()
	if !strings.Contains(view, "also cover the 503 path") || !strings.Contains(view, "then run the linter") {
		t.Fatalf("queued prompts should be shown:\n%s", view)
	}
	if m.anyDone {
		t.Fatal("a queued prompt must not be committed to the transcript")
	}

	// One per Idle, in order.
	send(m, Idle{})
	acts := drain(q, 1, time.Second)
	if p, ok := acts[0].(UserPrompt); !ok || p.Text != "also cover the 503 path" {
		t.Fatalf("want the first queued prompt, got %#v", acts[0])
	}
	if len(m.queued) != 1 {
		t.Fatalf("the rest should still be queued, got %v", m.queued)
	}

	send(m, Busy{}, Idle{})
	acts = drain(q, 1, time.Second)
	if p, ok := acts[0].(UserPrompt); !ok || p.Text != "then run the linter" {
		t.Fatalf("want the second queued prompt, got %#v", acts[0])
	}
	if len(m.queued) != 0 {
		t.Fatalf("queue should be empty, got %v", m.queued)
	}
}

// Slash commands are instructions to the application, not messages to the
// model: /quit has to work mid-turn.
func TestSlashCommandsAreNeverQueued(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, Busy{})
	typeText(m, "/quit")
	press(m, "enter")

	acts := drain(q, 1, time.Second)
	if c, ok := acts[0].(SlashCommand); !ok || c.Name != "quit" {
		t.Fatalf("want /quit delivered immediately, got %#v", acts)
	}
	if len(m.queued) != 0 {
		t.Fatalf("slash commands must not queue, got %v", m.queued)
	}
}

// Interrupting hands the queue back rather than firing it at an agent that was
// just told to stop — and nothing typed is lost.
func TestInterruptReturnsTheQueue(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, Busy{})
	typeText(m, "first follow-up")
	press(m, "enter")
	typeText(m, "second follow-up")
	press(m, "enter")

	press(m, "esc")
	acts := drain(q, 1, time.Second)
	if _, ok := acts[0].(Interrupt); !ok {
		t.Fatalf("want an Interrupt, got %#v", acts[0])
	}
	if len(m.queued) != 0 {
		t.Fatalf("the queue should have been handed back, got %v", m.queued)
	}
	if got := m.input.value(); got != "first follow-up\nsecond follow-up" {
		t.Fatalf("queued text should land in the input, got %q", got)
	}
	// And nothing fires when the turn finally ends.
	send(m, Idle{})
	if acts := drain(q, 1, 150*time.Millisecond); len(acts) != 0 {
		t.Fatalf("an interrupted queue must not fire later, got %#v", acts)
	}
}

// Backspace on an empty prompt takes the last queued message back for editing.
func TestBackspaceTakesBackTheLastQueuedPrompt(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, Busy{})
	typeText(m, "one")
	press(m, "enter")
	typeText(m, "two")
	press(m, "enter")

	press(m, "backspace")
	if got := m.input.value(); got != "two" {
		t.Fatalf("want the last queued prompt back, got %q", got)
	}
	if len(m.queued) != 1 || m.queued[0] != "one" {
		t.Fatalf("want one still queued, got %v", m.queued)
	}
	// With text in the box, backspace is just backspace again.
	press(m, "backspace")
	if got := m.input.value(); got != "tw" {
		t.Fatalf("want an ordinary delete, got %q", got)
	}
}

func TestDeliverImmediatelyKeepsTheOldBehaviour(t *testing.T) {
	o := testOptions(io.Discard)
	o.PromptQueue = DeliverImmediately
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	send(m, Busy{})
	typeText(m, "right now please")
	press(m, "enter")

	acts := drain(q, 1, time.Second)
	if p, ok := acts[0].(UserPrompt); !ok || p.Text != "right now please" {
		t.Fatalf("want the prompt delivered immediately, got %#v", acts)
	}
	if len(m.queued) != 0 {
		t.Fatalf("nothing should be queued, got %v", m.queued)
	}
}

// Some decisions are irreversible in a way no policy covers. They get the same
// box a proxy approval gets, and the same safe default.
func TestConfirmBox(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, Confirm{
		ID: "c1", Title: "DISCARD SANDBOX",
		Question: "Reset the sandbox for ~/src/payments?",
		Detail:   []string{"2 files have not been applied to your working directory."},
	})
	if m.confirm == nil {
		t.Fatal("the confirm box did not open")
	}
	view := m.View()
	for _, want := range []string{"DISCARD SANDBOX", "Reset the sandbox", "have not been applied", "y  yes", "n  no"} {
		if !strings.Contains(view, want) {
			t.Fatalf("confirm box is missing %q:\n%s", want, view)
		}
	}

	// Enter takes the safe answer, because that is where the cursor starts.
	press(m, "enter")
	acts := drain(q, 1, time.Second)
	d, ok := acts[0].(ConfirmDecision)
	if !ok || d.ID != "c1" || d.Decision != DecisionNo {
		t.Fatalf("want a declining answer, got %#v", acts[0])
	}
	if m.confirm != nil {
		t.Fatal("the box should have closed")
	}
}

func TestConfirmAcceptsDeliberately(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, Confirm{ID: "c1", Question: "Apply 3 files to ~/src/payments?"})
	press(m, "left", "enter") // step off "no" onto "yes"

	acts := drain(q, 1, time.Second)
	if d, ok := acts[0].(ConfirmDecision); !ok || d.Decision != DecisionYes {
		t.Fatalf("want yes, got %#v", acts[0])
	}
}

func TestConfirmEscDeclines(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, Confirm{ID: "c1", Question: "Reset?"})
	press(m, "esc")

	acts := drain(q, 1, time.Second)
	if d, ok := acts[0].(ConfirmDecision); !ok || d.Decision != DecisionNo {
		t.Fatalf("esc must decline, got %#v", acts[0])
	}
}

func TestConfirmQueuesBehindAnApproval(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, sampleApproval(), Confirm{ID: "c1", Question: "Reset?"})
	if m.confirm != nil {
		t.Fatal("the confirm should be queued behind the approval")
	}
	press(m, "r") // reject the approval
	if m.confirm == nil {
		t.Fatal("the queued confirm should have opened")
	}
	press(m, "n")

	acts := drain(q, 2, time.Second)
	if len(acts) != 2 {
		t.Fatalf("want both decisions, got %#v", acts)
	}
	if _, ok := acts[0].(ApprovalDecision); !ok {
		t.Fatalf("want the approval answered first, got %#v", acts[0])
	}
	if _, ok := acts[1].(ConfirmDecision); !ok {
		t.Fatalf("want the confirm answered second, got %#v", acts[1])
	}
}

func TestPlainRendererDeclinesConfirmations(t *testing.T) {
	var buf bytes.Buffer
	ui := New(Options{Output: &buf, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	ui.Send(Confirm{ID: "c1", Question: "Reset the sandbox?"})

	done := make(chan error, 1)
	go func() { done <- ui.Run(context.Background()) }()
	select {
	case act := <-ui.Actions():
		d, ok := act.(ConfirmDecision)
		if !ok || d.Decision != DecisionNo || d.Reason == "" {
			t.Fatalf("want a reasoned decline, got %#v", act)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no decision from the plain renderer")
	}
	ui.Quit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A degraded session says so on a line of its own above the input box, and
// keeps saying it however narrow the terminal gets.
func TestStatusAlertLeadsAndSurvives(t *testing.T) {
	for _, width := range []int{40, 70, 120} {
		m, _ := newTestModel(t, width, 30)
		send(m, Status{
			Workdir: "~/src/payments", Model: "internal/codegen-l", Tokens: 6100,
			Alert: "backend unreachable · operations blocked",
		})
		lines := strings.Split(stripANSI(m.View()), "\n")
		at := -1
		for i, l := range lines {
			if strings.Contains(l, "backend unreachable") {
				at = i
			}
		}
		if at < 0 {
			t.Fatalf("@%d: the alert was dropped:\n%s", width, strings.Join(lines, "\n"))
		}
		if !strings.Contains(lines[at], m.th.Symbols.Alert) {
			t.Fatalf("@%d: the alert should be marked: %q", width, lines[at])
		}
		if at+1 >= len(lines) || !strings.HasPrefix(lines[at+1], m.th.Symbols.BoxTL) {
			t.Fatalf("@%d: the alert should sit directly above the box:\n%s", width, strings.Join(lines, "\n"))
		}
		if visibleWidth(lines[at]) > m.frameWidth() {
			t.Fatalf("@%d: alert overflowed: %d columns", width, visibleWidth(lines[at]))
		}
	}

}

// Long, staged work reports as one live block that replaces itself, and lands
// in the transcript as a single line when it is done.
func TestProgressBlock(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, Progress{ID: "p1", Label: "building sandbox image", Step: 3, Steps: 12,
		Percent: 47, Detail: "RUN apt-get install -y build-essential"})

	view := m.View()
	for _, want := range []string{"building sandbox image", "step 3/12", "47%",
		"RUN apt-get install", m.th.Symbols.BarFull, m.th.Symbols.BarEmpty} {
		if !strings.Contains(view, want) {
			t.Fatalf("progress block is missing %q:\n%s", want, view)
		}
	}
	if len(m.pending) != 1 {
		t.Fatalf("progress should be live, not committed: %d blocks", len(m.pending))
	}

	// An update replaces, it does not accumulate.
	send(m, Progress{ID: "p1", Label: "building sandbox image", Step: 4, Steps: 12,
		Percent: 60, Detail: "RUN go mod download"})
	view = m.View()
	if strings.Contains(view, "apt-get") {
		t.Fatalf("the previous detail should have been replaced:\n%s", view)
	}
	if len(m.pending) != 1 {
		t.Fatalf("still one block, got %d", len(m.pending))
	}

	send(m, Progress{ID: "p1", Label: "build sandbox image", Done: true,
		Status: ToolOK, Meta: "12 steps · cached"})
	if len(m.pending) != 0 {
		t.Fatalf("a finished progress block should commit, %d pending", len(m.pending))
	}
}

// A bar that cannot say how far along it is should not draw one.
func TestProgressWithoutPercentDrawsNoBar(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, Progress{ID: "p1", Label: "provisioning the virtual machine"})
	if view := m.View(); strings.Contains(view, m.th.Symbols.BarEmpty) {
		t.Fatalf("no percentage, no bar:\n%s", view)
	}
}

func TestProgressAnimatesAndSurvivesIdle(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, Progress{ID: "p1", Label: "building", Percent: 10})
	if !m.animating() {
		t.Fatal("an open progress block should keep the spinner running")
	}
	send(m, Idle{})
	if len(m.pending) != 0 {
		t.Fatalf("Idle must not leave a progress block wedged, %d pending", len(m.pending))
	}
}

func TestPlainRendererLogsProgressSteps(t *testing.T) {
	var buf bytes.Buffer
	ui := New(Options{Output: &buf, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	ui.Send(
		Progress{ID: "p1", Label: "building sandbox image", Step: 1, Steps: 3, Detail: "FROM golang"},
		Progress{ID: "p1", Label: "building sandbox image", Step: 1, Steps: 3, Percent: 40, Detail: "FROM golang"},
		Progress{ID: "p1", Label: "building sandbox image", Step: 2, Steps: 3, Detail: "RUN go build"},
		Progress{ID: "p1", Label: "build sandbox image", Done: true, Status: ToolOK, Meta: "3 steps"},
		Quit{},
	)
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "step 1/3"); n != 1 {
		t.Fatalf("want one line per step, got %d for step 1:\n%s", n, out)
	}
	for _, want := range []string{"step 2/3", "RUN go build", "3 steps"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plain progress log missing %q:\n%s", want, out)
		}
	}
}

// The review step is the security control, so a one-character change has to
// look different from a reformatted line.
func TestDiffHighlightsWhatActuallyChanged(t *testing.T) {
	before := `	if resp.StatusCode == http.StatusTooManyRequests {`
	after := `	if resp.StatusCode == http.StatusServiceUnavailable {`
	b, a := changedSpan(before, after)
	if b.empty() || a.empty() {
		t.Fatal("a changed constant should produce a span")
	}
	if got := string([]rune(before)[b.start:b.end]); !strings.Contains(got, "TooManyRequests") {
		t.Fatalf("removed span should cover the change, got %q", got)
	}
	if got := string([]rune(after)[a.start:a.end]); !strings.Contains(got, "ServiceUnavailable") {
		t.Fatalf("added span should cover the change, got %q", got)
	}
	// The unchanged head is not part of it.
	if strings.Contains(string([]rune(before)[b.start:b.end]), "if resp") {
		t.Fatalf("the common prefix should not be emphasised: %q",
			string([]rune(before)[b.start:b.end]))
	}
}

func TestDiffSpanIgnoresUnrelatedLines(t *testing.T) {
	if b, a := changedSpan("package billing", "import \"net/http\""); !b.empty() || !a.empty() {
		t.Fatal("two unrelated lines share nothing worth emphasising")
	}
	if b, _ := changedSpan("same", "same"); !b.empty() {
		t.Fatal("identical lines have no changed span")
	}
}

// The emphasis is laid over the line as drawn, with its tabs expanded, so it
// has to be counted there too: counted on the raw text it stopped three
// columns short of the change for every tab of indentation.
func TestPairedSpansCountTabsAsDrawn(t *testing.T) {
	lines := []DiffLine{
		{Kind: DiffRemove, Text: "\t\treturn resp, nil // caller decides"},
		{Kind: DiffAdd, Text: "\t\treturn resp, err // caller decides"},
	}
	spans := pairedSpans(lines)
	for i, l := range lines {
		drawn := []rune(truncPlain(l.Text, 200, "…"))
		got := string(drawn[spans[i].start:spans[i].end])
		if want := []string{"nil", "err"}[i]; strings.TrimSpace(got) != want {
			t.Fatalf("line %d: span covers %q of the drawn line, want %q", i, got, want)
		}
	}
}

// Lines that only sit side by side are two lines, not one line edited, and
// emphasising what "changed" between them lights up nearly all of both.
func TestDiffSpanSkipsLinesThatAreNotEdits(t *testing.T) {
	for _, pair := range [][2]string{
		{"\t\treturn resp, nil // caller decides", "\t\tdelay := retryAfter(resp.Header)"},
		{"\tif resp.StatusCode == http.StatusTooManyRequests {", "\tif retryable(resp.StatusCode) {"},
		{"\t}", "\t\tif delay <= 0 {"},
	} {
		if b, a := changedSpan(pair[0], pair[1]); !b.empty() || !a.empty() {
			t.Errorf("%q → %q should not be emphasised as an edit", pair[0], pair[1])
		}
	}
}

// Re-indenting a line is not a change to its code.
func TestDiffSpanLeavesIndentationAlone(t *testing.T) {
	before, after := "\tx := f(a)", "\t\tx := f(b)"
	b, a := changedSpan(before, after)
	if a.empty() {
		t.Fatal("the changed argument should be emphasised")
	}
	if b.start < 1 || a.start < 2 {
		t.Fatalf("the span starts inside the indentation: %v %v", b, a)
	}
}

// Pairing is per run of removals and the additions that follow them.
func TestPairedSpansMatchesRuns(t *testing.T) {
	lines := []DiffLine{
		{Kind: DiffContext, Text: "ctx"},
		{Kind: DiffRemove, Text: "delay := backoff(attempt)"},
		{Kind: DiffRemove, Text: "return resp, nil"},
		{Kind: DiffAdd, Text: "delay := backoff(attempt * 2)"},
		{Kind: DiffAdd, Text: "return resp, err"},
		{Kind: DiffContext, Text: "ctx"},
	}
	spans := pairedSpans(lines)
	if len(spans) != len(lines) {
		t.Fatalf("want a span per line, got %d", len(spans))
	}
	if !spans[0].empty() || !spans[5].empty() {
		t.Fatal("context lines have no span")
	}
	// A pure insertion has nothing to emphasise on the line it replaced.
	if !spans[1].empty() {
		t.Fatalf("nothing was removed from line 1, got a span %v", spans[1])
	}
	if got := span(lines[3].Text, spans[3]); !strings.Contains(got, "* 2") {
		t.Fatalf("the inserted text should be emphasised, got %q", got)
	}
	if got := span(lines[2].Text, spans[2]); got != "nil" {
		t.Fatalf("want the changed word alone, got %q", got)
	}
	if got := span(lines[4].Text, spans[4]); got != "err" {
		t.Fatalf("want the changed word alone, got %q", got)
	}
}

func span(text string, s wordSpan) string {
	r := []rune(text)
	if s.empty() || s.start > len(r) {
		return ""
	}
	return string(r[s.start:min(s.end, len(r))])
}

func TestDiffFilterNarrowsTheReview(t *testing.T) {
	m, _ := newTestModel(t, 110, 40)
	send(m, DiffReview{ID: "d1", Files: []DiffFile{
		{Path: "billing/client.go", Added: 3},
		{Path: "billing/client_test.go", Added: 1},
		{Path: "ledger/posting.go", Added: 2},
	}})
	m.diff.layout(boxInnerWidth(m.frameWidth()))
	if len(m.diff.visible) != 3 {
		t.Fatalf("want all three files, got %d", len(m.diff.visible))
	}

	press(m, "/")
	for _, r := range "ledger" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.diff.layout(boxInnerWidth(m.frameWidth()))
	if len(m.diff.visible) != 1 {
		t.Fatalf("want one file after filtering, got %d", len(m.diff.visible))
	}
	view := m.View()
	if strings.Contains(view, "billing/client.go") {
		t.Fatalf("filtered-out files should be hidden:\n%s", view)
	}
	if !strings.Contains(view, "filter: ledger") {
		t.Fatalf("the filter should be shown in the title:\n%s", view)
	}
	if !strings.Contains(view, "1 of 3 files") {
		t.Fatalf("the summary should say what is hidden:\n%s", view)
	}

	// Select-all only touches what is on screen.
	press(m, "enter") // leave filter editing
	press(m, "s")
	if got := m.diff.selectedPaths(); len(got) != 1 || got[0] != "ledger/posting.go" {
		t.Fatalf("select-all must not reach hidden files, got %v", got)
	}

	// Esc clears the filter rather than closing the review.
	press(m, "/")
	press(m, "esc")
	m.diff.layout(boxInnerWidth(m.frameWidth()))
	if m.diff.filter != "" || len(m.diff.visible) != 3 {
		t.Fatalf("esc should clear the filter, got %q with %d visible", m.diff.filter, len(m.diff.visible))
	}
	if m.diff == nil {
		t.Fatal("esc in the filter must not close the review")
	}
}

func TestSyntaxHighlightingIsOffWithoutColour(t *testing.T) {
	o := testOptions(io.Discard) // colourless theme
	if hl := diffHighlighter(&o); hl != nil {
		t.Fatal("no colour, no syntax highlighting")
	}
	o.Theme = colorTheme()
	if hl := diffHighlighter(&o); hl == nil {
		t.Fatal("a colour terminal should get syntax highlighting")
	}
	o.PlainDiff = true
	if hl := diffHighlighter(&o); hl != nil {
		t.Fatal("PlainDiff should turn it off")
	}
}

func TestSyntaxHighlighterColoursCode(t *testing.T) {
	hl := newHighlighter("github-dark", "terminal256")
	if hl == nil {
		t.Fatal("expected a highlighter")
	}
	out := hl.line("billing/client.go", "func retryable(code int) bool {")
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("expected colour, got %q", out)
	}
	if stripANSI(out) != "func retryable(code int) bool {" {
		t.Fatalf("highlighting must not change the text: %q", stripANSI(out))
	}
	// Cached calls return the same thing.
	if again := hl.line("billing/client.go", "func retryable(code int) bool {"); again != out {
		t.Fatal("cache returned something different")
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// A four-hundred line paste in an eight-line box hides the developer's own
// prompt. It collapses on screen; the application still gets every byte.
func TestLargePasteCollapses(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	pasted := strings.TrimSuffix(strings.Repeat("line of pasted text\n", 12), "\n")
	typeText(m, "explain this: ")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(pasted), Paste: true})

	shown := m.input.value()
	if strings.Count(shown, "\n") != 0 {
		t.Fatalf("the box should hold one line, got %q", shown)
	}
	if !strings.Contains(shown, "12 lines") {
		t.Fatalf("want a placeholder naming the size, got %q", shown)
	}

	press(m, "enter")
	acts := drain(q, 1, time.Second)
	p, ok := acts[0].(UserPrompt)
	if !ok {
		t.Fatalf("want a prompt, got %#v", acts[0])
	}
	if !strings.Contains(p.Text, pasted) {
		t.Fatalf("the application must receive the whole paste, got %q", p.Text)
	}
	if !strings.HasPrefix(p.Text, "explain this: ") {
		t.Fatalf("the typed text should survive too, got %q", p.Text)
	}
}

func TestSmallPasteIsLeftAlone(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("one\ntwo"), Paste: true})
	if got := m.input.value(); got != "one\ntwo" {
		t.Fatalf("a small paste should go in as-is, got %q", got)
	}
}

func TestReverseHistorySearch(t *testing.T) {
	o := testOptions(io.Discard)
	o.History = []string{"run the billing tests", "fix the retry in client.go", "why is the build slow"}
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	press(m, "ctrl+r")
	if !m.input.searching {
		t.Fatal("ctrl+r should start a search")
	}
	for _, r := range "the" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.input.value(); got != "why is the build slow" {
		t.Fatalf("want the newest match, got %q", got)
	}
	press(m, "ctrl+r")
	if got := m.input.value(); got != "fix the retry in client.go" {
		t.Fatalf("ctrl+r should step to the next older match, got %q", got)
	}
	if !strings.Contains(m.View(), "search") {
		t.Fatalf("the search should be visible:\n%s", m.View())
	}

	press(m, "enter")
	if m.input.searching {
		t.Fatal("enter should end the search")
	}
	if got := m.input.value(); got != "fix the retry in client.go" {
		t.Fatalf("enter should keep the match, got %q", got)
	}

	// Esc puts back what was being typed.
	m.input.reset()
	typeText(m, "half a thought")
	press(m, "ctrl+r")
	for _, r := range "billing" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	press(m, "esc")
	if got := m.input.value(); got != "half a thought" {
		t.Fatalf("esc should restore the draft, got %q", got)
	}
}

// Agent turns run for minutes; the developer is usually somewhere else by the
// time one ends.
func TestBellRingsOnlyWhenItShould(t *testing.T) {
	cases := []struct {
		name    string
		mode    BellMode
		focused bool
		ran     time.Duration
		want    bool
	}{
		{"unfocused long turn", BellWhenUnfocused, false, time.Minute, true},
		{"focused long turn", BellWhenUnfocused, true, time.Minute, false},
		{"unfocused short turn", BellWhenUnfocused, false, time.Second, false},
		{"always, focused", BellAlways, true, time.Minute, true},
		{"never", BellNever, false, time.Minute, false},
	}
	for _, c := range cases {
		o := testOptions(io.Discard)
		o.Bell = c.mode
		q := newActionQueue(8)
		m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
		m.focused = c.focused
		m.busy = true
		m.busyStart = time.Now().Add(-c.ran)
		if got := m.shouldRing(); got != c.want {
			t.Fatalf("%s: shouldRing = %v, want %v", c.name, got, c.want)
		}
	}
}

// The bell goes straight to the terminal: a frame carrying it would usually be
// overwritten before the renderer flushed it.
func TestBellIsWrittenToTheTerminal(t *testing.T) {
	var sink bytes.Buffer
	o := testOptions(io.Discard)
	o.Output = &sink
	o.Bell = BellAlways
	o.BellAfter = time.Millisecond
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	send(m, Busy{Label: "thinking"})
	time.Sleep(2 * time.Millisecond)
	send(m, Idle{})
	if got := bytes.Count(sink.Bytes(), []byte{7}); got != 1 {
		t.Fatalf("want exactly one bell, got %d", got)
	}

	// And a short turn stays quiet.
	sink.Reset()
	o.BellAfter = time.Hour
	send(m, Busy{}, Idle{})
	if got := bytes.Count(sink.Bytes(), []byte{7}); got != 0 {
		t.Fatalf("want silence, got %d bells", got)
	}
}

// An application may label its choices with any key it likes. A box whose
// "keep it" button cannot be pressed because k also scrolls is a box that lies
// about the choices it is offering.
func TestChoiceKeysBeatNavigationKeys(t *testing.T) {
	for _, key := range []string{"j", "k", "g", "G", "q", "a"} {
		m, q := newTestModel(t, 100, 16)
		send(m, Confirm{
			ID: "c1", Question: "Reset?",
			Choices: []Choice{
				{Key: rune(key[0]), Label: "the application's key", Decision: "chosen"},
				{Key: 'z', Label: "no", Decision: DecisionNo, Cancel: true},
			},
		})
		press(m, key)
		acts := drain(q, 1, time.Second)
		if len(acts) != 1 {
			t.Fatalf("%q produced no decision", key)
		}
		if d, ok := acts[0].(ConfirmDecision); !ok || d.Decision != "chosen" {
			t.Fatalf("%q: got %#v", key, acts[0])
		}
	}
}

// And the box still scrolls, with the keys no choice has claimed.
func TestScrollingSurvivesClaimedKeys(t *testing.T) {
	m, _ := newTestModel(t, 100, 16)
	req := sampleApproval()
	req.Options = []Choice{
		{Key: 'j', Label: "yes", Decision: DecisionYes},
		{Key: 'k', Label: "no", Decision: DecisionNo, Cancel: true},
	}
	send(m, req)
	m.approval.layout(boxInnerWidth(m.frameWidth()))
	h := m.modalHeight(len(m.approval.lines), approvalChrome)
	if !m.approval.scrollable(h) {
		t.Skip("the request fits; nothing to scroll")
	}
	press(m, "down", "down")
	if m.approval == nil {
		t.Fatal("arrows must not answer the box")
	}
	if m.approval.offset != 2 {
		t.Fatalf("arrows should still scroll, offset is %d", m.approval.offset)
	}
	press(m, "ctrl+d")
	if m.approval.offset <= 2 {
		t.Fatalf("page keys should still scroll, offset is %d", m.approval.offset)
	}
}

// What john says back is the point of the session: it is marked with an arrow
// in the plain foreground, not a coloured dot that files it as one more event.
func TestAssistantMarker(t *testing.T) {
	th := colorTheme()
	md := newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard))
	b := &block{kind: kindAssistant, done: true}
	b.sb.WriteString(strings.Repeat("429 is now retryable with Retry-After honoured. ", 4))
	lines := b.render(th, md, 60, "*")

	if !strings.Contains(lines[0], th.Symbols.Assistant) {
		t.Fatalf("the answer should be marked: %q", stripANSI(lines[0]))
	}
	// Plain foreground, the same colour the text itself is drawn in.
	prefix := func(s string) string { return s[:strings.Index(s, "m")+1] }
	if got, want := prefix(lines[0]), prefix(th.Text.Render("x")); got != want {
		t.Fatalf("the marker is %q, the body text is %q", got, want)
	}
	// Continuation lines line up under the text, whatever the marker's width.
	if len(lines) < 2 {
		t.Fatal("expected a wrapped answer")
	}
	indent := visibleWidth(th.Symbols.Assistant) + 1
	for _, l := range lines[1:] {
		if got := len(l) - len(strings.TrimLeft(l, " ")); got != indent {
			t.Fatalf("continuation indent is %d, marker needs %d: %q", got, indent, l)
		}
		if visibleWidth(l) > 60 {
			t.Fatalf("a wrapped line overflowed: %q", l)
		}
	}
}

// The ASCII marker is two columns wide, which is exactly why the indent is
// computed rather than assumed.
func TestAssistantMarkerInASCII(t *testing.T) {
	th := themeFromRenderer(lipgloss.NewRenderer(io.Discard))
	th.SetASCII()
	md := newMarkdownRenderer(false, true, lipgloss.NewRenderer(io.Discard))
	b := &block{kind: kindAssistant, done: true}
	b.sb.WriteString(strings.Repeat("the retry now backs off correctly. ", 4))
	lines := b.render(th, md, 50, "*")

	if !strings.HasPrefix(lines[0], "-> ") {
		t.Fatalf("want an ASCII arrow, got %q", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "   ") || strings.HasPrefix(l, "    ") {
			t.Fatalf("continuation should be indented three columns: %q", l)
		}
		if visibleWidth(l) > 50 {
			t.Fatalf("a wrapped line overflowed: %q", l)
		}
	}
}

// Allowed requests to one host collapse into one line that counts them, until
// anything but proxy traffic is printed — a second host gets its own line and
// both keep counting. Blocked requests count on a line of their own, beneath
// the allowed count, which carries on past it: one npm install is one count,
// not two, and fifty retries of one blocked host are one line, not fifty.
func TestAllowedProxyRequestsCollapseIntoOneLinePerHost(t *testing.T) {
	m, _ := newTestModel(t, 120, 40)
	allowed := func(path string) Event {
		return ProxyDecision{Method: "GET", URL: "http://deb.debian.org" + path, Verdict: "allowed", Allowed: true}
	}
	// What the model prints into the scrollback, gathered from the commands
	// its updates return.
	var printed []string
	var run func(tea.Cmd)
	run = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		switch msg := cmd().(type) {
		case tea.BatchMsg:
			for _, c := range msg {
				run(c)
			}
		default:
			text := strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%+v", msg), "{messageBody:"), "}")
			printed = append(printed, strings.Split(ansi.Strip(text), "\n")...)
		}
	}
	for _, ev := range []Event{allowed("/a.deb"), allowed("/b.deb"),
		ProxyDecision{Method: "GET", URL: "https://pypi.org/simple/x/", Verdict: "allowed", Allowed: true},
		allowed("/c.deb"),
		ProxyDecision{Method: "GET", URL: "https://example.com/", Verdict: "unknown", Allowed: false},
		allowed("/d.deb"),
		ProxyDecision{Method: "GET", URL: "https://example.com/a", Verdict: "unknown", Allowed: false},
		ProxyDecision{Method: "GET", URL: "https://example.com/b", Verdict: "unknown", Allowed: false},
		Idle{}} {
		_, cmd := m.Update(eventMsg{ev: ev})
		run(cmd)
	}
	var proxyLines []string
	for _, l := range printed {
		if strings.Contains(l, "proxy") {
			proxyLines = append(proxyLines, strings.TrimSpace(l))
		}
	}
	want := []string{"deb.debian.org", "pypi.org/simple/x/", "example.com"}
	if len(proxyLines) != len(want) {
		t.Fatalf("proxy lines:\n%s", strings.Join(proxyLines, "\n"))
	}
	for i, w := range want {
		if !strings.Contains(proxyLines[i], w) {
			t.Fatalf("line %d = %q, want it to mention %q\n%s", i, proxyLines[i], w, strings.Join(proxyLines, "\n"))
		}
	}
	if !strings.Contains(proxyLines[0], "4 requests allowed") {
		t.Fatalf("the collapsed line = %q, want it to count all 4", proxyLines[0])
	}
	if !strings.Contains(proxyLines[2], "3 requests unknown") {
		t.Fatalf("the blocked line = %q, want it to count all 3", proxyLines[2])
	}
}

// A box that opens while the developer is typing does not take the sentence
// as its answer. The "n" of "Run `node`" used to answer "REQUEST A POLICY"
// with no. Keys that keep coming are dropped until they pause for
// typingGrace; after that, and in a box that opened while the developer was
// idle, a key answers at once.
func TestABoxThatOpensMidSentenceWaitsForTheTypingToStop(t *testing.T) {
	ask := Confirm{ID: "ask", Title: "REQUEST A POLICY", Question: "Ask for example.com?",
		Choices: []Choice{
			{Key: 'h', Label: "the whole host", Decision: "host"},
			{Key: 'n', Label: "no", Decision: "no", Cancel: true},
		}}
	clock := time.Unix(1000, 0)
	at := func(m *uiModel, d time.Duration, keys ...string) {
		for _, k := range keys {
			clock = clock.Add(d)
			press(m, k)
		}
	}

	// Typing "Ru", the box opens, typing goes on: "n", "o", "d", "e".
	m, q := newTestModel(t, 100, 30)
	m.now = func() time.Time { return clock }
	at(m, 80*time.Millisecond, "R", "u")
	clock = clock.Add(40 * time.Millisecond)
	send(m, ask)
	at(m, 80*time.Millisecond, "n", "o", "d", "e")
	if m.confirm == nil {
		t.Fatalf("the sentence answered the box: %#v", drain(q, 1, 100*time.Millisecond))
	}
	// The developer sees it, stops, and answers.
	at(m, typingGrace+50*time.Millisecond, "h")
	if acts := drain(q, 1, time.Second); len(acts) != 1 || acts[0].(ConfirmDecision).Decision != "host" {
		t.Fatalf("after the pause the answer was %#v, want host", acts)
	}

	// Idle when it opens: the first key answers it.
	m, q = newTestModel(t, 100, 30)
	m.now = func() time.Time { return clock }
	at(m, 0, "x")
	clock = clock.Add(3 * time.Second)
	send(m, ask)
	at(m, 100*time.Millisecond, "n")
	if acts := drain(q, 1, time.Second); len(acts) != 1 || acts[0].(ConfirmDecision).Decision != "no" {
		t.Fatalf("a box opened while idle did not answer at once: %#v", acts)
	}
}

// A box opened because of a key pressed in another — apply's review, then its
// "write these files?" — is not a box opened mid-sentence: it answers at once.
func TestABoxOpenedByAnsweringAnotherAnswersAtOnce(t *testing.T) {
	clock := time.Unix(1000, 0)
	m, q := newTestModel(t, 100, 30)
	m.now = func() time.Time { return clock }
	send(m, Confirm{ID: "first", Question: "Apply?"})
	clock = clock.Add(100 * time.Millisecond)
	press(m, "y")
	send(m, Confirm{ID: "second", Question: "Write 3 files?"})
	clock = clock.Add(100 * time.Millisecond)
	press(m, "y")
	if acts := drain(q, 2, time.Second); len(acts) != 2 {
		t.Fatalf("the second box did not take its answer: %#v", acts)
	}
}

// The Enter that sends a prompt ends the sentence: a box the turn opens right
// after it answers at once.
func TestABoxOpenedRightAfterSendingAnswersAtOnce(t *testing.T) {
	clock := time.Unix(1000, 0)
	m, q := newTestModel(t, 100, 30)
	m.now = func() time.Time { return clock }
	for _, k := range []string{"g", "o", "enter"} {
		clock = clock.Add(60 * time.Millisecond)
		press(m, k)
	}
	clock = clock.Add(100 * time.Millisecond)
	send(m, Confirm{ID: "ask", Question: "Allow?"})
	clock = clock.Add(100 * time.Millisecond)
	press(m, "y")
	for _, act := range drain(q, 2, time.Second) {
		if d, ok := act.(ConfirmDecision); ok && d.Decision == DecisionYes {
			return
		}
	}
	t.Fatal("a box opened right after Enter did not take its answer")
}

// A choice marked Default is where the cursor starts, so Enter picks it; Esc
// still picks the way out.
func TestEnterPicksTheDefaultChoice(t *testing.T) {
	choices := []Choice{
		{Key: 'h', Label: "the whole host", Decision: "host", Default: true},
		{Key: 'n', Label: "no", Decision: "no", Cancel: true},
	}
	for key, want := range map[string]string{"enter": "host", "esc": "no"} {
		m, q := newTestModel(t, 100, 30)
		send(m, Confirm{ID: "ask", Question: "Ask?", Choices: choices})
		press(m, key)
		acts := drain(q, 1, time.Second)
		if len(acts) != 1 || acts[0].(ConfirmDecision).Decision != want {
			t.Fatalf("%s: %#v, want %s", key, acts, want)
		}
	}
}

// The box says how full the model's context is, against how much there is —
// not a sum over a turn's calls, which swung between 65k and 130k as a turn
// made one call or two.
func TestStatusShowsContextAgainstTheWindow(t *testing.T) {
	m, _ := newTestModel(t, 120, 30)
	bottom := func() string { b := m.inputBox(); return stripANSI(b[len(b)-1]) }
	send(m, Status{Workdir: "~/src/app", Model: "Claude Haiku 4.5", Tokens: 64_700, Window: 200_000})
	if got := bottom(); !strings.Contains(got, "64.7k / 200k") {
		t.Fatalf("bottom edge = %q", got)
	}
	send(m, Status{Workdir: "~/src/app", Tokens: 6_100})
	if got := bottom(); !strings.Contains(got, "6.1k") || strings.Contains(got, "k / ") {
		t.Fatalf("with no window known: %q", got)
	}
}

// As the terminal narrows, the edges give way in order: the directory gives
// up its leading segments; the context goes to its short form, then the model
// goes. What can be done now, and the sandbox's state, are the last to go.
func TestInputBoxEdgesGiveWayInOrder(t *testing.T) {
	edges := func(width int) (string, string) {
		m, _ := newTestModel(t, width, 30)
		send(m, Status{Workdir: "~/src/acme/payments", Model: "codegen-l", Sandbox: "7 unapplied · /apply",
			Tokens: 8400, Window: 200_000}, Busy{})
		b := m.inputBox()
		return stripANSI(b[0]), stripANSI(b[len(b)-1])
	}
	top, bottom := edges(100)
	if !strings.Contains(top, "~/src/acme/payments ─") || !strings.HasSuffix(top, "7 unapplied · /apply ─╮") ||
		!strings.Contains(bottom, "esc to interrupt") || !strings.HasSuffix(bottom, "codegen-l · 8.4k / 200k ─╯") {
		t.Fatalf("wide: %q / %q", top, bottom)
	}
	top, bottom = edges(46)
	if !strings.Contains(top, "payments") || strings.Contains(top, "~/src") || !strings.Contains(top, "7 unapplied") ||
		!strings.HasSuffix(bottom, "codegen-l · 8.4k ─╯") {
		t.Fatalf("narrow: %q / %q", top, bottom)
	}
	top, bottom = edges(36)
	if !strings.Contains(top, "7 unapplied") || strings.Contains(bottom, "codegen") ||
		!strings.Contains(bottom, "esc to interrupt") || !strings.Contains(bottom, "8.4k") {
		t.Fatalf("narrower: %q / %q", top, bottom)
	}
}

// The model picker is a box like the others: keys still coming when it opens
// do not answer it, and q is a letter, not a way out. A /quit typed as it
// opened used to have its q decline the narrowing — which cannot be taken
// back — and its "uit" sent as a prompt.
func TestTheModelPickerWaitsForTheTypingToStop(t *testing.T) {
	pick := ModelChoice{ID: "narrow", Reason: "keyword seen",
		Models: []ModelOption{{ID: "m_internal", Name: "Internal"}}}
	clock := time.Unix(1000, 0)
	m, q := newTestModel(t, 100, 30)
	m.now = func() time.Time { return clock }
	for _, k := range []string{"/", "q", "u"} {
		clock = clock.Add(60 * time.Millisecond)
		press(m, k)
		if k == "/" {
			send(m, pick)
		}
	}
	if m.picker == nil {
		t.Fatalf("keys typed as the picker opened answered it: %#v", drain(q, 1, 100*time.Millisecond))
	}
	// Idle, q still does not decline.
	clock = clock.Add(typingGrace + 50*time.Millisecond)
	press(m, "q")
	if m.picker == nil {
		t.Fatalf("q declined the narrowing: %#v", drain(q, 1, 100*time.Millisecond))
	}
	clock = clock.Add(100 * time.Millisecond)
	press(m, "enter")
	if acts := drain(q, 1, time.Second); len(acts) != 1 || acts[0].(ModelDecision).ModelID != "m_internal" {
		t.Fatalf("enter chose %#v, want the internal model", acts)
	}
}

// The busy line says what is happening — "running" while a command runs —
// and changing what it says does not restart its clock. Outside a turn there
// is nothing to relabel.
func TestTheBusyLineCanSayRunningWithoutStartingOver(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, BusyLabel{Label: "running"})
	if m.busy {
		t.Fatal("a label made the idle model busy")
	}
	send(m, Busy{Label: "thinking"})
	started := m.busyStart
	send(m, BusyLabel{Label: "running"})
	if m.busyLabel != "running" {
		t.Fatalf("busy label = %q, want running", m.busyLabel)
	}
	if !m.busyStart.Equal(started) {
		t.Fatal("relabelling the busy line restarted its clock")
	}
}

// An apply line names whole files, as many as fit, and counts the rest. The
// list was one string cut where the width ran out, which left "sr…" standing
// for four files.
func TestAnApplyLineNamesWholeFiles(t *testing.T) {
	files := []string{"index.html", "package-lock.json", "package.json", "src/App.tsx",
		"src/main.tsx", "src/styles.css", "vite.config.ts"}
	m, _ := newTestModel(t, 80, 24)
	send(m, ToolStart{ID: "a", Verb: "apply", Targets: files, Meta: "7 files → project"})
	view := m.View()
	line := ""
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, "apply") {
			line = l
		}
	}
	if !strings.Contains(line, "index.html, package-lock.json") || !strings.Contains(line, " more") {
		t.Fatalf("the apply line does not name whole files and count the rest:\n%s", view)
	}
	if strings.Contains(line, "…") {
		t.Fatalf("the apply line cut a name:\n%s", view)
	}

	for _, c := range []struct {
		names []string
		width int
		want  string
	}{
		{files, 38, "index.html, package-lock.json, +5 more"},
		{files, 37, "index.html, +6 more"},
		{files[:2], 40, "index.html, package-lock.json"},
		{[]string{"a-very-long-file-name.go", "b.go"}, 20, "a-very-lon…, +1 more"},
	} {
		if got := fitNames(c.names, c.width, "…"); got != c.want {
			t.Errorf("fitNames(%d names, %d) = %q, want %q", len(c.names), c.width, got, c.want)
		}
	}
}

// A tool line's target is what the line is about. A short meta still takes
// its room from a long target, but a long meta does not get to cut the target
// down to nothing: after a routing switch the line named everything but the
// model.
func TestToolLineKeepsItsTargetBesideALongMeta(t *testing.T) {
	th := animTheme()
	long := &block{kind: kindTool, verb: "model", done: true, status: ToolOK,
		target: "internal/codegen-l", meta: strings.Repeat("switched by policy ", 5)}
	line := stripANSI(long.renderTool(th, 80, "●")[0])
	if !strings.Contains(line, "internal/codegen-l") {
		t.Fatalf("the target was cut for the meta: %q", line)
	}
	if visibleWidth(line) > 80 {
		t.Fatalf("the line is %d columns, wider than 80: %q", visibleWidth(line), line)
	}

	short := &block{kind: kindTool, verb: "run", done: true, status: ToolFail,
		target: strings.Repeat("go test ./... ", 10), meta: "exit 1"}
	if line := stripANSI(short.renderTool(th, 80, "●")[0]); !strings.HasSuffix(line, "exit 1") {
		t.Fatalf("a short meta should stay whole beside a long target: %q", line)
	}
}

// A button row wider than the box shortens its longest label rather than
// running off the edge: the button that fell off was the last one, the "no".
func TestChoiceButtonsAllFitTheBox(t *testing.T) {
	b := newChoiceBox(animTheme(), []Choice{
		{Key: 'h', Label: "the whole host", Decision: "host", Default: true},
		{Key: 'p', Label: "only /lookup/github.com/cenkalti/backoff/v4@v4.3.0", Decision: "path"},
		{Key: 'n', Label: "no", Decision: "no", Cancel: true},
	})
	const inner = 60
	row := stripANSI(b.buttonRow(inner, 10))
	if visibleWidth(row) > inner {
		t.Fatalf("the row is %d columns in a box %d wide: %q", visibleWidth(row), inner, row)
	}
	for _, want := range []string{"h  the whole host", "p  only /lookup", "n  no"} {
		if !strings.Contains(row, want) {
			t.Fatalf("%q is missing from the row: %q", want, row)
		}
	}
}
