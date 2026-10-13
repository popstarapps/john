package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/popstarapps/john/policy"
)

// A junction is Windows' other link. Any user can make one, with no Developer
// Mode, and Lstat does not report it as a symbolic link, so a walk that only
// refuses links walks straight through it to wherever it points.
func TestAJunctionIsNotAWayOutOfTheWorkspace(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("not yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := hostTree(t, map[string]string{"main.go": "package main\n"})
	out := filepath.Join(dir, "out")
	if b, err := exec.Command("cmd", "/c", "mklink", "/J", out, outside).CombinedOutput(); err != nil {
		t.Skipf("cannot make a junction: %v: %s", err, b)
	}

	h := newHostFS(dir, policy.Protections{})
	if _, err := h.readAt("out/secret", 0, 16); err == nil {
		t.Fatal("the host read a file outside the workspace through a junction")
	}
	if _, err := hostPathFor(dir, "out/planted", false); err == nil {
		t.Fatal("apply accepted a path through a junction out of the workspace")
	}
}

// A link the developer made on Windows names its target the Windows way. Read
// as the guest reads it, that must still be a climb out, not a filename.
func TestAWindowsLinkTargetIsReadTheGuestsWay(t *testing.T) {
	for target, want := range map[string]string{
		`..\lib\cli.js`:    "../lib/cli.js",
		`C:\Users\bob`:     "/C:/Users/bob",
		`C:relative`:       "/C:relative",
		`\\server\share\x`: "//server/share/x",
		`\rooted`:          "/rooted",
	} {
		if got := linkTarget(target); got != want {
			t.Errorf("linkTarget(%q) = %q, want %q", target, got, want)
		}
	}
	for _, target := range []string{`..\..\etc`, `C:\Users\bob`, `\\server\share`} {
		if _, err := resolveLink("out", linkTarget(target)); err == nil {
			t.Errorf("a link to %s was followed", target)
		}
	}
}
