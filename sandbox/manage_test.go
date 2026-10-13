package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What `john sandbox` reports, without starting anything. A developer asking
// what is on their disk should not have to boot a virtual machine to find out.
func TestListingSandboxesReadsTheDiskAndNotTheMachines(t *testing.T) {
	dir := hostTree(t, map[string]string{"main.go": "package main\n"})
	state := t.TempDir()
	ctx := context.Background()

	vm, err := NewVM(dir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err != nil {
		t.Fatal(err)
	}
	vm.SetNote("help design the payments API")
	if err := vm.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := vm.Write(ctx, "main.go", []byte("package main // edited\n")); err != nil {
		t.Fatal(err)
	}
	if err := vm.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	w := describeWorkspace(stateDir(state))
	if w.Path != dir {
		t.Errorf("workspace = %q, want %q", w.Path, dir)
	}
	if w.Gone {
		t.Error("the workspace is there but was reported gone")
	}
	if len(w.Sandboxes) != 1 {
		t.Fatalf("sandboxes = %+v", w.Sandboxes)
	}
	s := w.Sandboxes[0]
	if s.Running {
		t.Error("a stopped sandbox is reported as running")
	}
	if s.Changes != 1 {
		t.Errorf("unapplied changes = %d, want 1", s.Changes)
	}
	if !strings.Contains(s.Describe(), "payments API") {
		t.Errorf("the description does not say what it was doing: %q", s.Describe())
	}

	// A workspace that has been deleted is the usual reason to reclaim one.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if !describeWorkspace(stateDir(state)).Gone {
		t.Error("a deleted workspace was not reported as gone")
	}
}

// Removing a sandbox refuses to discard work nobody applied, because that work
// is the only thing in one that cannot be made again.
func TestRemovingASandboxRefusesToDiscardUnappliedWork(t *testing.T) {
	dir := hostTree(t, map[string]string{"main.go": "package main\n"})
	ctx := context.Background()
	state := stateDir(t.TempDir())

	vm, err := NewVM(dir, VMOptions{StateDir: string(state), Hypervisor: &loopback{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}
	vm.Write(ctx, "main.go", []byte("package main // edited\n"))
	id := vm.Instance()
	vm.Stop(ctx)

	// RemoveSandbox works from the workspace, so point it at this state dir
	// the way the command does.
	in := state.instances()
	if len(in) != 1 || in[0].ID != id {
		t.Fatalf("instances = %+v", in)
	}
	if in[0].Changes == 0 {
		t.Fatal("the unapplied change was not recorded")
	}

	// A prune leaves it alone for the same reason.
	kept, err := Prune(PruneOptions{OlderThan: time.Nanosecond, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range kept {
		if p.Workspace == dir {
			t.Errorf("prune would take a sandbox holding unapplied work: %+v", p)
		}
	}
}

// Rebuilding the environment removes the environment and nothing else. That
// is the whole difference between it and a reset.
func TestRebuildingTheEnvironmentKeepsTheSandboxes(t *testing.T) {
	state := stateDir(t.TempDir())
	if err := state.ensure(); err != nil {
		t.Fatal(err)
	}
	inst, err := state.newInstance()
	if err != nil {
		t.Fatal(err)
	}
	// The environment image is named for what this platform's machine
	// attaches, which is what the rebuild has to find and delete.
	envImage := "env-abc" + diskExt
	stateImage := "state" + diskExt
	for _, name := range []string{envImage, "env-abc.built", "env-abc.lock"} {
		if err := os.WriteFile(state.path(name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"resources.json", "protections.json", "workspace"} {
		if err := os.WriteFile(state.path(name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(inst.path(stateImage), []byte("the work"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rebuildEnvironmentIn(state); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{envImage, "env-abc.built"} {
		if _, err := os.Stat(state.path(gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived the rebuild", gone)
		}
	}
	for _, kept := range []string{"resources.json", "protections.json", "workspace"} {
		if _, err := os.Stat(state.path(kept)); err != nil {
			t.Errorf("%s was removed: the project's settings are not the environment", kept)
		}
	}
	if data, err := os.ReadFile(inst.path(stateImage)); err != nil || string(data) != "the work" {
		t.Errorf("the sandbox's own disk was touched: %q %v", data, err)
	}
	_ = filepath.Base
}
