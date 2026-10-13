package terminalui

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The plain renderer is what `john | tee build.log` and every CI run sees. It
// has no live region to correct a mistake on the next frame: whatever it writes
// is the record.

func plainRun(t *testing.T, opts Options, events ...Event) (string, []Action) {
	t.Helper()
	var buf bytes.Buffer
	opts.Output = &buf
	opts.Input = strings.NewReader("")
	opts.Mode = ModePlain
	if opts.Theme == nil {
		opts.Theme = testTheme()
	}
	ui := New(opts)
	ui.Send(events...)
	ui.Send(Quit{})

	var acts []Action
	done := make(chan error, 1)
	go func() { done <- ui.Run(context.Background()) }()
	for a := range ui.Actions() {
		acts = append(acts, a)
	}
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	return buf.String(), acts
}

func TestPlainRendersEveryKindOfOutput(t *testing.T) {
	out, _ := plainRun(t, Options{},
		Notice{Text: "sandbox resumed"},
		Notice{Level: NoticeWarn, Text: "policy cache expiring"},
		Notice{Level: NoticeError, Text: "backend unreachable"},
		AssistantStart{ID: "m1"},
		AssistantDelta{ID: "m1", Text: "the retry now "},
		AssistantDelta{ID: "m1", Text: "backs off"},
		AssistantEnd{ID: "m1"},
		ToolStart{ID: "t1", Verb: "run", Target: "go test ./..."},
		ToolDetail{ID: "t1", Text: "--- FAIL: TestRetry"},
		ToolEnd{ID: "t1", Status: ToolFail, Meta: "FAIL"},
		ToolStart{ID: "t2", Verb: "edit", Target: "billing/client.go"},
		ToolEnd{ID: "t2", Status: ToolOK, Meta: "+34 -6"},
		ProxyDecision{Method: "GET", URL: "proxy.golang.org/list", Verdict: "always allow", Allowed: true},
		ProxyDecision{Method: "POST", URL: "telemetry.vendor.io/v1", Verdict: "blocked", Allowed: false},
		AssistantMessage{Text: "done"},
	)
	for _, want := range []string{
		"sandbox resumed", "policy cache expiring", "backend unreachable",
		"the retry now backs off", "run go test ./...", "--- FAIL: TestRetry",
		"edit billing/client.go", "+34 -6", "always allow", "blocked", "done",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plain output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("plain output must contain no escape sequences:\n%q", out)
	}
}

// State is not history: none of it belongs in a log.
func TestPlainIgnoresStateEvents(t *testing.T) {
	out, _ := plainRun(t, Options{},
		Status{Workdir: "~/src/payments", Alert: "backend unreachable"},
		Busy{Label: "thinking"},
		Thinking{Text: "a private thought"},
		Completions{Token: 1, Items: []Completion{{Value: "x.go"}}},
		CancelApproval{ID: "gone"},
		CancelConfirm{ID: "gone"},
		CancelDiffReview{ID: "gone"},
		Idle{},
	)
	for _, unwanted := range []string{"a private thought", "thinking", "x.go", "~/src/payments"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("plain output should not carry %q:\n%s", unwanted, out)
		}
	}
}

// `john diff > review.txt` has to produce the diff, not a summary of one.
func TestPlainWritesTheWholeDiff(t *testing.T) {
	out, acts := plainRun(t, Options{}, DiffReview{
		ID: "d1", Title: "apply to ~/src/payments",
		Files: []DiffFile{
			{Path: "billing/client.go", Added: 2, Removed: 1, Hunks: []DiffHunk{{
				Header: "@@ -18,6 +18,9 @@",
				Lines: []DiffLine{
					{Kind: DiffContext, Text: "resp, err := c.http.Do(req)"},
					{Kind: DiffRemove, Text: "return resp, nil"},
					{Kind: DiffAdd, Text: "return c.retry(req, resp)"},
					{Kind: DiffAdd, Text: "// retries are capped"},
				},
			}}},
			{Path: "internal/testdata/fixture.bin", Binary: true},
		},
	})
	for _, want := range []string{
		"apply to ~/src/payments",
		"billing/client.go  +2 -1",
		"@@ -18,6 +18,9 @@",
		"  resp, err := c.http.Do(req)",
		"- return resp, nil",
		"+ return c.retry(req, resp)",
		"internal/testdata/fixture.bin",
		"not applied",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plain diff missing %q:\n%s", want, out)
		}
	}
	// And it answers, so the application is not left waiting.
	if len(acts) != 1 {
		t.Fatalf("want one decision, got %#v", acts)
	}
	if d, ok := acts[0].(DiffDecision); !ok || d.Apply {
		t.Fatalf("a diff nobody can review is not applied: %#v", acts[0])
	}
}

