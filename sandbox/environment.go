package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sandbox environments are defined by the host's .john/Dockerfile, with .john/
// as the build context. See "Sandbox environment" in readme.md for the design
// and supported instructions.
const (
	// EnvDir is the project's john directory.
	EnvDir = ".john"
	// EnvFile is the project's sandbox environment definition, relative to
	// the working directory.
	EnvFile = EnvDir + "/Dockerfile"
)

// environment is the resolved sandbox environment for one workspace.
type environment struct {
	// Dockerfile is the host path of the project's definition, empty when the
	// project does not ship one.
	Dockerfile string
	// Context is the host directory the build runs in.
	Context string
	// ID hashes the build context. A change triggers an environment rebuild
	// while preserving the workspace and unapplied changes.
	ID string
}

// Default reports whether this project supplies no environment of its own.
func (e environment) Default() bool { return e.Dockerfile == "" }

// Describe names the environment for Status and for the line a developer sees
// while the sandbox starts.
func (e environment) Describe() string {
	if e.Default() {
		return "john/default"
	}
	return EnvFile + "@" + e.ID[:12]
}

// resolveEnvironment finds the project's environment and records it beside
// the sandbox, so that a later start can tell whether the definition moved
// under a machine that is already built.
func resolveEnvironment(workdir string, dir stateDir) (environment, error) {
	path := filepath.Join(workdir, filepath.FromSlash(EnvFile))
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return environment{ID: defaultEnvID}, nil
	case err != nil:
		return environment{}, err
	case info.IsDir():
		return environment{}, fmt.Errorf("sandbox: %s is a directory", EnvFile)
	}

	ctx := filepath.Join(workdir, EnvDir)
	id, err := hashTree(ctx)
	if err != nil {
		return environment{}, err
	}
	return environment{Dockerfile: path, Context: ctx, ID: id}, nil
}

// defaultEnvID names the built-in environment. It is a constant rather than a
// hash because the built-in environment ships with john: it changes when john
// changes, and the version stamp already says so.
const defaultEnvID = "default00000000"

// hashTree hashes a directory's contents — names, modes and bytes — so that
// the identity of an environment covers everything a build would see.
func hashTree(root string) (string, error) {
	type entry struct {
		rel  string
		mode fs.FileMode
		sum  [32]byte
	}
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		entries = append(entries, entry{filepath.ToSlash(rel), info.Mode().Perm(), sha256.Sum256(data)})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%o\x00%s\x00", e.rel, e.mode, hex.EncodeToString(e.sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// StarterEnvironment is the .john/Dockerfile john offers to write for a
// project that has none, and what a project without one is built from: a
// sandbox has the same basic tools whether or not the file is written down.
// It is a starting point a developer edits, not a default they are stuck
// with — which is the whole reason the environment is a file in the
// repository rather than a setting somewhere.
func StarterEnvironment() string {
	return strings.Join([]string{
		"# The sandbox john runs this project's tools in.",
		"#",
		"# This is a development environment, not a deployment image: put the",
		"# compilers, linters, test runners and language servers here. It is",
		"# built before any session starts, with the network open unless your",
		"# organization holds image builds to its network policies.",
		"#",
		"# The build context is this directory (.john), so COPY reads from here.",
		"",
		"FROM " + DefaultBaseImage,
		"",
		"# The basics most projects expect. Add this project's own toolchain below",
		"# — a language runtime, its package manager, the linters and test runners",
		"# the project uses — and john rebuilds the sandbox from it.",
		"",
		"# apt installs what a package needs and not what it recommends, here and",
		"# for every apt-get install run in the sandbox later: recommends is where",
		"# a server package pulls in a desktop. Delete this line to have them.",
		"RUN echo 'APT::Install-Recommends \"false\";' > /etc/apt/apt.conf.d/99-no-recommends",
		"",
		"RUN apt-get update && apt-get install -y \\",
		"      ca-certificates curl git less procps \\",
		"      build-essential file jq ripgrep unzip \\",
		"    && rm -rf /var/lib/apt/lists/*",
		"",
	}, "\n")
}

// DefaultBaseImage is what the built-in environment is built from, and what a
// starter .john/Dockerfile starts FROM.
const DefaultBaseImage = "debian:stable-slim"

// EnvGitignore is written alongside a generated .john/Dockerfile. The
// directory holds one tracked file and nothing else should join it by
// accident — the sandbox's disks live in ~/.john/sandboxes, deliberately
// outside the tree they mount.
const EnvGitignore = "*\n!.gitignore\n!Dockerfile\n"

// Environment describes the sandbox environment a working directory would get,
// for the client to show. It never builds anything.
func Environment(workdir string) (file string, id string, custom bool, err error) {
	env, err := resolveEnvironment(workdir, "")
	if err != nil {
		return "", "", false, err
	}
	if env.Default() {
		return "", env.Describe(), false, nil
	}
	return EnvFile, env.Describe(), true, nil
}

// CheckEnvironment reads the project's .john/Dockerfile the way a build would,
// so a mistake is reported when the file is saved rather than minutes into a
// rebuild. A project without one has nothing to check.
func CheckEnvironment(workdir string) error {
	text, err := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(EnvFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = parseDockerfile(string(text))
	return err
}

// WriteStarterIfNew writes the starter .john/Dockerfile the first time john
// starts in a project, so the file that decides what the sandbox holds is
// there to be found and edited rather than something to know to ask for.
//
// Only then, and only in a project: this directory is the top of a git
// repository or empty, john has never built a sandbox for it, and there is
// no .john/Dockerfile yet. The first rules out writing into a home directory
// or wherever else john happens to be started, while an empty directory is a
// project about to begin — the one place a developer most needs to find the
// file, because nothing is installed yet. The second rules out changing the
// environment of a sandbox that already exists, which would rebuild it on
// its next start for no reason anyone chose. It reports what it wrote, or ""
// when it wrote nothing.
func (v *VM) WriteStarterIfNew() (string, error) {
	if _, err := os.Stat(filepath.Join(v.workdir, ".git")); err != nil && !emptyDir(v.workdir) {
		return "", nil
	}
	if len(v.stateDir.instances()) > 0 {
		return "", nil
	}
	if entries, err := os.ReadDir(string(v.stateDir)); err == nil {
		for _, e := range entries {
			if isEnvDisk(e.Name()) {
				return "", nil
			}
		}
	}
	if _, err := os.Stat(filepath.Join(v.workdir, filepath.FromSlash(EnvFile))); err == nil {
		return "", nil
	}
	return InitEnvironment(v.workdir)
}

// emptyDir reports a directory with nothing in it but what Finder leaves.
func emptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != ".DS_Store" {
			return false
		}
	}
	return true
}

// InitEnvironment writes a starter .john/Dockerfile for a project that has
// none, and the .gitignore that keeps the directory to what belongs in it.
//
// This writes the developer's project, and it is the developer asking. It is
// not a hole in the rule that only Apply writes the working directory: that
// rule is about the sandbox, which reaches the host tree through one path and
// cannot reach it through this one. An agent that wants to change the
// environment edits .john/Dockerfile inside the sandbox like any other file,
// and it arrives as a diff to review.
func InitEnvironment(workdir string) (string, error) {
	dir := filepath.Join(workdir, EnvDir)
	path := filepath.Join(dir, "Dockerfile")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists", EnvFile)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(StarterEnvironment()), 0o644); err != nil {
		return "", err
	}
	gitignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gitignore); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(gitignore, []byte(EnvGitignore), 0o644); err != nil {
			return "", err
		}
	}
	return EnvFile, nil
}
