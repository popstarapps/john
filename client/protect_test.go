//go:build !windows

package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/store"
)

// workspacePolicy writes one workspace-protection policy into a baseline
// group, which is how an administrator makes a rule that every team works
// inside. The client picks it up from the signed snapshot on the way up.
func workspacePolicy(t *testing.T, name, mode string, paths ...string) func(*store.Store) {
	t.Helper()
	return func(st *store.Store) {
		g, err := st.CreatePolicyGroup("Protected paths", "", true, "admin@example.com")
		if err != nil {
			t.Fatal(err)
		}
		spec, err := json.Marshal(policy.WorkspaceSpec{Kind: "workspace", Mode: mode, Paths: paths})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreatePolicy(g.ID, name, "", spec, "admin@example.com"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAnEditOfAReadOnlyPathIsBlocked is the whole loop for one policy: an
// administrator writes it in the backend, the client pulls it inside a signed
// snapshot, and the edit the model asks for never happens — the developer sees
// why, and so does the model.
func TestAnEditOfAReadOnlyPathIsBlocked(t *testing.T) {
	s := newE2EWith(t,
		workspacePolicy(t, "Tests are not the agent's to rewrite", policy.ModeReadOnly, "billing/client_test.go"),
		llmtest.ToolCall("edit", map[string]any{
			"path":    "billing/client_test.go",
			"content": "package billing // deleted the failing test\n",
		}),
		llmtest.Text("That file is read-only, so I left it alone."),
	)
	s.wait("workspace protection: 1 path rule ·")

	s.type_("make the billing tests pass\r")
	s.wait("That file is read-only, so I left it alone.")
	s.wait("read-only by policy")

	// Nothing was written: not to the host, and not even to the sandbox's own
	// copy-on-write layer.
	if n := s.sb.Pending(); n != 0 {
		t.Errorf("the sandbox holds %d change(s) after a blocked edit", n)
	}

	// And the model was told which rule stopped it, as an error it can reason
	// about rather than a silent no-op it will try again.
	reqs := s.fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(reqs))
	}
	blocked := false
	for _, m := range reqs[1].Messages {
		if !m.IsError {
			continue
		}
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "read-only under workspace policy") {
				blocked = true
			}
		}
	}
	if !blocked {
		t.Fatalf("the refusal never reached the model: %+v", reqs[1].Messages)
	}
}

// TestAnExcludedPathIsNotThereToRead: exclusion is not a check in front of a
// file, it is the file never being in the sandbox. The client refuses the read
// before it goes looking, so the answer is the rule rather than "no such file".
func TestAnExcludedPathIsNotThereToRead(t *testing.T) {
	s := newE2EWith(t,
		workspacePolicy(t, "Secrets stay out of the sandbox", policy.ModeExclude, ".env"),
		llmtest.ToolCall("read", map[string]any{"path": ".env"}),
		llmtest.Text("I cannot see that file."),
	)
	// A real secret sits in the working directory, so what is being asserted is
	// that it stayed there — not that there was nothing to find.
	if err := os.WriteFile(filepath.Join(s.workdir, ".env"),
		[]byte("STRIPE_KEY=sk_live_real\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s.type_("what is in the env file\r")
	s.wait("I cannot see that file.")
	s.wait("excluded by policy")

	reqs := s.fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(reqs))
	}
	for _, m := range reqs[1].Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "sk_live") {
				t.Fatal("the excluded file's content reached the model")
			}
		}
	}
}