func TestPlainLogsProgressOncePerStep(t *testing.T) {
	out, _ := plainRun(t, Options{},
		Progress{ID: "p", Label: "building", Step: 1, Steps: 3, Detail: "FROM golang"},
		Progress{ID: "p", Label: "building", Step: 1, Steps: 3, Percent: 20, Detail: "FROM golang"},
		Progress{ID: "p", Label: "building", Step: 1, Steps: 3, Percent: 90, Detail: "FROM golang"},
		Progress{ID: "p", Label: "building", Step: 2, Steps: 3, Detail: "RUN go build"},
		Progress{ID: "p", Label: "sandbox image", Done: true, Status: ToolOK, Meta: "3 steps"},
	)
	if n := strings.Count(out, "step 1/3"); n != 1 {
		t.Fatalf("a redrawn bar is not a log line, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "step 2/3") || !strings.Contains(out, "3 steps") {
		t.Fatalf("missing steps:\n%s", out)
	}
}

// Fail closed, with a reason, and never leave the application waiting.
func TestPlainAnswersEveryQuestion(t *testing.T) {
	_, acts := plainRun(t, Options{},
		ApprovalRequest{ID: "r1", Method: "POST", URL: "https://telemetry.vendor.io/v1"},
		Confirm{ID: "c1", Question: "Reset the sandbox?"},
		DiffReview{ID: "d1", Files: []DiffFile{{Path: "a.go"}}},
	)
	if len(acts) != 3 {
		t.Fatalf("want three answers, got %#v", acts)
	}
	a, ok := acts[0].(ApprovalDecision)
	if !ok || a.Decision != DecisionReject || a.Reason == "" {
		t.Fatalf("approval: %#v", acts[0])
	}
	c, ok := acts[1].(ConfirmDecision)
	if !ok || c.Decision != DecisionNo || c.Reason == "" {
		t.Fatalf("confirm: %#v", acts[1])
	}
	d, ok := acts[2].(DiffDecision)
	if !ok || d.Apply {
		t.Fatalf("diff: %#v", acts[2])
	}
}

func TestPlainCanRefuseToRunAtAll(t *testing.T) {
	cases := map[string]Event{
		"approval": ApprovalRequest{ID: "r1", Method: "POST", URL: "https://x/y"},
		"confirm":  Confirm{ID: "c1", Question: "Reset?"},
		"diff":     DiffReview{ID: "d1", Files: []DiffFile{{Path: "a.go"}}},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			ui := New(Options{
				Output: &buf, Input: strings.NewReader(""), Mode: ModePlain,
				NonInteractive: NonInteractiveError, Theme: testTheme(),
			})
			ui.Send(ev)
			err := ui.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "interactive decision required") {
				t.Fatalf("want ErrNotInteractive, got %v", err)
			}
		})
	}
}

func TestPlainSurvivesUnknownIDs(t *testing.T) {
	// A ToolEnd for a call that never started, a delta for a message that does
	// not exist: the application is buggy, but the log still has to come out.
	out, _ := plainRun(t, Options{},
		ToolDetail{ID: "nope", Text: "detail"},
		ToolEnd{ID: "nope", Status: ToolOK},
		AssistantDelta{ID: "nope", Text: "text"},
		AssistantEnd{ID: "nope"},
		Notice{Text: "still fine"},
	)
	if !strings.Contains(out, "still fine") {
		t.Fatalf("output stopped at the first bad ID:\n%s", out)
	}
}

func TestPlainSanitizesUntrustedOutput(t *testing.T) {
	out, _ := plainRun(t, Options{},
		ToolStart{ID: "t", Verb: "run", Target: "go test"},
		ToolDetail{ID: "t", Text: "evil\x1b[2J\x1b[Hcleared"},
		ToolEnd{ID: "t", Status: ToolOK},
		AssistantMessage{Text: "also \x1b[31mred\x1b[0m"},
	)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("an escape sequence survived into the log:\n%q", out)
	}
}

func TestPlainStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ui := New(Options{Output: &bytes.Buffer{}, Input: strings.NewReader(""), Mode: ModePlain, Theme: testTheme()})
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if err := ui.Run(ctx); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestPlainWidthFollowsCOLUMNS(t *testing.T) {
	t.Setenv("COLUMNS", "40")
	out, _ := plainRun(t, Options{}, Notice{Text: strings.Repeat("long ", 40)})
	for _, l := range strings.Split(out, "\n") {
		if len([]rune(l)) > 40 {
			t.Fatalf("line is %d columns, COLUMNS said 40: %q", len([]rune(l)), l)
		}
	}

	t.Setenv("COLUMNS", "not a number")
	if got := plainWidth(&bytes.Buffer{}); got != 80 {
		t.Fatalf("a nonsense COLUMNS should fall back to 80, got %d", got)
	}
}

func TestPlainNeedsNoTheme(t *testing.T) {
	// The plain renderer draws no glyphs, so it must not require the terminal
	// query that building a theme performs.
	var buf bytes.Buffer
	ui := New(Options{Output: &buf, Input: strings.NewReader(""), Mode: ModePlain})
	ui.Send(Notice{Text: "no theme needed"}, Quit{})
	if err := ui.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no theme needed") {
		t.Fatalf("got %q", buf.String())
	}
}
