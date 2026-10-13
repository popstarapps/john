package terminalui

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The edges: the keys and event sequences that a session reaches eventually,
// and that nothing else in the suite goes near.

func TestDiffFileJumping(t *testing.T) {
	m, _ := newTestModel(t, 110, 40)
	send(m, DiffReview{ID: "d1", Files: []DiffFile{
		{Path: "a.go", Added: 1, Hunks: []DiffHunk{{Header: "@@", Lines: []DiffLine{{Text: "x"}}}}},
		{Path: "b.go", Added: 1, Hunks: []DiffHunk{{Header: "@@", Lines: []DiffLine{{Text: "y"}}}}},
		{Path: "c.go", Added: 1},
	}})
	m.diff.layout(boxInnerWidth(m.frameWidth()))

	press(m, "]")
	if got := m.diff.currentFile(); got != 1 {
		t.Fatalf("] should jump to the next file, got %d", got)
	}
	press(m, "]")
	if got := m.diff.currentFile(); got != 2 {
		t.Fatalf("] again: got %d", got)
	}
	press(m, "]") // past the end: stays put rather than wrapping
	if got := m.diff.currentFile(); got != 2 {
		t.Fatalf("] at the last file should stay, got %d", got)
	}
	press(m, "[", "[")
	if got := m.diff.currentFile(); got != 0 {
		t.Fatalf("[ should walk back, got %d", got)
	}

	// Expanded files do not change which file the jump lands on.
	press(m, "enter") // expand a.go
	press(m, "]")
	if got := m.diff.currentFile(); got != 1 {
		t.Fatalf("jumping over an expanded file: got %d", got)
	}
}

// Jumping walks the files on screen, not the ones a filter is hiding.
func TestDiffFileJumpingRespectsTheFilter(t *testing.T) {
	m, _ := newTestModel(t, 110, 40)
	send(m, DiffReview{ID: "d1", Files: []DiffFile{
		{Path: "billing/a.go"}, {Path: "ledger/b.go"}, {Path: "billing/c.go"},
	}})
	press(m, "/")
	for _, r := range "billing" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	press(m, "enter")
	m.diff.layout(boxInnerWidth(m.frameWidth()))

	press(m, "]")
	if got := m.diff.currentFile(); got != 2 {
		t.Fatalf("] should skip the hidden file and land on billing/c.go, got %d", got)
	}
}

func TestDiffNavigationWithNoMatches(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, DiffReview{ID: "d1", Files: []DiffFile{{Path: "a.go"}}})
	press(m, "/")
	for _, r := range "zzz" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	press(m, "enter")
	// Every navigation key on an empty list must be inert, not fatal.
	press(m, "j", "k", "]", "[", " ", "s", "enter", "g", "G", "ctrl+d", "ctrl+u")
	view := m.View()
	if !strings.Contains(view, "no files match") {
		t.Fatalf("an empty result should say so:\n%s", view)
	}
	if m.diff == nil {
		t.Fatal("navigation must not close the review")
	}
}

func TestCompletionPopupPages(t *testing.T) {
	m, q := newTestModel(t, 100, 40)
	typeText(m, "@x")
	acts := drain(q, 2, 500*time.Millisecond)
	req := acts[len(acts)-1].(CompletionRequest)

	items := make([]Completion, 20)
	for i := range items {
		items[i] = Completion{Value: "file" + itoa(i) + ".go", Detail: "file"}
	}
	send(m, Completions{Token: req.Token, Items: items})

	// The popup shows a window, not twenty rows.
	popup := m.input.popupView(m.frameWidth())
	if len(popup) > 9 {
		t.Fatalf("the popup should cap its height, got %d rows", len(popup))
	}
	if !strings.Contains(strings.Join(popup, "\n"), "/20") {
		t.Fatalf("a windowed popup should say where you are:\n%s", strings.Join(popup, "\n"))
	}

	// Walking past the end wraps, and the window follows.
	for i := 0; i < 25; i++ {
		press(m, "down")
	}
	if m.input.pop.sel >= len(items) {
		t.Fatalf("selection ran off the end: %d", m.input.pop.sel)
	}
	popup = m.input.popupView(m.frameWidth())
	if len(popup) > 9 {
		t.Fatalf("popup grew while scrolling: %d rows", len(popup))
	}
}

