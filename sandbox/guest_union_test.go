package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/popstarapps/john/policy"
)

// directLower is hostFS answering the union in the same process. It is the
// same code that answers the guest over the link — the link is tested
// separately — so these tests are about the merge and nothing else.
type directLower struct{ h *hostFS }

func (d *directLower) Stat(ctx context.Context, rel string) (lowerStat, error) {
	return d.h.stat(rel)
}
func (d *directLower) ReadDir(ctx context.Context, rel string) ([]lowerStat, error) {
	return d.h.readdir(rel)
}
func (d *directLower) ReadAt(ctx context.Context, rel string, off int64, n int) ([]byte, error) {
	return d.h.readAt(rel, off, n)
}

func newTestUnion(t *testing.T, host map[string]string, prot policy.Protections) (*union, string) {
	t.Helper()
	workdir := t.TempDir()
	for rel, content := range host {
		p := filepath.Join(workdir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	u, err := newUnion(&directLower{newHostFS(workdir, prot)}, t.TempDir(), prot)
	if err != nil {
		t.Fatal(err)
	}
	return u, workdir
}

func mustRead(t *testing.T, u *union, rel string) string {
	t.Helper()
	data, err := u.ReadFile(context.Background(), rel)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func names(entries []lowerStat) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func TestTheUnionReadsThroughToTheHostUntilSomethingIsWritten(t *testing.T) {
	ctx := context.Background()
	u, workdir := newTestUnion(t, map[string]string{
		"main.go":     "package main",
		"lib/util.go": "package lib",
	}, policy.Protections{})

	if got := mustRead(t, u, "main.go"); got != "package main" {
		t.Fatalf("read through the lower layer = %q", got)
	}
	if err := u.WriteFile(ctx, "main.go", []byte("edited")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, "main.go"); got != "edited" {
		t.Fatalf("after writing = %q", got)
	}

	// The developer's file is untouched. This is the promise the whole
	// package exists to keep, and the one that has to hold on every path.
	onHost, err := os.ReadFile(filepath.Join(workdir, "main.go"))
	if err != nil || string(onHost) != "package main" {
		t.Fatalf("the host file changed: %q %v", onHost, err)
	}
	if got := mustRead(t, u, "lib/util.go"); got != "package lib" {
		t.Fatalf("an untouched file changed: %q", got)
	}
}

// The direction the developer asked for: a host edit during a session is seen
// by the next command inside the sandbox, with nothing rebuilt and nothing
// re-synced.
func TestAHostEditDuringASessionIsVisibleInsideTheSandbox(t *testing.T) {
	ctx := context.Background()
	u, workdir := newTestUnion(t, map[string]string{"config.yaml": "before"}, policy.Protections{})

	if got := mustRead(t, u, "config.yaml"); got != "before" {
		t.Fatalf("read = %q", got)
	}
	if err := os.WriteFile(filepath.Join(workdir, "config.yaml"), []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, "config.yaml"); got != "after" {
		t.Fatalf("after a host edit the sandbox still reads %q, want %q", got, "after")
	}

	// A file created on the host mid-session appears too, and one deleted on
	// the host disappears — the lower layer is the tree, not a copy of it.
	if err := os.WriteFile(filepath.Join(workdir, "new.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, "new.txt"); got != "hello" {
		t.Fatalf("a file created on the host reads %q", got)
	}
	if err := os.Remove(filepath.Join(workdir, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := u.ReadFile(ctx, "config.yaml"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file deleted on the host still reads: %v", err)
	}
}

// The other direction, which must not hold: once the sandbox has written a
// file, the sandbox's copy is what the sandbox sees, even if the host changes
// underneath. Anything else would let a host edit silently discard the agent's
// unapplied work.
func TestTheSandboxCopyWinsOnceItHasBeenWritten(t *testing.T) {
	ctx := context.Background()
	u, workdir := newTestUnion(t, map[string]string{"contested.txt": "host v1"}, policy.Protections{})

	if err := u.WriteFile(ctx, "contested.txt", []byte("sandbox")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "contested.txt"), []byte("host v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, "contested.txt"); got != "sandbox" {
		t.Fatalf("a host edit overwrote the sandbox's own work: %q", got)
	}
}

func TestDirectoriesMergeAndDeletionsHideTheHostFile(t *testing.T) {
	ctx := context.Background()
	u, _ := newTestUnion(t, map[string]string{
		"a.txt":     "a",
		"b.txt":     "b",
		"sub/c.txt": "c",
	}, policy.Protections{})

	if err := u.WriteFile(ctx, "new.txt", []byte("n")); err != nil {
		t.Fatal(err)
	}
	entries, err := u.ReadDir(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "b.txt", "new.txt", "sub"}
	if got := names(entries); len(got) != len(want) {
		t.Fatalf("merged listing = %v, want %v", got, want)
	}

	if err := u.Remove(ctx, "b.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Stat(ctx, "b.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a deleted file still stats: %v", err)
	}
	entries, _ = u.ReadDir(ctx, "")
	for _, n := range names(entries) {
		if n == "b.txt" {
			t.Fatal("a deleted file is still listed")
		}
	}

	// Deleting a directory hides everything under it, and writing back into
	// it makes exactly that path visible again.
	if err := u.Remove(ctx, "sub/c.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := u.Remove(ctx, "sub", true); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Stat(ctx, "sub/c.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a file under a deleted directory is still there")
	}
	if err := u.WriteFile(ctx, "sub/d.txt", []byte("d")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, "sub/d.txt"); got != "d" {
		t.Fatalf("recreated file = %q", got)
	}
	if _, err := u.Stat(ctx, "sub/c.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("recreating a directory brought a deleted file back")
	}
}

// The deletion markers live in their own tree, so no name a project can
// contain is ever read as a marker. Were they siblings of the files they hide
// — as overlayfs spells them — an agent could make a file vanish from the diff
// the developer reviews by creating one with the right name.
func TestADeletionCannotBeForgedByWritingAFile(t *testing.T) {
	ctx := context.Background()
	u, _ := newTestUnion(t, map[string]string{"secret.txt": "important"}, policy.Protections{})

	for _, forge := range []string{".wh.secret.txt", ".jwh.secret.txt", "..wh..wh..opq"} {
		if err := u.WriteFile(ctx, forge, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustRead(t, u, "secret.txt"); got != "important" {
		t.Fatalf("a written file hid another one: %q", got)
	}
	changes, err := u.Changes()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Deleted {
			t.Fatalf("writing %q was recorded as a deletion", c.Path)
		}
	}
}

func TestChangesAreTheUpperLayerAndNothingElse(t *testing.T) {
	ctx := context.Background()
	u, _ := newTestUnion(t, map[string]string{
		"kept.txt":    "untouched",
		"edited.txt":  "before",
		"removed.txt": "gone soon",
	}, policy.Protections{})

	if err := u.WriteFile(ctx, "edited.txt", []byte("after")); err != nil {
		t.Fatal(err)
	}
	if err := u.WriteFile(ctx, "created.txt", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := u.Remove(ctx, "removed.txt", false); err != nil {
		t.Fatal(err)
	}

	changes, err := u.Changes()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range changes {
		got[c.Path] = c.Deleted
	}
	if len(got) != 3 {
		t.Fatalf("changes = %v, want three", got)
	}
	if _, ok := got["kept.txt"]; ok {
		t.Fatal("a file the sandbox never touched is reported as a change")
	}
	if !got["removed.txt"] {
		t.Fatal("the deletion is missing")
	}
	if got["edited.txt"] || got["created.txt"] {
		t.Fatal("a write is reported as a deletion")
	}
}

// A shell inside the sandbox writes in pieces, appends, truncates and renames.
// All of it has to work against files that exist only in the developer's tree,
// which means copying them up first.
func TestPartialWritesCopyTheFileUpFirst(t *testing.T) {
	ctx := context.Background()
	u, workdir := newTestUnion(t, map[string]string{"notes.txt": "hello world"}, policy.Protections{})

	if _, err := u.WriteAt(ctx, "notes.txt", 6, []byte("there")); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, "notes.txt"); got != "hello there" {
		t.Fatalf("after a partial write = %q", got)
	}
	if err := u.Truncate(ctx, "notes.txt", 5); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, "notes.txt"); got != "hello" {
		t.Fatalf("after truncation = %q", got)
	}
	onHost, _ := os.ReadFile(filepath.Join(workdir, "notes.txt"))
	if string(onHost) != "hello world" {
		t.Fatalf("the host file changed: %q", onHost)
	}
}

func TestRenamingOutOfTheHostTreeCopiesAndMarksTheOriginalGone(t *testing.T) {
	ctx := context.Background()
	u, workdir := newTestUnion(t, map[string]string{"old.txt": "content"}, policy.Protections{})

	if err := u.Rename(ctx, "old.txt", "new.txt"); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, "new.txt"); got != "content" {
		t.Fatalf("renamed file = %q", got)
	}
	if _, err := u.Stat(ctx, "old.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the original is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, "old.txt")); err != nil {
		t.Fatal("renaming inside the sandbox removed the developer's file")
	}
}

// Exclusion is enforced by the layer that owns the developer's tree, so it
// holds against anything inside the sandbox — there is no file to find rather
// than a refusal to argue with.
func TestAnExcludedPathIsNotServedAtAll(t *testing.T) {
	ctx := context.Background()
	prot := protections(t, map[string][]string{policy.ModeExclude: {".env"}})
	u, _ := newTestUnion(t, map[string]string{".env": "SECRET=1", "main.go": "package main"}, prot)

	if _, err := u.Stat(ctx, ".env"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an excluded file stats: %v", err)
	}
	if _, err := u.ReadFile(ctx, ".env"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an excluded file reads: %v", err)
	}
	entries, err := u.ReadDir(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names(entries) {
		if n == ".env" {
			t.Fatal("an excluded file is listed in its directory")
		}
	}
}

// A policy tightened mid-session reaches a machine that is already running:
// exclusion is answered by the side serving the tree, on every request, so a
// path can be taken back from a sandbox that has already started.
func TestTighteningAPolicyTakesAPathAwayFromARunningSandbox(t *testing.T) {
	ctx := context.Background()
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := newHostFS(workdir, policy.Protections{})
	u, err := newUnion(&directLower{host}, t.TempDir(), policy.Protections{})
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, u, ".env"); got != "SECRET=1" {
		t.Fatalf("read = %q", got)
	}

	prot := protections(t, map[string][]string{policy.ModeExclude: {".env"}})
	host.setProtections(prot)
	u.setProtections(prot)

	if _, err := u.ReadFile(ctx, ".env"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the path was still served after the policy tightened: %v", err)
	}
}

// A removal the union refuses says which refusal it is, as an errno a program
// can act on. Node's rmSync tries rmdir on a directory first and empties it
// only when told ENOTEMPTY; told anything else it stops, and every `vite
// build` after the first failed with "EIO: i/o error, rmdir 'dist/assets'".
func TestARefusedRemovalSaysWhy(t *testing.T) {
	ctx := context.Background()
	u, _ := newTestUnion(t, map[string]string{"host/a.txt": "a"}, policy.Protections{})
	if err := u.WriteFile(ctx, "dist/assets/app.js", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rel  string
		dir  bool
		want syscall.Errno
	}{
		{"dist/assets", true, syscall.ENOTEMPTY}, // written in the sandbox
		{"host", true, syscall.ENOTEMPTY},        // only in the host tree
		{"dist/assets/app.js", true, syscall.ENOTDIR},
		{"dist", false, syscall.EISDIR},
	} {
		if err := u.Remove(ctx, c.rel, c.dir); !errors.Is(err, c.want) {
			t.Errorf("Remove(%q, dir=%v) = %v, want %v", c.rel, c.dir, err, c.want)
		}
	}
}
