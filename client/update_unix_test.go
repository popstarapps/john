//go:build !windows

package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Replacing the binary is the most destructive thing john does to a
// developer's machine, so it is worth proving rather than assuming: after
// this, the path holds the new build, it is executable, and nothing of the
// old one is left behind under another name.
func TestReplacingTheBinaryInstallsTheNewBuild(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "john")
	if err := os.WriteFile(exe, []byte("the old build"), 0o755); err != nil {
		t.Fatal(err)
	}
	downloaded := filepath.Join(dir, ".john-update-xyz")
	if err := os.WriteFile(downloaded, []byte("the new build"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := replaceBinary(exe, downloaded); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the new build" {
		t.Fatalf("john is now %q", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("the installed build is not executable: %v", fi.Mode())
	}

	// One file, named john. A leftover beside it would be a second binary
	// nobody knows about.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "john" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("after the swap the directory holds %v", names)
	}
}

// relaunch has to REPLACE this process, not fork a second john underneath the
// first — the pid, the terminal and the descriptors carry over, which is what
// makes an update look like john simply carrying on.
//
// It cannot be called in-process without ending the test binary, so this runs
// itself as a child. The proof is negative and exact: the child execs the
// target, so the Go test framework in that child never reaches its own
// "PASS". Seeing the target's output without it means the image really was
// replaced.
func TestRelaunchReplacesThisProcessRatherThanForking(t *testing.T) {
	const marker = "i-am-the-new-build"

	if os.Getenv("JOHN_RELAUNCH_CHILD") == "1" {
		target := os.Getenv("JOHN_RELAUNCH_TARGET")
		if _, err := relaunch(target, []string{target}); err != nil {
			// Only reached when exec failed; the parent sees no marker.
			t.Fatalf("relaunch: %v", err)
		}
		return
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "new-john")
	script := "#!/bin/sh\necho " + marker + "\n"
	if err := os.WriteFile(target, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRelaunchReplacesThisProcessRatherThanForking", "-test.v")
	cmd.Env = append(os.Environ(),
		"JOHN_RELAUNCH_CHILD=1",
		"JOHN_RELAUNCH_TARGET="+target,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	got := string(out)

	if !strings.Contains(got, marker) {
		t.Fatalf("the new build never ran:\n%s", got)
	}
	if strings.Contains(got, "PASS") || strings.Contains(got, "--- PASS") {
		t.Fatalf("the child survived relaunch — it forked instead of being replaced:\n%s", got)
	}
}

// An exec that cannot happen has to come back as an error, so runUpdate can
// say so instead of leaving the developer with a replaced binary and no
// explanation of why nothing started.
func TestRelaunchReportsAnExecThatCannotHappen(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-a-binary")
	if _, err := relaunch(missing, []string{missing}); err == nil {
		t.Fatal("relaunching something that does not exist reported success")
	}
}