// Completion replaces the token under the cursor, not the tail of the line.
func TestCompletionAcceptsMidLine(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	typeText(m, "compare @bil with the other one")
	// Put the cursor back at the end of "@bil".
	for i := 0; i < len(" with the other one"); i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	}
	// Retyping the trigger context refreshes the request.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	acts := drain(q, 6, 500*time.Millisecond)
	var req CompletionRequest
	for _, a := range acts {
		if r, ok := a.(CompletionRequest); ok {
			req = r
		}
	}
	send(m, Completions{Token: req.Token, Items: []Completion{{Value: "billing/client.go"}}})
	press(m, "tab")

	got := m.input.value()
	if !strings.Contains(got, "@billing/client.go") {
		t.Fatalf("the mention should be completed in place: %q", got)
	}
	if !strings.HasSuffix(got, "with the other one") {
		t.Fatalf("the rest of the line should survive: %q", got)
	}
}

func TestApplicationCanExtendSlashCompletion(t *testing.T) {
	o := testOptions(io.Discard)
	o.CompleteSlashCommands = true
	q := newActionQueue(16)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	typeText(m, "/d")
	acts := drain(q, 2, 500*time.Millisecond)
	var req CompletionRequest
	for _, a := range acts {
		if r, ok := a.(CompletionRequest); ok {
			req = r
		}
	}
	if req.Kind != CompletionSlash {
		t.Fatalf("want a slash completion request, got %#v", acts)
	}
	send(m, Completions{Token: req.Token, Items: []Completion{
		{Value: "/deploy", Display: "/deploy", Detail: "from the application"},
	}})

	var labels []string
	for _, it := range m.input.pop.items {
		labels = append(labels, it.Value)
	}
	joined := strings.Join(labels, " ")
	if !strings.Contains(joined, "/diff") {
		t.Fatalf("the configured commands should still be offered: %v", labels)
	}
	if !strings.Contains(joined, "/deploy") {
		t.Fatalf("the application's commands should be merged in: %v", labels)
	}
}

func TestHistoryLimitAndDuplicates(t *testing.T) {
	o := testOptions(io.Discard)
	o.HistoryLimit = 3
	o.History = []string{"one", "two", "three", "four", "five"} // over the limit
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	if got := len(m.input.history); got != 3 {
		t.Fatalf("seeded history should be trimmed to the limit, got %d", got)
	}
	typeText(m, "six")
	press(m, "enter")
	if got := len(m.input.history); got != 3 {
		t.Fatalf("history should stay at the limit, got %d: %v", got, m.input.history)
	}
	if m.input.history[len(m.input.history)-1] != "six" {
		t.Fatalf("newest entry missing: %v", m.input.history)
	}

	// The same prompt twice in a row is one entry.
	typeText(m, "six")
	press(m, "enter")
	if n := len(m.input.history); n != 3 || m.input.history[2] != "six" {
		t.Fatalf("a repeated prompt should not stack: %v", m.input.history)
	}
}

func TestCancellingTheModalOnScreenOpensTheNext(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m,
		ApprovalRequest{ID: "r1", Method: "GET", URL: "https://a/1"},
		Confirm{ID: "c1", Question: "Reset?"},
		DiffReview{ID: "d1", Files: []DiffFile{{Path: "a.go"}}},
	)
	if m.approval == nil {
		t.Fatal("the first request should be on screen")
	}
	// The application withdraws the one being shown.
	send(m, CancelApproval{ID: "r1", Reason: "timed out"})
	if m.approval != nil || m.confirm == nil {
		t.Fatal("withdrawing the visible request should open the next one")
	}
	// And one that is still queued.
	send(m, CancelDiffReview{ID: "d1"})
	press(m, "n")
	if m.confirm != nil || m.diff != nil {
		t.Fatal("everything should be closed")
	}
	acts := drain(q, 2, 300*time.Millisecond)
	if len(acts) != 1 {
		t.Fatalf("only the answered box reports a decision, got %#v", acts)
	}
	if _, ok := acts[0].(ConfirmDecision); !ok {
		t.Fatalf("want the confirm's decision, got %#v", acts[0])
	}
}

