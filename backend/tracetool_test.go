package backend

import (
	"strings"
	"testing"

	"github.com/popstarapps/john/protocol"
)

// The trace keeps the end of what a tool printed, so a failure can be read
// from the record and not only from what the model was told; and an exit
// code only for run, the one tool that has one.
func TestATraceRecordKeepsTheEndOfTheOutput(t *testing.T) {
	long := strings.Repeat("x", traceOutputTail) + "\nEIO: i/o error, rmdir 'dist/assets'"
	rec := toolResultRecord("t1", "run", &protocol.ToolResult{Status: "failed", Exit: 1, Stderr: long, Stdout: "building…"})
	if rec["exit"] != 1 || rec["stdout"] != "building…" {
		t.Fatalf("record = %v", rec)
	}
	stderr, _ := rec["stderr"].(string)
	if !strings.HasSuffix(stderr, "rmdir 'dist/assets'") || len(stderr) > traceOutputTail+len("…") {
		t.Fatalf("stderr kept %d bytes ending %q", len(stderr), stderr[max(0, len(stderr)-40):])
	}

	edit := toolResultRecord("t2", "edit", &protocol.ToolResult{Status: "failed", Stderr: "sandbox: path escapes the workspace"})
	if _, has := edit["exit"]; has {
		t.Fatalf("an edit was given an exit code: %v", edit)
	}
	if edit["stderr"] != "sandbox: path escapes the workspace" {
		t.Fatalf("edit record = %v", edit)
	}
}
