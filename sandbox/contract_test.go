package sandbox

import (
	"context"
	"testing"

	"github.com/popstarapps/john/policy"
)

// The Sandbox interface is a contract, and these are the parts of it that are
// controls rather than conveniences. They run against every implementation,
// because a protection that held only where one of them is installed would be
// no protection at all — and because the interface is the thing the client
// programs against, so an implementation that answers it differently is a bug
// nobody would find until a developer's files were already gone.
//
// The set is whatever this machine can run: the in-memory sandbox always, the
// virtual machine over the loopback link always, and — where a guest image has
// been built — the same virtual machine inside an actual booted kernel. That
// last one is the point of having a set at all: the controls are claims about
// a boundary, and the boundary is only real in the last of them.

type impl struct {
	name string
	// build returns a sandbox over the host tree at dir, already built and
	// ready for tool calls.
	build func(t *testing.T, dir string, prot policy.Protections) Sandbox
}

func implementations(t *testing.T) []impl {
	impls := []impl{
		{"fake", func(t *testing.T, dir string, prot policy.Protections) Sandbox {
			f := NewFake(dir)
			if err := f.Protect(prot); err != nil {
				t.Fatal(err)
			}
			if err := f.Build(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			return f
		}},
		{"vm", func(t *testing.T, dir string, prot policy.Protections) Sandbox {
			vm, err := NewVM(dir, VMOptions{StateDir: t.TempDir(), Protect: prot, Hypervisor: &loopback{}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { vm.Stop(context.Background()) })
			if err := vm.Build(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			return vm
		}},
	}
	for _, rm := range realMachines(t) {
		impls = append(impls, impl{rm.name, func(t *testing.T, dir string, prot policy.Protections) Sandbox {
			vm, _ := bootRealMachine(t, rm, dir, prot)
			return vm
		}})
	}
	return impls
}

// eachSandbox runs one set of assertions against every implementation.
func eachSandbox(t *testing.T, files map[string]string, byMode map[string][]string, fn func(t *testing.T, sb Sandbox, dir string)) {
	t.Helper()
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			dir := hostTree(t, files)
			prot := protections(t, byMode)
			fn(t, im.build(t, dir, prot), dir)
		})
	}
}

// The enforcement point that matters: a tool call cannot reach a protected
// path, whatever the caller forgot to check.
func TestEverySandboxRefusesAProtectedPath(t *testing.T) {
	files := map[string]string{
		".env":                    "STRIPE_KEY=sk_live_real\n",
		".git/config":             "[core]\n",
		".github/workflows/ci.go": "package ci\n",
		"main.go":                 "package main\n",
	}
	modes := map[string][]string{
		policy.ModeExclude:  {".env"},
		policy.ModeReadOnly: {".git"},
		policy.ModeNoDelete: {".github/workflows"},
	}
	eachSandbox(t, files, modes, func(t *testing.T, sb Sandbox, dir string) {
		ctx := context.Background()

		// Excluded: not readable, and — the one that would otherwise clobber a
		// real secret at apply — not creatable over either.
		_, err := sb.Read(ctx, ".env", 0)
		wantProtected(t, "read .env", err, policy.ModeExclude)
		wantProtected(t, "write .env", sb.Write(ctx, ".env", []byte("STRIPE_KEY=stolen\n")), policy.ModeExclude)

		// Read-only: readable, not writable, not removable.
		if _, err := sb.Read(ctx, ".git/config", 0); err != nil {
			t.Fatalf("read .git/config: %v", err)
		}
		wantProtected(t, "write .git/config", sb.Write(ctx, ".git/config", []byte("bare=true\n")), policy.ModeReadOnly)
		wantProtected(t, "delete .git/config", sb.Delete(ctx, ".git/config"), policy.ModeReadOnly)

		// No-delete: editable, not removable.
		if err := sb.Write(ctx, ".github/workflows/ci.go", []byte("package ci // edited\n")); err != nil {
			t.Fatalf("write ci.go: %v", err)
		}
		wantProtected(t, "delete ci.go", sb.Delete(ctx, ".github/workflows/ci.go"), policy.ModeNoDelete)

		// Everything else is untouched by any of this.
		if err := sb.Write(ctx, "main.go", []byte("package main // edited\n")); err != nil {
			t.Fatalf("write main.go: %v", err)
		}
		if err := sb.Delete(ctx, "main.go"); err != nil {
			t.Fatalf("delete main.go: %v", err)
		}
	})
}

// The way around a rule that a case-insensitive working directory would
// otherwise open. The sandbox is Linux, so `.SECURE` is a different file there
// and a shell can create it freely; on the macOS or Windows host it is the
// same file as `.secure`, and applying it would overwrite exactly what the
// policy protects.
func TestEverySandboxTreatsADifferentSpellingAsTheSameFile(t *testing.T) {
	eachSandbox(t,
		map[string]string{".secure": "original\n"},
		map[string][]string{policy.ModeReadOnly: {".secure"}},
		func(t *testing.T, sb Sandbox, dir string) {
			ctx := context.Background()
			wantProtected(t, "write .SECURE", sb.Write(ctx, ".SECURE", []byte("blah\n")), policy.ModeReadOnly)
			wantProtected(t, "write .Secure", sb.Write(ctx, ".Secure", []byte("blah\n")), policy.ModeReadOnly)

			if n, err := sb.Apply(ctx, nil); err != nil || n != 0 {
				t.Fatalf("Apply = %d, %v — want 0 files and no error", n, err)
			}
			if got, _ := readHost(t, dir, ".secure"); got != "original\n" {
				t.Fatalf(".secure on the host = %q", got)
			}
		})
}