// A buggy application must not be able to crash the terminal.
func TestUnknownAndRepeatedIDsAreHarmless(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m,
		ToolDetail{ID: "nope", Text: "detail"},
		ToolEnd{ID: "nope", Status: ToolOK},
		AssistantDelta{ID: "nope", Text: "text"},
		AssistantEnd{ID: "nope"},
		CancelApproval{ID: "nope"},
		CancelConfirm{ID: "nope"},
		CancelDiffReview{ID: "nope"},
		Completions{Token: 99, Items: []Completion{{Value: "x"}}},
	)
	send(m,
		ToolStart{ID: "t", Verb: "run", Target: "go test"},
		ToolEnd{ID: "t", Status: ToolOK},
		ToolEnd{ID: "t", Status: ToolFail}, // twice
		AssistantStart{ID: "a"},
		AssistantEnd{ID: "a"},
		AssistantEnd{ID: "a"}, // twice
		Notice{Text: "still standing"},
	)
	if len(m.pending) != 0 {
		t.Fatalf("everything should have committed, %d pending", len(m.pending))
	}
	_ = m.View()
}

func TestProgressAndToolsCommitInStartOrder(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m,
		Progress{ID: "p", Label: "building", Percent: 10},
		ToolStart{ID: "t", Verb: "read", Target: "go.mod"},
		ToolEnd{ID: "t", Status: ToolOK}, // finishes first
	)
	if len(m.pending) != 2 {
		t.Fatalf("the finished tool waits behind the progress block, got %d", len(m.pending))
	}
	if m.pending[0].kind != kindProgress {
		t.Fatal("order should follow when work started, not when it ended")
	}
	send(m, Progress{ID: "p", Label: "built", Done: true, Status: ToolOK})
	if len(m.pending) != 0 {
		t.Fatalf("both should have committed, %d pending", len(m.pending))
	}
}

func TestUntrustedTextIsSanitizedEverywhere(t *testing.T) {
	const evil = "evil\x1b[2J\x1b[Hcleared"
	m, _ := newTestModel(t, 100, 40)

	send(m, ApprovalRequest{
		ID: "r1", Method: "GET", URL: "https://x/" + evil,
		Headers: []Header{{Name: "x-evil", Value: evil}},
		Body:    evil,
		Policy:  evil,
	})
	if v := m.View(); strings.Contains(v, "\x1b[2J") {
		t.Fatalf("an escape survived into the approval box:\n%q", v)
	}
	press(m, "r")

	send(m, DiffReview{ID: "d1", Files: []DiffFile{{
		Path: "a" + evil + ".go", Added: 1, Selected: true,
		Hunks: []DiffHunk{{Header: "@@" + evil, Lines: []DiffLine{{Kind: DiffAdd, Text: evil}}}},
	}}})
	if v := m.View(); strings.Contains(v, "\x1b[2J") {
		t.Fatalf("an escape survived into the diff viewer:\n%q", v)
	}
	press(m, "q")

	send(m, Progress{ID: "p", Label: evil, Detail: evil, Percent: 10})
	if v := m.View(); strings.Contains(v, "\x1b[2J") {
		t.Fatalf("an escape survived into the progress block:\n%q", v)
	}
	send(m, Progress{ID: "p", Label: evil, Done: true, Status: ToolOK})

	// The status line is state the application sets, but the application is
	// repeating what a sandbox told it.
	send(m, Status{Workdir: evil, Model: evil, Sandbox: evil, Extra: evil, Alert: evil})
	if v := m.View(); strings.Contains(v, "\x1b[2J") {
		t.Fatalf("an escape survived into the status line:\n%q", v)
	}

	send(m, ModelChoice{
		ID: "mc1", Title: evil, Detail: evil, Reason: evil, Policy: evil,
		Models: []ModelOption{{ID: "m1", Name: evil, Detail: evil}},
	})
	if v := m.View(); strings.Contains(v, "\x1b[2J") {
		t.Fatalf("an escape survived into the model picker:\n%q", v)
	}
	press(m, "q")

	send(m, Confirm{ID: "c1", Title: evil, Question: evil, Detail: []string{evil}})
	if v := m.View(); strings.Contains(v, "\x1b[2J") {
		t.Fatalf("an escape survived into the confirm box:\n%q", v)
	}
	press(m, "n")

	send(m, Notice{Text: evil}, AssistantMessage{Text: evil},
		ToolStart{ID: "t2", Verb: evil, Target: evil},
		ToolDetail{ID: "t2", Text: evil},
		ToolEnd{ID: "t2", Status: ToolOK, Meta: evil})
	if v := m.View(); strings.Contains(v, "\x1b[2J") {
		t.Fatalf("an escape survived into the transcript:\n%q", v)
	}
}

