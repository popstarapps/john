package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/popstarapps/john/policy"
)

// The protection tests that are about one implementation's own arrangements.
// The ones that are about the contract every implementation owes — a
// protected path refused, a protected change refused again at Apply, a
// different spelling of the same file — live in contract_test.go and run
// against all of them.

// protections builds the protections a signed snapshot would carry, so these
// tests exercise the same shape the backend sends rather than a hand-made one.
func protections(t *testing.T, byMode map[string][]string) policy.Protections {
	t.Helper()
	return testProtections(byMode)
}

func wantProtected(t *testing.T, what string, err error, mode string) {
	t.Helper()
	var pe *policy.ProtectedError
	if !errors.As(err, &pe) {
		t.Fatalf("%s: %v, want a workspace-protection refusal", what, err)
	}
	if pe.Mode != mode {
		t.Fatalf("%s: refused as %s, want %s", what, pe.Mode, mode)
	}
}

// TestDeletingAProtectedFileNeverReachesTheHost covers the other direction:
// the sandbox's copy is gone, the developer's is not.
func TestDeletingAProtectedFileNeverReachesTheHost(t *testing.T) {
	ctx := context.Background()
	dir := hostTree(t, map[string]string{".github/workflows/ci.yml": "on: push\n"})
	f := NewFake(dir)
	if err := f.Delete(ctx, ".github/workflows/ci.yml"); err != nil {
		t.Fatal(err)
	}
	if err := f.Protect(protections(t, map[string][]string{
		policy.ModeNoDelete: {".github/workflows"},
	})); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Apply(ctx, nil); err != nil || n != 0 {
		t.Fatalf("Apply = %d, %v — want 0 files and no error", n, err)
	}
	if _, ok := readHost(t, dir, ".github/workflows/ci.yml"); !ok {
		t.Fatal("the workflow was deleted from the host under a no-delete policy")
	}
}

// The gate `john apply` stands in — it runs with no backend and no snapshot,
// and must still refuse what an administrator protected — is covered by
// TestProtectionsSurviveForTheSessionlessApplyOnTheVM in vm_test.go. It was
// covered twice while there were two sandboxes.

// testProtections is the same, for the tests that have no *testing.T to hand.
func testProtections(byMode map[string][]string) policy.Protections {
	snap := &policy.Snapshot{}
	for mode, paths := range byMode {
		snap.Workspace = append(snap.Workspace, policy.WorkspacePolicy{
			ID:   "p_" + mode,
			Spec: policy.WorkspaceSpec{Kind: "workspace", Mode: mode, Paths: paths},
		})
	}
	return snap.Protections()
}
