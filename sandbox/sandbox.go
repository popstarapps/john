// Package sandbox is where the agent's tool calls actually execute. The
// Sandbox interface is the shape the client programs against; vm.go is the
// implementation — the per-directory virtual machine of the readme — and
// fake.go is the in-memory test double.
//
// Two properties are the point, and every implementation must keep them:
//
//   - The host working directory is never modified by anything except Apply.
//     Writes land in the sandbox's copy-on-write layer, and reach the host
//     only when the developer reviews the diff and applies it.
//   - Paths are workspace-relative and may not escape. A path that tries is
//     rejected here, before any implementation sees it.
//   - Workspace-protection policies are enforced by the sandbox itself, on
//     every path in and every path out, so no caller can bypass one by
//     forgetting to check.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/popstarapps/john/policy"
)

// State is where a sandbox is in its life.
type State string

const (
	StateNone     State = "none"     // nothing exists yet
	StateBuilding State = "building" // image build or seed in progress
	StateReady    State = "ready"    // container up, workspace seeded
)

// Status describes a sandbox.
type Status struct {
	State     State
	Image     string // the image the container runs
	Resources Resources
}

// Progress is one step of a build, for the client to render.
type Progress struct {
	Step   int
	Steps  int
	Label  string
	Detail string
}

// Resources are the sandbox limits. They apply when the sandbox next starts;
// a running machine does not grow underneath the work inside it.
type Resources struct {
	CPUs      int `json:"cpus"`
	MemoryGB  int `json:"memory_gb"`
	DiskGB    int `json:"disk_gb"`
	Processes int `json:"processes"`
}

// DefaultResources are meant to be right for every project without anyone
// touching them.
func DefaultResources() Resources {
	return Resources{CPUs: 4, MemoryGB: 8, DiskGB: 100, Processes: 1024}
}

// ExecResult is what a command produced.
type ExecResult struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool
	Duration  time.Duration
}

// Content is a file read from the sandbox.
type Content struct {
	Data      []byte
	Truncated bool
}

// FileDiff is one file's difference between the sandbox and the host tree,
// as it stands right now. Before is the host's current content, After the
// sandbox's; a created file has no Before, a deleted one no After.
//
// Protected names the reason this change is held back from the host, and is
// empty for a change that may be applied. Usually that reason is a
// workspace-protection mode; it is also how a path that reaches the
// developer's tree through a symbolic link out of the workspace is reported.
// Either way the change is shown rather than hidden — that the agent tried to
// rewrite a protected path is something the developer should see — and Apply
// refuses it.
type FileDiff struct {
	Path      string
	Before    []byte
	After     []byte
	Created   bool
	Deleted   bool
	Binary    bool
	Protected string
}

// protectedBy is the mode holding this change back, "" when it may be
// applied. A deletion is judged as a deletion; everything else as a write.
func (f FileDiff) protectedBy(prot policy.Protections) string {
	var err error
	if f.Deleted {
		err = prot.CheckDelete(f.Path)
	} else {
		err = prot.CheckWrite(f.Path)
	}
	var pe *policy.ProtectedError
	if errors.As(err, &pe) {
		return pe.Mode
	}
	return ""
}

// Sandbox is the closed surface tools execute against.
type Sandbox interface {
	Status(ctx context.Context) (Status, error)
	// Build brings the sandbox to ready: image built or pulled, container
	// created, workspace seeded. Idempotent; progress may be nil.
	Build(ctx context.Context, progress func(Progress)) error
	// Protect installs the workspace-protection policies this sandbox
	// enforces: excluded paths are never seeded and cannot be read or
	// written, read-only paths cannot be changed, and no-delete paths cannot
	// be removed. Enforcement lives here rather than in the caller so that no
	// tool call, and no path through Apply, can reach the host by forgetting
	// to ask.
	Protect(policy.Protections) error
	Read(ctx context.Context, path string, maxBytes int) (Content, error)
	Write(ctx context.Context, path string, content []byte) error
	Delete(ctx context.Context, path string) error
	Exec(ctx context.Context, cmd, cwd string, timeout time.Duration) (ExecResult, error)
	// Diff is everything the sandbox has changed relative to the host tree.
	Diff(ctx context.Context) ([]FileDiff, error)
	// Changed is the paths Diff would offer to apply — changed, and not held
	// back by a protection — without fetching what is in them. It is cheap
	// enough to ask after every tool call.
	Changed(ctx context.Context) ([]string, error)
	// Apply writes the selected paths (nil means all of them) to the host
	// working directory and returns how many files changed. This is the only
	// code path anywhere that writes the host tree.
	Apply(ctx context.Context, paths []string) (int, error)
	// Reset discards the sandbox: container, overlay, unapplied work.
	Reset(ctx context.Context) error
	Resources() Resources
	SetResources(Resources) error
	// StateDir is where this sandbox keeps what outlives it — the resource
	// limits, the workspace protections, the proxy's certificate authority
	// and, for a machine, its disks. It is answered by the sandbox rather
	// than worked out again by the caller so that there is one place a
	// workspace's sandbox lives, and everything belonging to it lands there
	// rather than somewhere that only usually agrees.
	StateDir() string
}

// CleanPath validates a workspace-relative path and returns it in canonical
// slash form. Absolute paths and anything escaping the workspace are
// rejected before an implementation ever sees them.
func CleanPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("sandbox: empty path")
	}
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("sandbox: path %q escapes the workspace", path)
	}
	return filepath.ToSlash(filepath.Clean(path)), nil
}

// looksBinary reports whether content should be diffed as bytes, not lines.
func looksBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

// truncateOutput caps command output at max bytes, keeping the head and the
// tail with a marker between them — the loop wants the result, and the result
// of a long build lives at both ends.
func truncateOutput(data []byte, max int) ([]byte, bool) {
	if max <= 0 || len(data) <= max {
		return data, false
	}
	marker := []byte("\n… [output truncated] …\n")
	head := max * 6 / 10
	tail := max - head - len(marker)
	if tail < 0 {
		return data[:max], true
	}
	out := make([]byte, 0, max)
	out = append(out, data[:head]...)
	out = append(out, marker...)
	out = append(out, data[len(data)-tail:]...)
	return out, true
}