// Double-width text must not push a box out of shape.
func TestWideRunesKeepBoxesRectangular(t *testing.T) {
	m, _ := newTestModel(t, 90, 30)
	send(m, Confirm{
		ID: "c1", Title: "確認", Question: "変更を適用しますか？ 🚀",
		Detail: []string{"変更されたファイル: billing/client.go 🎉"},
		Choices: []Choice{
			{Key: 'y', Label: "はい", Decision: DecisionYes},
			{Key: 'n', Label: "いいえ", Decision: DecisionNo, Cancel: true},
		},
	})
	var widths []int
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.ContainsAny(l, m.th.Symbols.BoxV+m.th.Symbols.BoxTL+m.th.Symbols.BoxLT+m.th.Symbols.BoxBL) {
			widths = append(widths, visibleWidth(l))
		}
	}
	if len(widths) < 4 {
		t.Fatalf("expected a box, found %d border rows:\n%s", len(widths), m.View())
	}
	for _, w := range widths {
		if w != widths[0] {
			t.Fatalf("wide runes bent the box: %v\n%s", widths, m.View())
		}
	}
}

func TestWideRunesInTheTranscript(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m,
		ToolStart{ID: "t", Verb: "read", Target: strings.Repeat("変更されたファイル/", 6)},
		ToolDetail{ID: "t", Text: strings.Repeat("🚀", 40)},
		ToolEnd{ID: "t", Status: ToolOK, Meta: "完了"},
	)
	for _, l := range strings.Split(m.View(), "\n") {
		if visibleWidth(l) > m.frameWidth() {
			t.Fatalf("a line is %d columns, frame is %d: %q", visibleWidth(l), m.frameWidth(), l)
		}
	}
}

func TestScrollbarStaysInBounds(t *testing.T) {
	th := themeFromRenderer(lipgloss.NewRenderer(io.Discard))
	if th.scrollbar(0, 10, 10) != nil {
		t.Fatal("no bar when everything fits")
	}
	if th.scrollbar(0, 0, 10) != nil {
		t.Fatal("no bar with no height")
	}
	for _, total := range []int{11, 40, 1000} {
		for _, height := range []int{1, 5, 20} {
			if height >= total {
				continue
			}
			for _, offset := range []int{0, 1, total / 2, total - height, total} {
				bar := th.scrollbar(offset, height, total)
				if len(bar) != height {
					t.Fatalf("bar is %d rows, want %d", len(bar), height)
				}
				thumb := 0
				for _, cell := range bar {
					if strings.Contains(cell, th.Symbols.Thumb) {
						thumb++
					}
				}
				if thumb == 0 {
					t.Fatalf("total=%d height=%d offset=%d: the thumb disappeared", total, height, offset)
				}
				if thumb > height {
					t.Fatalf("the thumb is longer than the bar")
				}
			}
		}
	}
}

