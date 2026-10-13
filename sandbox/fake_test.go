package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func hostTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readHost(t *testing.T, dir, p string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
	if err != nil {
		return "", false
	}
	return string(data), true
}

func writeHost(t *testing.T, dir, p, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFakeSandboxLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := hostTree(t, map[string]string{
		"main.go":        "package main\n",
		"pkg/util.go":    "package pkg\n",
		"docs/readme.md": "# hi\n",
	})
	f := NewFake(dir)
	if err := f.Build(ctx, nil); err != nil {
		t.Fatal(err)
	}

	// Reads fall through to the host.
	c, err := f.Read(ctx, "main.go", 0)
	if err != nil || string(c.Data) != "package main\n" {
		t.Fatalf("read = %q, %v", c.Data, err)
	}

	// Writes are invisible to the host until applied.
	if err := f.Write(ctx, "main.go", []byte("package main // edited\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Write(ctx, "new/file.txt", []byte("fresh\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(ctx, "docs/readme.md"); err != nil {
		t.Fatal(err)
	}
	if got, _ := readHost(t, dir, "main.go"); got != "package main\n" {
		t.Fatalf("host modified before apply: %q", got)
	}
	if _, ok := readHost(t, dir, "new/file.txt"); ok {
		t.Fatal("created file reached the host before apply")
	}

	// The diff is sandbox-vs-host, complete.
	diffs, err := f.Diff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileDiff{}
	for _, d := range diffs {
		byPath[d.Path] = d
	}
	if len(diffs) != 3 {
		t.Fatalf("diff count = %d: %+v", len(diffs), diffs)
	}
	if d := byPath["main.go"]; d.Created || d.Deleted || string(d.After) != "package main // edited\n" {
		t.Fatalf("main.go diff = %+v", d)
	}
	if d := byPath["new/file.txt"]; !d.Created {
		t.Fatalf("new file diff = %+v", d)
	}
	if d := byPath["docs/readme.md"]; !d.Deleted {
		t.Fatalf("deleted diff = %+v", d)
	}

	// Partial apply: only the selected path reaches the host — and every
	// byte of it does (the no-line-lost property, as bytes).
	n, err := f.Apply(ctx, []string{"new/file.txt"})
	if err != nil || n != 1 {
		t.Fatalf("apply = %d, %v", n, err)
	}
	if got, _ := readHost(t, dir, "new/file.txt"); got != "fresh\n" {
		t.Fatalf("applied file = %q, bytes lost", got)
	}
	if got, _ := readHost(t, dir, "main.go"); got != "package main\n" {
		t.Fatal("unselected file was applied")
	}

	// Full apply flushes the rest, including the deletion.
	if _, err := f.Apply(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := readHost(t, dir, "main.go"); got != "package main // edited\n" {
		t.Fatalf("main.go after apply = %q", got)
	}
	if _, ok := readHost(t, dir, "docs/readme.md"); ok {
		t.Fatal("deletion did not apply")
	}
	if f.Pending() != 0 {
		t.Fatalf("pending after full apply = %d", f.Pending())
	}
}

func TestFakeResetDiscardsUnappliedWork(t *testing.T) {
	ctx := context.Background()
	dir := hostTree(t, map[string]string{"a.txt": "one\n"})
	f := NewFake(dir)
	f.Build(ctx, nil)
	f.Write(ctx, "a.txt", []byte("two\n"))
	if err := f.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	diffs, _ := f.Diff(ctx)
	if len(diffs) != 0 {
		t.Fatalf("diff after reset = %+v", diffs)
	}
	if got, _ := readHost(t, dir, "a.txt"); got != "one\n" {
		t.Fatalf("host after reset = %q", got)
	}
}

func TestPathEscapesAreRejected(t *testing.T) {
	ctx := context.Background()
	f := NewFake(t.TempDir())
	f.Build(ctx, nil)
	for _, p := range []string{"../secrets", "/etc/passwd", "a/../../b", ""} {
		if err := f.Write(ctx, p, []byte("x")); err == nil {
			t.Errorf("Write(%q) accepted an escaping path", p)
		}
		if _, err := f.Read(ctx, p, 0); err == nil {
			t.Errorf("Read(%q) accepted an escaping path", p)
		}
	}
}

func TestTruncateOutputKeepsHeadAndTail(t *testing.T) {
	data := []byte("HEAD-" + string(make([]byte, 10000)) + "-TAIL")
	out, truncated := truncateOutput(data, 200)
	if !truncated || len(out) > 200+40 {
		t.Fatalf("truncated=%v len=%d", truncated, len(out))
	}
	if string(out[:5]) != "HEAD-" || string(out[len(out)-5:]) != "-TAIL" {
		t.Fatalf("head/tail lost: %q … %q", out[:5], out[len(out)-5:])
	}
}
