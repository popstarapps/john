package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/popstarapps/john/policy"
)

// These run the whole sandbox: the control protocol, the live lower layer over
// a real socket, the union, and the workspace actually mounted, so that `sh`
// inside the sandbox reads and writes through every layer the way a build
// would. The machine under it is the loopback one, which isolates nothing —
// what is being tested here is everything above the hypervisor, which is the
// same code whichever hypervisor is under it.

func newTestVM(t *testing.T, files map[string]string, prot policy.Protections) (*VM, string) {
	t.Helper()
	workdir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(workdir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vm, err := NewVM(workdir, VMOptions{
		StateDir:   t.TempDir(),
		Protect:    prot,
		Hypervisor: &loopback{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vm.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := vm.Build(ctx, nil); err != nil {
		t.Fatalf("build: %v", err)
	}
	return vm, workdir
}

// run executes a command in the sandbox and fails the test if it could not be
// executed at all — a non-zero exit is an answer and is returned.
//
// The timeout is generous because the same helper drives the emulated machine,
// where every instruction is interpreted and a loaded build host can stretch a
// command that takes milliseconds here into tens of seconds there. Nothing
// that passes waits for it; it is the difference between a slow machine and a
// failed test.
// needsAShell skips a test that runs commands inside the sandbox when this
// host cannot give it a workspace to run them in.
//
// That is the loopback machine on macOS or Windows, where the agent is an
// ordinary process on the developer's own operating system and mounting the
// workspace needs FUSE. Nothing is skipped anywhere it can run, and nothing
// goes uncovered: the same assertions run inside a real machine, on every
// platform, in machine_test.go.
func needsAShell(t *testing.T, vm *VM) {
	t.Helper()
	if mounted, why := vm.WorkspaceMounted(); !mounted {
		t.Skip("no shell in the sandbox here: " + why)
	}
}

// needsAMachine skips a claim about isolation when the sandbox under it is not
// a machine.
//
// That is the loopback one: the agent as an ordinary process on the
// developer's own operating system, with the developer's network interfaces
// and the developer's filesystem. It answers everything about the route to
// the proxy — the forwarder, the environment, the certificate authority — and
// it cannot answer "and there is no other way out", because there is one. The
// half it cannot answer is asserted inside every real machine.
func needsAMachine(t *testing.T, vm *VM) {
	t.Helper()
	vm.mu.Lock()
	root := ""
	if vm.mach != nil {
		root = vm.mach.Root()
	}
	vm.mu.Unlock()
	if root != "" {
		t.Skip("the sandbox here is a process on this computer, not a machine: it isolates nothing")
	}
}

// needsAnEnvironment skips a test about what is installed in the sandbox when
// this host cannot host one. Same shape as needsAShell, and the same machine:
// building an environment means mounting and chrooting a root filesystem, and
// the loopback machine has no filesystem of its own to do it in.
func needsAnEnvironment(t *testing.T, vm *VM) {
	t.Helper()
	if _, why := vm.Environment(); why != "" {
		t.Skip("no environment in the sandbox here: " + why)
	}
}

func run(t *testing.T, vm *VM, cmd string) ExecResult {
	t.Helper()
	res, err := vm.Exec(context.Background(), cmd, "", 2*time.Minute)
	if err != nil {
		t.Fatalf("exec %q: %v", cmd, err)
	}
	return res
}

func TestTheVMSandboxRunsToolCalls(t *testing.T) {
	ctx := context.Background()
	vm, workdir := newTestVM(t, map[string]string{"main.go": "package main\n"}, policy.Protections{})

	st, err := vm.Status(ctx)
	if err != nil || st.State != StateReady {
		t.Fatalf("status = %+v %v", st, err)
	}

	content, err := vm.Read(ctx, "main.go", 0)
	if err != nil || string(content.Data) != "package main\n" {
		t.Fatalf("read = %q %v", content.Data, err)
	}
	if err := vm.Write(ctx, "main.go", []byte("package main // edited\n")); err != nil {
		t.Fatal(err)
	}
	if err := vm.Write(ctx, "lib/new.go", []byte("package lib\n")); err != nil {
		t.Fatal(err)
	}

	diffs, err := vm.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 {
		t.Fatalf("diff = %d entries, want 2: %+v", len(diffs), diffs)
	}

	// Nothing has reached the developer's tree yet, which is the point.
	onHost, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
	if string(onHost) != "package main\n" {
		t.Fatalf("the host file changed before Apply: %q", onHost)
	}

	n, err := vm.Apply(ctx, nil)
	if err != nil || n != 2 {
		t.Fatalf("apply = %d %v", n, err)
	}
	onHost, _ = os.ReadFile(filepath.Join(workdir, "main.go"))
	if string(onHost) != "package main // edited\n" {
		t.Fatalf("after Apply the host file is %q", onHost)
	}
	if _, err := os.Stat(filepath.Join(workdir, "lib", "new.go")); err != nil {
		t.Fatalf("the created file did not reach the host: %v", err)
	}

	// Applied work is no longer a change: the two sides agree, and there is no
	// manifest that has to be told so.
	diffs, err = vm.Diff(ctx)
	if err != nil || len(diffs) != 0 {
		t.Fatalf("after Apply the diff is %+v %v", diffs, err)
	}
}

// The mount is the part that makes the sandbox an ordinary place to work: a
// command that knows nothing about john reads the developer's files.
func TestACommandInTheSandboxReadsTheDevelopersFiles(t *testing.T) {
	vm, _ := newTestVM(t, map[string]string{
		"main.go":     "package main\n",
		"lib/util.go": "package lib\n",
	}, policy.Protections{})
	needsAShell(t, vm)

	if got := strings.TrimSpace(string(run(t, vm, "cat main.go").Stdout)); got != "package main" {
		t.Fatalf("cat main.go = %q", got)
	}
	if got := strings.TrimSpace(string(run(t, vm, "ls").Stdout)); !strings.Contains(got, "main.go") || !strings.Contains(got, "lib") {
		t.Fatalf("ls = %q", got)
	}
	if got := strings.TrimSpace(string(run(t, vm, "cat lib/util.go").Stdout)); got != "package lib" {
		t.Fatalf("cat lib/util.go = %q", got)
	}
	if res := run(t, vm, "exit 3"); res.ExitCode != 3 {
		t.Fatalf("a non-zero exit is the answer, not an error: %d", res.ExitCode)
	}
}

// A shell command writing into the workspace lands in the sandbox's own layer
// and shows up in the diff — it does not reach the developer's tree, and it is
// not invisible either.
func TestACommandsWritesStayInTheSandboxUntilApplied(t *testing.T) {
	ctx := context.Background()
	vm, workdir := newTestVM(t, map[string]string{"main.go": "package main\n"}, policy.Protections{})
	needsAShell(t, vm)

	run(t, vm, "echo 'generated' > out.txt")
	run(t, vm, "echo '// appended' >> main.go")

	if _, err := os.Stat(filepath.Join(workdir, "out.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a command inside the sandbox created a file in the developer's tree")
	}
	onHost, _ := os.ReadFile(filepath.Join(workdir, "main.go"))
	if string(onHost) != "package main\n" {
		t.Fatalf("a command inside the sandbox changed the developer's file: %q", onHost)
	}

	diffs, err := vm.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	if d, ok := byPath["out.txt"]; !ok || !d.Created || strings.TrimSpace(string(d.After)) != "generated" {
		t.Fatalf("out.txt diff = %+v", d)
	}
	if d, ok := byPath["main.go"]; !ok || !strings.Contains(string(d.After), "appended") {
		t.Fatalf("main.go diff = %+v", d)
	}

	if _, err := vm.Apply(ctx, []string{"out.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "out.txt")); err != nil {
		t.Fatalf("the selected file did not reach the host: %v", err)
	}
	onHost, _ = os.ReadFile(filepath.Join(workdir, "main.go"))
	if strings.Contains(string(onHost), "appended") {
		t.Fatal("applying one path applied another")
	}
}

// The live mount, seen the way a developer would see it: edit a file in an
// editor while a session is open, and the next command inside the sandbox
// reads the new content, with nothing rebuilt and nothing re-synced.
func TestAHostEditIsVisibleToTheNextCommand(t *testing.T) {
	vm, workdir := newTestVM(t, map[string]string{"config.yaml": "version: 1\n"}, policy.Protections{})
	needsAShell(t, vm)

	if got := strings.TrimSpace(string(run(t, vm, "cat config.yaml").Stdout)); got != "version: 1" {
		t.Fatalf("cat = %q", got)
	}
	if err := os.WriteFile(filepath.Join(workdir, "config.yaml"), []byte("version: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "added-later.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(string(run(t, vm, "cat config.yaml").Stdout)); got != "version: 2" {
		t.Fatalf("after a host edit the sandbox reads %q, want version: 2", got)
	}
	if got := strings.TrimSpace(string(run(t, vm, "cat added-later.txt").Stdout)); got != "hello" {
		t.Fatalf("a file created on the host mid-session reads %q", got)
	}
	// And none of that is a change the sandbox made.
	diffs, err := vm.Diff(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("host edits appeared in the sandbox's diff: %+v %v", diffs, err)
	}
}

// Exclusion holds against a shell, not just against the edit tool. There is no
// file to find rather than a refusal to argue with, which is what a root shell
// inside the sandbox cannot get around.
func TestAnExcludedFileIsNotInTheSandboxAtAll(t *testing.T) {
	ctx := context.Background()
	prot := protections(t, map[string][]string{policy.ModeExclude: {".env"}})
	vm, workdir := newTestVM(t, map[string]string{
		".env":    "AWS_SECRET_ACCESS_KEY=real\n",
		"main.go": "package main\n",
	}, prot)
	needsAShell(t, vm)

	if res := run(t, vm, "cat .env"); res.ExitCode == 0 {
		t.Fatalf("a shell read an excluded file: %q", res.Stdout)
	}
	if got := string(run(t, vm, "ls -a").Stdout); strings.Contains(got, ".env") {
		t.Fatalf("an excluded file is listed inside the sandbox: %q", got)
	}
	if got := string(run(t, vm, "grep -r AWS_SECRET . 2>/dev/null || true").Stdout); strings.Contains(got, "real") {
		t.Fatalf("an excluded file's contents were reachable: %q", got)
	}

	_, err := vm.Read(ctx, ".env", 0)
	wantProtected(t, "read .env", err, policy.ModeExclude)

	// And the developer's copy is exactly where it was.
	onHost, _ := os.ReadFile(filepath.Join(workdir, ".env"))
	if string(onHost) != "AWS_SECRET_ACCESS_KEY=real\n" {
		t.Fatalf("the excluded host file changed: %q", onHost)
	}
}

func TestTheVMRefusesProtectedWritesAndDeletes(t *testing.T) {
	ctx := context.Background()
	prot := protections(t, map[string][]string{
		policy.ModeReadOnly: {"vendor"},
		policy.ModeNoDelete: {"go.mod"},
	})
	vm, _ := newTestVM(t, map[string]string{
		"vendor/dep.go": "package dep\n",
		"go.mod":        "module x\n",
	}, prot)

	wantProtected(t, "write vendor/dep.go", vm.Write(ctx, "vendor/dep.go", []byte("x")), policy.ModeReadOnly)
	wantProtected(t, "delete vendor/dep.go", vm.Delete(ctx, "vendor/dep.go"), policy.ModeReadOnly)
	wantProtected(t, "delete go.mod", vm.Delete(ctx, "go.mod"), policy.ModeNoDelete)

	// go.mod may still be edited: no-delete is not read-only.
	if err := vm.Write(ctx, "go.mod", []byte("module y\n")); err != nil {
		t.Fatalf("no-delete refused an edit: %v", err)
	}
}

// Write protection now holds in two places rather than one. The mount refuses
// the write, so ordinary tooling inside the sandbox meets the wall where it
// expects to — and Apply refuses it again, which is the half that holds when
// something goes around the mount, because root in its own machine can always
// write to the disk the upper layer is on.
func TestAReadOnlyPathIsRefusedAtTheMountAndAgainAtApply(t *testing.T) {
	ctx := context.Background()
	prot := protections(t, map[string][]string{policy.ModeReadOnly: {"vendor"}})
	vm, workdir := newTestVM(t, map[string]string{"vendor/dep.go": "package dep\n"}, prot)
	needsAShell(t, vm)

	// Through the mount, the way a build or a script would reach it. chmod is
	// allowed to succeed — the permission bit was never the control — and the
	// write is still refused.
	res := run(t, vm, "chmod u+w vendor/dep.go 2>/dev/null; echo 'package hostile' > vendor/dep.go")
	if res.ExitCode == 0 {
		t.Fatal("a shell wrote to a read-only path through the mount")
	}

	// Around the mount, the way root inside the machine could: straight into
	// the upper layer on the sandbox's own disk. Nothing guest-side can stop
	// this, which is why Apply exists.
	upper := filepath.Join(vm.mach.Root(), "john", "upper", "f", "vendor")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upper, "dep.go"), []byte("package hostile\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diffs, err := vm.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "vendor/dep.go" {
		t.Fatalf("diff = %+v", diffs)
	}
	// Reported, not hidden: that the agent wrote into a protected path is
	// exactly the kind of thing a developer should see.
	if diffs[0].Protected != policy.ModeReadOnly {
		t.Fatalf("the change is not marked protected: %+v", diffs[0])
	}

	// Named explicitly, which is the case that has to hold.
	n, err := vm.Apply(ctx, []string{"vendor/dep.go"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Apply wrote %d protected files", n)
	}
	onHost, _ := os.ReadFile(filepath.Join(workdir, "vendor", "dep.go"))
	if string(onHost) != "package dep\n" {
		t.Fatalf("a protected host file was overwritten: %q", onHost)
	}
}

func TestDeletingAFileReachesTheHostOnlyThroughApply(t *testing.T) {
	ctx := context.Background()
	vm, workdir := newTestVM(t, map[string]string{"gone.txt": "bye\n"}, policy.Protections{})

	if err := vm.Delete(ctx, "gone.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "gone.txt")); err != nil {
		t.Fatal("Delete removed the developer's file")
	}
	diffs, _ := vm.Diff(ctx)
	if len(diffs) != 1 || !diffs[0].Deleted {
		t.Fatalf("diff = %+v", diffs)
	}
	if _, err := vm.Apply(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "gone.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the deletion did not reach the host: %v", err)
	}
}

// A file the sandbox created and then removed again never was a change to the
// developer's tree, and must not be offered as one.
func TestAFileCreatedAndRemovedInTheSandboxIsNotADiff(t *testing.T) {
	vm, _ := newTestVM(t, map[string]string{"main.go": "package main\n"}, policy.Protections{})
	needsAShell(t, vm)
	run(t, vm, "echo scratch > scratch.txt && rm scratch.txt")

	diffs, err := vm.Diff(context.Background())
	if err != nil || len(diffs) != 0 {
		t.Fatalf("diff = %+v %v", diffs, err)
	}
}

func TestResourcesPersistAndApplyAtTheNextStart(t *testing.T) {
	state := t.TempDir()
	workdir := t.TempDir()
	vm, err := NewVM(workdir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := vm.Resources(); got != DefaultResources() {
		t.Fatalf("resources = %+v", got)
	}
	want := Resources{CPUs: 8, MemoryGB: 16, DiskGB: 200, Processes: 2048}
	if err := vm.SetResources(want); err != nil {
		t.Fatal(err)
	}
	again, err := NewVM(workdir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Resources(); got != want {
		t.Fatalf("a new handle sees %+v, want %+v", got, want)
	}
}

// `john apply` runs with no backend and no snapshot. The protections the last
// session installed have to still be there, whichever implementation wrote
// them.
func TestProtectionsSurviveForTheSessionlessApplyOnTheVM(t *testing.T) {
	state := t.TempDir()
	workdir := t.TempDir()
	prot := protections(t, map[string][]string{policy.ModeExclude: {".env"}})

	vm, err := NewVM(workdir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := vm.Protect(prot); err != nil {
		t.Fatal(err)
	}

	later, err := NewVM(workdir, VMOptions{StateDir: state, Hypervisor: &loopback{}})
	if err != nil {
		t.Fatal(err)
	}
	if later.protections().Empty() {
		t.Fatal("a later handle with no backend has no protections")
	}
	wantProtected(t, "read .env", later.protections().CheckRead(".env"), policy.ModeExclude)
}

// A command interrupted on the host stops in the sandbox, and so does what it
// started. Esc on `sleep 300; echo finished` used to report the command
// cancelled while the sleep ran on: the host only stopped waiting, and the
// guest, told nothing, ran the shell to its own deadline — and then killed
// only the shell.
func TestAnInterruptedCommandStopsInTheSandbox(t *testing.T) {
	machines := realMachines(t)
	if len(machines) == 0 {
		t.Skip("no machine here can boot the sandbox")
	}
	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			vm, _ := bootRealMachine(t, m, hostTree(t, map[string]string{"a.txt": "a\n"}), policy.Protections{})
			needsAShell(t, vm)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			start := time.Now()
			vm.Exec(ctx, "sleep 300; echo finished", "", 5*time.Minute)
			if took := time.Since(start); took > 30*time.Second {
				t.Fatalf("the interrupted command took %s to return", took)
			}
			// Killing is asynchronous: give it a moment, then look.
			for i := 0; ; i++ {
				res := run(t, vm, "pgrep -x sleep || echo no-sleep")
				if strings.Contains(string(res.Stdout), "no-sleep") {
					return
				}
				if i == 10 {
					t.Fatalf("the sleep is still running after the command was interrupted: pgrep says %q", res.Stdout)
				}
				time.Sleep(500 * time.Millisecond)
			}
		})
	}
}