func TestProgressBarClampsPercent(t *testing.T) {
	th := themeFromRenderer(lipgloss.NewRenderer(io.Discard))
	for _, p := range []int{-50, 0, 1, 50, 100, 250} {
		bar := th.progressBar(p, 10)
		if got := visibleWidth(bar); got != 10 {
			t.Fatalf("percent %d: bar is %d columns, want 10", p, got)
		}
	}
	if got := visibleWidth(th.progressBar(50, 1)); got < 4 {
		t.Fatalf("a silly width should still produce a bar, got %d", got)
	}
}

func TestRingWithoutATerminalIsSilent(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	m.sink = nil
	m.ring() // must not panic
}

func TestLiveRegionOverflowShowsThatItIsTruncated(t *testing.T) {
	o := testOptions(io.Discard)
	o.MaxLiveLines = 4
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	for i := 0; i < 8; i++ {
		send(m, ToolStart{ID: itoa(i), Verb: "run", Target: "job " + itoa(i)})
	}
	lines := m.liveLines(o.MaxLiveLines)
	if len(lines) > o.MaxLiveLines {
		t.Fatalf("the live region should be capped, got %d lines", len(lines))
	}
	if !strings.Contains(lines[0], m.th.Symbols.Ellipsis) {
		t.Fatalf("a truncated live region should say so: %q", lines[0])
	}
	if !strings.Contains(strings.Join(lines, "\n"), "job 7") {
		t.Fatalf("the newest work should survive the cap:\n%s", strings.Join(lines, "\n"))
	}
}

func TestFocusTracking(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	if !m.focused {
		t.Fatal("a session starts focused")
	}
	m.Update(tea.BlurMsg{})
	if m.focused {
		t.Fatal("blur should be noticed")
	}
	m.Update(tea.FocusMsg{})
	if !m.focused {
		t.Fatal("focus should be noticed")
	}
}

func TestInputGrowsAndStopsGrowing(t *testing.T) {
	m, _ := newTestModel(t, 100, 40)
	for i := 0; i < 20; i++ {
		typeText(m, "line "+itoa(i))
		m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true}) // alt+enter: newline
	}
	rows := strings.Count(m.input.view(), "\n") + 1
	if rows > inputMaxHeight {
		t.Fatalf("the input box grew to %d rows, cap is %d", rows, inputMaxHeight)
	}
	if !strings.Contains(m.input.value(), "\n") {
		t.Fatal("alt+enter should insert a newline rather than submitting")
	}
}

func TestReadlineEditingKeys(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	typeText(m, "delete this word")
	press(m, "ctrl+w")
	if got := m.input.value(); got != "delete this " {
		t.Fatalf("ctrl+w should delete a word, got %q", got)
	}
	press(m, "ctrl+u")
	if got := m.input.value(); got != "" {
		t.Fatalf("ctrl+u should clear to the start, got %q", got)
	}
}

func TestModalHeightNeverCollapses(t *testing.T) {
	for _, height := range []int{4, 6, 10, 24, 60} {
		m, _ := newTestModel(t, 100, height)
		if got := m.modalHeight(100, approvalChrome); got < 3 {
			t.Fatalf("@height %d the modal body collapsed to %d rows", height, got)
		}
	}
}

func TestApprovalWithoutHeadersOrBody(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, ApprovalRequest{ID: "r1", Method: "GET", URL: "https://example.com/x"})
	view := m.View()
	if !strings.Contains(view, "APPROVAL REQUIRED") || !strings.Contains(view, "example.com") {
		t.Fatalf("a bare request still gets a box:\n%s", view)
	}
	press(m, "r")
	if m.approval != nil {
		t.Fatal("it still answers")
	}
}

