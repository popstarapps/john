package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A sandbox is named for the directory it backs, so ~/.john/sandboxes can be
// read, audited and reclaimed by a person rather than only by john.
func TestASandboxIsNamedAfterItsWorkspace(t *testing.T) {
	for _, c := range []struct{ dir, prefix string }{
		{"/Users/bob/Projects/billing-api", "billing-api-"},
		{"/Users/bob/work/john", "john-"},
		{"/Users/bob/My Project (v2)", "my-project-v2-"},
		{"/", "workspace"},
	} {
		got := WorkspaceKey(c.dir)
		if !strings.HasPrefix(got, c.prefix) {
			t.Errorf("WorkspaceKey(%q) = %q, want it to start with %q", c.dir, got, c.prefix)
		}
		// And it stays safe as a filename, a container name and an image tag.
		for _, r := range got {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				t.Errorf("WorkspaceKey(%q) = %q contains %q", c.dir, got, r)
			}
		}
	}
}

// Two names for one directory must be one sandbox. Two would mean two
// environments and two sets of unapplied work, and `john apply` run from one
// of them could not see what the other held.
func TestOneDirectoryIsOneSandboxHoweverItIsSpelled(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if WorkspaceKey(real) != WorkspaceKey(link) {
		t.Errorf("a symbolic link to a workspace got its own sandbox: %s vs %s",
			WorkspaceKey(real), WorkspaceKey(link))
	}

	// On a case-insensitive filesystem — the default on macOS and Windows —
	// the same directory spelled two ways is still one directory.
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		if WorkspaceKey("/Users/bob/Projects/John") != WorkspaceKey("/Users/bob/projects/john") {
			t.Error("a case-different spelling got its own sandbox")
		}
	}
}

// Different projects that happen to share a directory name are different
// sandboxes, which is what the hash is for.
func TestTwoProjectsWithTheSameNameAreTwoSandboxes(t *testing.T) {
	a := WorkspaceKey("/Users/bob/work/api")
	b := WorkspaceKey("/Users/bob/archive/api")
	if a == b {
		t.Fatalf("two projects share a sandbox: %s", a)
	}
	if !strings.HasPrefix(a, "api-") || !strings.HasPrefix(b, "api-") {
		t.Fatalf("names lost the project: %s %s", a, b)
	}
}

// Two terminals in one workspace get two sandboxes, and they do not see each
// other's work.
//
// This is the case the whole arrangement exists for: two agents on one
// repository doing different things. What they share is the project — the
// environment, the limits, the protections. What they must not share is the
// upper layer, because that is each one's own unapplied work.
func TestTwoSessionsInOneWorkspaceGetTheirOwnWork(t *testing.T) {
	dir := hostTree(t, map[string]string{"main.go": "package main\n"})
	state := t.TempDir()
	ctx := context.Background()

	boot := func(want string) *VM {
		vm, err := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}, Instance: want})
		if err != nil {
			t.Fatal(err)
		}
		if err := vm.Build(ctx, nil); err != nil {
			t.Fatal(err)
		}
		return vm
	}

	first := boot("")
	t.Cleanup(func() { first.Stop(context.Background()) })
	if err := first.Write(ctx, "main.go", []byte("package main // first\n")); err != nil {
		t.Fatal(err)
	}

	// The second is not refused: the first is busy, so this session gets one
	// of its own rather than being told to wait for a terminal it cannot see.
	second := boot(newInstanceName)
	t.Cleanup(func() { second.Stop(context.Background()) })
	if first.Instance() == second.Instance() {
		t.Fatalf("both sessions took the same sandbox (%s)", first.Instance())
	}

	// And the work is separate on both sides.
	got, err := second.Read(ctx, "main.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != "package main\n" {
		t.Fatalf("the second session sees the first's unapplied work: %q", got.Data)
	}
	if diffs, err := second.Diff(ctx); err != nil || len(diffs) != 0 {
		t.Fatalf("the second session starts with changes: %+v %v", diffs, err)
	}
	if diffs, err := first.Diff(ctx); err != nil || len(diffs) != 1 {
		t.Fatalf("the first session lost its change: %+v %v", diffs, err)
	}
}

