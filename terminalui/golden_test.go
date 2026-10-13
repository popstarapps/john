package terminalui

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden frames in testdata")

// elapsed strips running clocks, which are the only part of a frame that is not
// a function of the events that produced it.
var elapsed = regexp.MustCompile(`· \d+m?\d*s`)

// TestGoldenFrames renders each state the UI can be in and compares the whole
// frame against a recorded one.
//
// The other tests assert invariants — nothing too wide, nothing missing. This
// one asserts the thing invariants cannot: that the UI still looks the way it
// was designed to look. Run `go test ./terminalui -update` to re-record, and
// read the diff before committing it.
func TestGoldenFrames(t *testing.T) {
	cases := map[string]func(*uiModel){
		"idle": func(m *uiModel) {
			send(m, Status{Workdir: "~/src/payments", Model: "internal/codegen-l",
				Sandbox: "sandbox ready"})
		},
		"working": func(m *uiModel) {
			send(m,
				Status{Workdir: "~/src/payments", Model: "internal/codegen-l",
					Sandbox: "sandbox ready", Tokens: 6100},
				ToolStart{ID: "1", Verb: "run", Target: "go test ./billing/..."},
				Busy{Label: "thinking"},
				Thinking{Text: "the 429 branch returns early instead of backing off"},
			)
		},
		"queued": func(m *uiModel) {
			send(m, Busy{Label: "thinking"})
			for _, q := range []string{"also cover the 503 path", "then run the linter"} {
				typeText(m, q)
				press(m, "enter")
			}
		},
		"degraded": func(m *uiModel) {
			send(m, Status{Workdir: "~/src/payments", Model: "internal/codegen-l",
				Alert: "backend unreachable · policy decisions blocked"})
		},
		"commands": func(m *uiModel) {
			send(m, Status{Workdir: "~/src/payments"})
			typeText(m, "/")
		},
		"progress": func(m *uiModel) {
			send(m, Progress{ID: "p1", Label: "building sandbox image",
				Step: 3, Steps: 6, Percent: 50, Detail: "RUN go mod download"})
		},
		"approval": func(m *uiModel) {
			send(m, ApprovalRequest{
				ID: "r1", Method: "POST", URL: "https://telemetry.vendor.io/v1/events",
				Headers: []Header{
					{Name: "content-type", Value: "application/json"},
					{Name: "authorization", Value: "Bearer sk-live-…redacted by policy…"},
				},
				Body:    `{"project":"payments","event":"build.finished"}`,
				Verdict: "ask developer", Policy: "policy group staging-writes",
			})
		},
		"confirm": func(m *uiModel) {
			send(m, Confirm{
				ID: "c1", Title: "DISCARD SANDBOX",
				Question: "Reset the sandbox for ~/src/payments?",
				Detail: []string{
					"2 files have not been applied to your working directory.",
					"Installed tools and caches are rebuilt on the next run.",
				},
			})
		},
		"model": func(m *uiModel) {
			send(m, ModelChoice{
				ID: "mc1", Detail: "payments-restricted",
				Reason: `keyword "C++" seen in billing/native/bridge.cc`,
				Policy: "this session continues on an internally hosted model",
				Models: []ModelOption{
					{ID: "m1", Name: "internal/codegen-l", Detail: "128k context", Internal: true},
					{ID: "m2", Name: "internal/codegen-s", Detail: "32k context, faster", Internal: true},
				},
			})
		},
		"review": func(m *uiModel) {
			send(m, DiffReview{ID: "d1", Title: "apply to ~/src/payments", Files: []DiffFile{
				{Path: "billing/client.go", Added: 3, Removed: 2, Selected: true, Hunks: []DiffHunk{{
					Header: "@@ -18,6 +18,9 @@ func (c *Client) do(req *http.Request)",
					Lines: []DiffLine{
						{Kind: DiffContext, Text: "\tresp, err := c.http.Do(req)"},
						{Kind: DiffRemove, Text: "\tif resp.StatusCode == http.StatusTooManyRequests {"},
						{Kind: DiffRemove, Text: "\t\treturn resp, nil // caller decides"},
						{Kind: DiffAdd, Text: "\tif retryable(resp.StatusCode) {"},
						{Kind: DiffAdd, Text: "\t\treturn c.retry(req, resp)"},
						{Kind: DiffContext, Text: "\t}"},
					},
				}}},
				{Path: "billing/client_test.go", Added: 21, Selected: true},
			}})
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			m, _ := newTestModel(t, 88, 26)
			setup(m)
			got := elapsed.ReplaceAllString(m.View(), "· Ns")

			path := filepath.Join("testdata", name+".frame")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run: go test ./terminalui -update)", err)
			}
			if want := strings.TrimSuffix(string(raw), "\n"); got != want {
				t.Fatalf("frame changed.\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}