func TestApprovalMarksATruncatedBody(t *testing.T) {
	m, _ := newTestModel(t, 100, 40)
	send(m, ApprovalRequest{
		ID: "r1", Method: "POST", URL: "https://x/y",
		Body: strings.Repeat("a", 200), BodyTruncated: true,
	})
	view := m.View()
	if !strings.Contains(view, "first 200 bytes") {
		t.Fatalf("a truncated body should say so:\n%s", view)
	}
	if !strings.Contains(view, "truncated") {
		t.Fatalf("and say it again at the end:\n%s", view)
	}
}

func TestApplicationSuppliedChoices(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, ApprovalRequest{
		ID: "r1", Method: "POST", URL: "https://x/y",
		Options: []Choice{
			{Key: '1', Label: "allow for this session", Decision: "session"},
			{Key: '2', Label: "block", Decision: "block", Danger: true, Cancel: true},
		},
	})
	view := m.View()
	if !strings.Contains(view, "allow for this session") || strings.Contains(view, "allow once") {
		t.Fatalf("the application's choices should replace the defaults:\n%s", view)
	}
	press(m, "1")
	acts := drain(q, 1, time.Second)
	if d, ok := acts[0].(ApprovalDecision); !ok || d.Decision != "session" {
		t.Fatalf("want the application's decision string, got %#v", acts[0])
	}
}

func TestMarkdownRendering(t *testing.T) {
	m, _ := newTestModel(t, 90, 30)
	md := newMarkdownRenderer(true, false, lipgloss.NewRenderer(io.Discard))
	b := &block{kind: kindAssistant, done: true}
	b.sb.WriteString("# Heading\n\n- one\n- two\n\n```go\nx := 1\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n")
	lines := b.render(m.th, md, 88, "*")
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"Heading", "one", "two", "x := 1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("markdown lost %q:\n%s", want, joined)
		}
	}
	for _, l := range lines {
		if visibleWidth(l) > 88 {
			t.Fatalf("markdown overflowed: %q", l)
		}
	}
}

func TestMalformedMarkdownStillRenders(t *testing.T) {
	m, _ := newTestModel(t, 90, 30)
	md := newMarkdownRenderer(true, false, lipgloss.NewRenderer(io.Discard))
	b := &block{kind: kindAssistant, done: true}
	b.sb.WriteString("an unclosed ```fence and *emphasis and | a broken | table")
	if got := b.render(m.th, md, 88, "*"); len(got) == 0 {
		t.Fatal("a malformed message must never cost the developer the message")
	}
}

func TestPlainAssistantTextSkipsMarkdown(t *testing.T) {
	md := newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard))
	b := &block{kind: kindAssistant, done: true}
	b.sb.WriteString("**not bold** and `not code`")
	got := strings.Join(b.render(themeFromRenderer(lipgloss.NewRenderer(io.Discard)), md, 88, "*"), "\n")
	if !strings.Contains(got, "**not bold**") {
		t.Fatalf("with markdown off the text is passed through: %q", got)
	}
}

func TestStreamingMessageIsLiveThenCommitted(t *testing.T) {
	m, _ := newTestModel(t, 90, 30)
	send(m, AssistantStart{ID: "m"})
	send(m, AssistantDelta{ID: "m", Text: "the retry "})
	if !strings.Contains(m.View(), "the retry") {
		t.Fatalf("a streaming message should be visible while it streams:\n%s", m.View())
	}
	if len(m.pending) != 1 {
		t.Fatal("and should not be committed yet")
	}
	send(m, AssistantDelta{ID: "m", Text: "now backs off"}, AssistantEnd{ID: "m"})
	if len(m.pending) != 0 {
		t.Fatal("ending the message should commit it")
	}
}

func TestQuitDropsWhatWasQueued(t *testing.T) {
	m, q := newTestModel(t, 100, 30)
	send(m, Busy{Label: "thinking"})
	typeText(m, "never sent")
	press(m, "enter")
	send(m, Quit{})
	if !m.quitting {
		t.Fatal("Quit should stop the session")
	}
	if acts := drain(q, 1, 150*time.Millisecond); len(acts) != 0 {
		t.Fatalf("a queued prompt must not fire on the way out: %#v", acts)
	}
}
