package sandbox

import (
	"path/filepath"
	"strings"
	"unicode"
)

// GuestMount is where a host working directory is mounted inside the sandbox.
//
// The sandbox mirrors the host path wherever the host path can be spelled in
// Linux, so that /Users/bob/project is /Users/bob/project on both sides. This
// is not cosmetic. Compilers write absolute paths into debug information, Go
// keys its build cache on them, sourcemaps and stack traces carry them, and
// test output names them — so a mirrored path means a failure inside the
// sandbox prints something the developer can click in their editor, and a
// rebuild after `john apply` does not miss on every cache entry because the
// tree moved.
//
// Windows paths cannot be mirrored literally: C:\Users\bob\project is not a
// path Linux can hold. They are transliterated to /c/Users/bob/project, the
// spelling Git Bash and MSYS use, which is short, reversible and already
// familiar to anyone who has run a POSIX shell on Windows.
//
// Anything that cannot be mapped at all falls back to /workspace, which is
// where every sandbox lived before mirroring and is always a valid answer.
func GuestMount(hostDir string) string {
	if p, ok := mirrorPath(hostDir); ok {
		return p
	}
	return "/workspace"
}

// mirrorPath does the transliteration, reporting whether the host path was
// one it could mirror at all.
func mirrorPath(hostDir string) (string, bool) {
	p := strings.ReplaceAll(strings.TrimSpace(hostDir), `\`, "/")
	if p == "" {
		return "", false
	}

	// \\server\share\dir → /unc/server/share/dir. A UNC path has no drive to
	// borrow, and the share name is the closest thing to a root it has.
	if rest, ok := strings.CutPrefix(p, "//"); ok {
		rest = strings.Trim(rest, "/")
		if rest == "" {
			return "", false
		}
		return cleanGuest("/unc/" + rest)
	}

	// C:/Users/bob/project → /c/Users/bob/project.
	if len(p) >= 2 && p[1] == ':' && isDriveLetter(rune(p[0])) {
		rest := strings.TrimPrefix(p[2:], "/")
		return cleanGuest("/" + string(unicode.ToLower(rune(p[0]))) + "/" + rest)
	}

	if strings.HasPrefix(p, "/") {
		return cleanGuest(p)
	}

	// A relative path has no host location to mirror; the caller passes an
	// absolute one, and this is the honest answer when it did not.
	return "", false
}

// cleanGuest normalises a mirrored path and refuses one that cannot be a
// mount point: NUL bytes, or a path that cleans away to the root itself.
// Mounting the workspace at / would put the guest's own filesystem behind it.
func cleanGuest(p string) (string, bool) {
	if strings.ContainsRune(p, 0) {
		return "", false
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "/" || p == "." {
		return "", false
	}
	return p, true
}

func isDriveLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}
