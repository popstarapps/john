package sandbox

import "testing"

// The sandbox mounts the workspace where the host keeps it, so that a path in
// a compiler error, a stack trace or a build cache means the same thing on
// both sides of the machine.
func TestTheSandboxMirrorsTheHostPath(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"/Users/bob/project", "/Users/bob/project"},
		{"/home/bob/src/john", "/home/bob/src/john"},
		{"/Users/bob/project/", "/Users/bob/project"},
		{"/Users/bob/./project", "/Users/bob/project"},

		// Windows has no path Linux can hold, so it is transliterated the way
		// a POSIX shell on Windows already spells it.
		{`C:\Users\bob\project`, "/c/Users/bob/project"},
		{`c:\Users\bob\project`, "/c/Users/bob/project"},
		{`D:\src`, "/d/src"},
		{`\\build\share\project`, "/unc/build/share/project"},

		// Nothing to mirror, or nothing safe to mirror onto.
		{"", "/workspace"},
		{"relative/path", "/workspace"},
		{"/", "/workspace"},
	}
	for _, c := range cases {
		if got := GuestMount(c.host); got != c.want {
			t.Errorf("GuestMount(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

// Mounting the workspace at / would put the sandbox's own filesystem behind
// the developer's project. The fallback is a mount point, never the root.
func TestTheWorkspaceIsNeverMountedOverTheRoot(t *testing.T) {
	for _, host := range []string{"/", "//", `C:\`, `\\`, "/.."} {
		if got := GuestMount(host); got == "/" {
			t.Errorf("GuestMount(%q) = %q — the workspace would cover the sandbox's own root", host, got)
		}
	}
}
