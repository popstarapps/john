package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// One workspace, one sandbox — and a name a person can read.
//
// A sandbox is named for the directory it backs, and the name has to satisfy
// two things at once. It has to be legible: ~/.john/sandboxes full of hashes
// is a directory nobody can audit, reclaim or reason about, and the first
// question anyone asks of it is "which of these is my project". And it has to
// be exact: two names for one directory means two sandboxes for one project,
// each with its own environment and its own unapplied work, and `john apply`
// run from one of them cannot see what the other holds.
//
// So the name is the directory's own, plus a hash of the canonical path that
// makes it unambiguous:
//
//	~/Projects/billing-api  →  billing-api-9f8e7d6c1a2b
//
// The hash is of the path as the filesystem actually resolves it, not as it
// was typed. On macOS /tmp is a symbolic link to /private/tmp, so the same
// project reached the two ways would otherwise get two sandboxes; and on a
// case-insensitive filesystem — which is the default on macOS and Windows —
// ~/Projects/John and ~/Projects/john are one directory and must be one
// sandbox.

// WorkspaceKey names the sandbox for a working directory.
func WorkspaceKey(workdir string) string {
	canon := canonicalWorkspace(workdir)
	host, _ := os.Hostname()
	sum := sha256.Sum256([]byte(host + "\x00" + foldPath(canon)))
	return workspaceSlug(filepath.Base(canon)) + "-" + hex.EncodeToString(sum[:6])
}

// canonicalWorkspace is the directory as the filesystem sees it, with symbolic
// links followed. A path that cannot be resolved — a workspace that does not
// exist yet — is used as given, because refusing to name it would be worse
// than naming it twice.
func canonicalWorkspace(workdir string) string {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		abs = workdir
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// foldPath lowercases the path where the platform's filesystem is
// case-insensitive, so one directory has one name.
//
// It is decided by platform rather than probed. Probing means writing a file
// to find out, the answer can differ per mount, and being wrong in the
// direction this chooses is harmless: two case-different paths to genuinely
// different directories on a case-sensitive volume mounted on a Mac would
// share a sandbox, which is confusing, where the other direction corrupts one
// project's work with another's.
func foldPath(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// workspaceSlug is the directory's name, reduced to something safe as a
// filename and as an image tag at once.
func workspaceSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if !strings.HasSuffix(b.String(), "-") {
			// Anything else becomes a separator, and a run of them becomes
			// one: "My Project (v2)" is my-project-v2 and not my--project--v2.
			b.WriteRune('-')
		}
		if b.Len() >= 32 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	// A directory named "/" or "..." leaves nothing usable, and a name has to
	// start with a letter or a digit to be a container name.
	if slug == "" || !(slug[0] >= 'a' && slug[0] <= 'z' || slug[0] >= '0' && slug[0] <= '9') {
		slug = "workspace" + slug
	}
	return slug
}

// legacyWorkspaceKey is what sandboxes were named before they were named after
// their workspace. It exists so an upgrade finds the sandbox it already has
// rather than silently starting a second one and leaving a developer's
// unapplied work in a directory nothing points at any more.
func legacyWorkspaceKey(workdir string) string {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		abs = workdir
	}
	host, _ := os.Hostname()
	sum := sha256.Sum256([]byte(host + "\x00" + abs))
	return hex.EncodeToString(sum[:8])
}

// adoptLegacyStateDir renames a sandbox from the old naming to the new one, so
// the upgrade is invisible. It does nothing when there is nothing to move, and
// never overwrites a sandbox that already exists under the new name.
func adoptLegacyStateDir(workdir string, dir stateDir) {
	if _, err := os.Stat(string(dir)); err == nil {
		return
	}
	old, err := defaultStateDir(legacyWorkspaceKey(workdir))
	if err != nil {
		return
	}
	if _, err := os.Stat(string(old)); err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(string(dir)), 0o700)
	os.Rename(string(old), string(dir))
}