// Closing the terminal and opening it again picks up where the last session
// left off, without asking. One sandbox waiting is not a choice.
func TestReopeningATerminalCarriesOnInTheSameSandbox(t *testing.T) {
	dir := hostTree(t, map[string]string{"main.go": "package main\n"})
	state := t.TempDir()
	ctx := context.Background()

	first, err := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := first.Write(ctx, "main.go", []byte("package main // yesterday\n")); err != nil {
		t.Fatal(err)
	}
	was := first.Instance()
	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	next, err := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Stop(context.Background()) })
	if err := next.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if next.Instance() != was {
		t.Fatalf("a new session started a new sandbox (%s) instead of carrying on with %s",
			next.Instance(), was)
	}
	if diffs, err := next.Diff(ctx); err != nil || len(diffs) != 1 {
		t.Fatalf("yesterday's unapplied work is gone: %+v %v", diffs, err)
	}
}

// Coming back to more than one is the only case that asks, because it is the
// only one where guessing strands somebody's work.
func TestComingBackToTwoSandboxesAsksWhichOne(t *testing.T) {
	dir := hostTree(t, map[string]string{"main.go": "package main\n"})
	state := t.TempDir()
	ctx := context.Background()

	// Two sessions at once, then both closed: two sandboxes, nobody in them.
	// The second says it wants one of its own, which is what the developer
	// would have answered the dialog with.
	a, _ := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err := a.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}, Instance: newInstanceName})
	if err := b.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	idA, idB := a.Instance(), b.Instance()
	a.Stop(ctx)
	b.Stop(ctx)

	next, _ := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	err := next.Build(ctx, nil)
	var choose *errChooseInstance
	if !errors.As(err, &choose) {
		next.Stop(ctx)
		t.Fatalf("john picked one of two sandboxes by itself: %v", err)
	}
	if len(choose.Choices()) != 2 {
		t.Fatalf("offered %d sandboxes, want 2", len(choose.Choices()))
	}

	// And naming one carries it on.
	chosen, _ := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}, Instance: idB})
	t.Cleanup(func() { chosen.Stop(context.Background()) })
	if err := chosen.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if chosen.Instance() != idB {
		t.Fatalf("asked for %s, got %s (the other was %s)", idB, chosen.Instance(), idA)
	}
}

// The message a developer sees when they come back to more than one sandbox.
// It has to say which is which: the time alone identifies a sandbox, and what
// the session was doing identifies the work.
func TestChoosingBetweenSandboxesSaysWhatEachWasDoing(t *testing.T) {
	err := &errChooseInstance{
		Workdir: "/Users/bob/Projects/billing-api",
		Instances: []instance{
			{ID: "1", LastUsed: time.Now().Add(-4 * time.Minute), Note: "help design the new payments API", Changes: 3},
			{ID: "2", LastUsed: time.Now().Add(-50 * time.Hour), Note: "refactor the core interfaces so the ledger is not special-cased"},
		},
	}
	got := err.Error()
	for _, want := range []string{
		"You have 2 sandboxes",
		"Which one do you want to use?",
		`1: last used 4 minutes ago ("help design the new payments API") · 3 unapplied changes`,
		`2: last used 2 days ago ("refactor the core interfaces so the ledger is n…")`,
		"-sandbox-instance",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the message does not contain %q:\n%s", want, got)
		}
	}
}

// The one sandbox here being in use is not a reason to make a second one
// quietly. It is a reason to say so — a developer who did not know they had
// another terminal open would otherwise end up with their work split across
// two sandboxes and no hint that it happened.
func TestASandboxInUseIsSaidRatherThanWorkedAround(t *testing.T) {
	dir := hostTree(t, map[string]string{"main.go": "package main\n"})
	state := t.TempDir()
	ctx := context.Background()

	first, _ := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err := first.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Stop(context.Background()) })

	second, _ := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	err := second.Build(ctx, nil)
	var choose *errChooseInstance
	if !errors.As(err, &choose) {
		second.Stop(ctx)
		t.Fatalf("a second sandbox was made without saying so: %v", err)
	}
	if len(choose.Choices()) != 1 || !choose.Choices()[0].Busy {
		t.Fatalf("the sandbox in use was not shown: %+v", choose.Choices())
	}
	if !strings.Contains(choose.Question(), "in use") {
		t.Errorf("the question does not say why: %q", choose.Question())
	}

	// And asking for a new one gets a new one.
	second.SetInstance(newInstanceName)
	if err := second.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Stop(context.Background()) })
	if first.Instance() == second.Instance() {
		t.Fatal("both sessions took the same sandbox")
	}
}