// The backstop, across implementations: a change made before the policy
// arrived is reported and refused rather than hidden and applied.
func TestEverySandboxRefusesAProtectedChangeAtApply(t *testing.T) {
	files := map[string]string{".git/config": "[core]\n", "main.go": "package main\n"}
	for _, im := range implementations(t) {
		t.Run(im.name, func(t *testing.T) {
			ctx := context.Background()
			dir := hostTree(t, files)
			sb := im.build(t, dir, policy.Protections{})

			// Both changes are made while nothing is protected.
			if err := sb.Write(ctx, ".git/config", []byte("[core]\nbare=true\n")); err != nil {
				t.Fatal(err)
			}
			if err := sb.Write(ctx, "main.go", []byte("package main // fixed\n")); err != nil {
				t.Fatal(err)
			}

			// Then the policy arrives, the way a refresh reaches a running
			// session.
			if err := sb.Protect(protections(t, map[string][]string{policy.ModeReadOnly: {".git"}})); err != nil {
				t.Fatal(err)
			}

			diffs, err := sb.Diff(ctx)
			if err != nil {
				t.Fatal(err)
			}
			marked := map[string]string{}
			for _, d := range diffs {
				marked[d.Path] = d.Protected
			}
			if marked[".git/config"] != policy.ModeReadOnly {
				t.Errorf(".git/config marked %q, want %q", marked[".git/config"], policy.ModeReadOnly)
			}
			if marked["main.go"] != "" {
				t.Errorf("main.go marked %q, want unprotected", marked["main.go"])
			}

			n, err := sb.Apply(ctx, nil)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if n != 1 {
				t.Errorf("applied %d files, want 1", n)
			}
			if got, _ := readHost(t, dir, ".git/config"); got != "[core]\n" {
				t.Errorf(".git/config on the host = %q — the protection did not hold", got)
			}
			if got, _ := readHost(t, dir, "main.go"); got != "package main // fixed\n" {
				t.Errorf("main.go on the host = %q", got)
			}

			// Naming the protected path explicitly does not talk Apply into it.
			if n, err := sb.Apply(ctx, []string{".git/config"}); err != nil || n != 0 {
				t.Errorf("Apply(.git/config) = %d, %v — want 0 files and no error", n, err)
			}
			if got, _ := readHost(t, dir, ".git/config"); got != "[core]\n" {
				t.Errorf(".git/config on the host = %q — the protection did not hold", got)
			}
		})
	}
}

// The lifecycle every implementation owes the client: build, edit, diff,
// apply, and a reset that discards what was never applied.
func TestEverySandboxAppliesOnlyWhatIsSelected(t *testing.T) {
	files := map[string]string{"a.txt": "a\n", "b.txt": "b\n"}
	eachSandbox(t, files, nil, func(t *testing.T, sb Sandbox, dir string) {
		ctx := context.Background()
		if err := sb.Write(ctx, "a.txt", []byte("a edited\n")); err != nil {
			t.Fatal(err)
		}
		if err := sb.Write(ctx, "b.txt", []byte("b edited\n")); err != nil {
			t.Fatal(err)
		}
		if n, err := sb.Apply(ctx, []string{"a.txt"}); err != nil || n != 1 {
			t.Fatalf("Apply(a.txt) = %d, %v", n, err)
		}
		if got, _ := readHost(t, dir, "a.txt"); got != "a edited\n" {
			t.Errorf("a.txt on the host = %q", got)
		}
		if got, _ := readHost(t, dir, "b.txt"); got != "b\n" {
			t.Errorf("b.txt on the host = %q — applying one path applied another", got)
		}
	})
}

// Once applied, a file is the developer's again: an edit they make to it on
// the host is what the sandbox sees, and the next review does not offer the
// sandbox's old version back. The sandbox used to keep its copy, which hid the
// edit, and the next apply wrote over it. The same holds for a deletion
// applied and the file then made again on the host.
func TestEverySandboxHandsAnAppliedFileBackToTheHost(t *testing.T) {
	files := map[string]string{"a.txt": "a\n", "b.txt": "b\n"}
	eachSandbox(t, files, nil, func(t *testing.T, sb Sandbox, dir string) {
		ctx := context.Background()
		if err := sb.Write(ctx, "a.txt", []byte("a from the sandbox\n")); err != nil {
			t.Fatal(err)
		}
		if err := sb.Delete(ctx, "b.txt"); err != nil {
			t.Fatal(err)
		}
		if n, err := sb.Apply(ctx, nil); err != nil || n != 2 {
			t.Fatalf("Apply = %d, %v", n, err)
		}
		writeHost(t, dir, "a.txt", "a edited on the host\n")
		writeHost(t, dir, "b.txt", "b made again on the host\n")

		for path, want := range map[string]string{"a.txt": "a edited on the host\n", "b.txt": "b made again on the host\n"} {
			got, err := sb.Read(ctx, path, 0)
			if err != nil || string(got.Data) != want {
				t.Errorf("the sandbox reads %s as %q, %v — want the host's %q", path, got.Data, err, want)
			}
		}
		diffs, err := sb.Diff(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range diffs {
			t.Errorf("after the host's edits the review offers %s — applying it would undo them", d.Path)
		}
	})
}
