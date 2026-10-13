package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// What `john sandbox` needs: the sandboxes on this machine, described well
// enough to decide what to keep.
//
// None of it starts a machine. A developer asking what is on their disk should
// not have to boot four virtual machines to find out, and the answers that
// matter — where it came from, how big it is, when it was last used and what
// it was doing — are all on the host. The one thing that is not is how much a
// running sandbox has changed since its last summary: that lives inside a
// filesystem image the host cannot read, so a running sandbox is reported as
// running with whatever was last written down.

// Workspace is one project's sandboxes.
type Workspace struct {
	// Path is the working directory these back. Empty for a sandbox made
	// before john recorded it.
	Path string
	// Gone reports that the working directory is no longer there — the usual
	// reason a sandbox is worth reclaiming.
	Gone bool
	// Dir is the sandbox directory on this machine.
	Dir string
	// Environment is how much the shared environment images weigh.
	Environment int64
	// Sandboxes are the lines of work in it.
	Sandboxes []SandboxInfo
}

// Size is everything this workspace holds.
func (w Workspace) Size() int64 {
	total := w.Environment
	for _, s := range w.Sandboxes {
		total += s.Size
	}
	return total
}

// SandboxInfo is one sandbox, as `john sandbox` reports it.
type SandboxInfo struct {
	ID string
	// Running reports that a john has it open right now.
	Running bool
	// LastUsed, Changes and Note are what its last session left behind.
	// Changes is what was recorded when it was last stopped, so for a running
	// sandbox it is the count as of then rather than now.
	LastUsed time.Time
	Changes  int
	Note     string
	// Size is what its disk actually occupies, which for a sparse image is
	// what has been written rather than what it could hold.
	Size int64
	Dir  string
}

// Describe is the one-line summary a developer reads.
func (s SandboxInfo) Describe() string {
	return instance{LastUsed: s.LastUsed, Changes: s.Changes, Note: s.Note}.Describe()
}

// Sandboxes lists what is on this machine. With a workdir it is that
// project's; with "" it is every project's, largest first.
func Sandboxes(workdir string) ([]Workspace, error) {
	if workdir != "" {
		dir, err := defaultStateDir(WorkspaceKey(workdir))
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(string(dir)); err != nil {
			return nil, nil
		}
		return []Workspace{describeWorkspace(dir)}, nil
	}

	root, err := sandboxesRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Workspace
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, describeWorkspace(stateDir(filepath.Join(root, e.Name()))))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Size() > out[b].Size() })
	return out, nil
}

// sandboxesRoot is where every workspace's sandbox lives: under JOHN_HOME
// when it is set, like the credentials and the standalone database, so that
// a john run with a home of its own — a test, an end-to-end run — leaves
// nothing behind in the real one.
func sandboxesRoot() (string, error) {
	if h := os.Getenv("JOHN_HOME"); h != "" {
		return filepath.Join(h, "sandboxes"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".john", "sandboxes"), nil
}

func describeWorkspace(dir stateDir) Workspace {
	w := Workspace{Path: dir.workspace(), Dir: string(dir)}
	if w.Path != "" {
		if _, err := os.Stat(w.Path); err != nil {
			w.Gone = true
		}
	}
	// The environment images, which are the workspace's rather than any one
	// sandbox's — one number for the thing they all share.
	if entries, err := os.ReadDir(string(dir)); err == nil {
		for _, e := range entries {
			if isEnvDisk(e.Name()) {
				w.Environment += diskUsage(dir.path(e.Name()))
			}
		}
	}
	for _, in := range dir.instances() {
		w.Sandboxes = append(w.Sandboxes, SandboxInfo{
			ID: in.ID, Running: in.Busy, LastUsed: in.LastUsed,
			Changes: in.Changes, Note: in.Note,
			Size: treeSize(string(in.Dir)), Dir: string(in.Dir),
		})
	}
	return w
}

// RemoveSandbox discards one sandbox. It refuses one that is running, and one
// holding work that was never applied unless it is told to go ahead — that
// work is the only thing in a sandbox which cannot be made again.
func RemoveSandbox(workdir, id string, force bool) (int64, error) {
	dir, err := defaultStateDir(WorkspaceKey(workdir))
	if err != nil {
		return 0, err
	}
	for _, in := range dir.instances() {
		if in.ID != id {
			continue
		}
		if in.Busy {
			return 0, fmt.Errorf("sandbox %s is in use by a running john", id)
		}
		if in.Changes > 0 && !force {
			return 0, fmt.Errorf("sandbox %s has %d file(s) it never applied to your working "+
				"directory; removing it discards them", id, in.Changes)
		}
		size := treeSize(string(in.Dir))
		return size, os.RemoveAll(string(in.Dir))
	}
	return 0, fmt.Errorf("this workspace has no sandbox %q", id)
}

// PruneOptions says what is worth reclaiming.
type PruneOptions struct {
	// OlderThan keeps anything used more recently. Zero keeps nothing on
	// those grounds.
	OlderThan time.Duration
	// GoneOnly limits it to sandboxes whose working directory is no longer
	// there, which is the safest sweep there is.
	GoneOnly bool
	// WithChanges allows removing sandboxes holding unapplied work. Off by
	// default: that work is the one thing here that cannot be made again.
	WithChanges bool
	// DryRun reports what would go without removing it.
	DryRun bool
}

// Pruned is one sandbox a prune took, or would take.
type Pruned struct {
	Workspace string
	ID        string
	Size      int64
	Reason    string
}

// Prune reclaims dormant sandboxes. A running one is never touched, and
// neither is one holding unapplied work unless asked.
func Prune(opts PruneOptions) ([]Pruned, error) {
	spaces, err := Sandboxes("")
	if err != nil {
		return nil, err
	}
	var out []Pruned
	for _, w := range spaces {
		for _, s := range w.Sandboxes {
			reason := ""
			switch {
			case s.Running:
				continue
			case w.Gone:
				reason = "its working directory is gone"
			case opts.GoneOnly:
				continue
			case opts.OlderThan > 0 && !s.LastUsed.IsZero() && time.Since(s.LastUsed) > opts.OlderThan:
				reason = "last used " + humanSince(s.LastUsed)
			default:
				continue
			}
			if s.Changes > 0 && !opts.WithChanges {
				continue
			}
			p := Pruned{Workspace: w.Path, ID: s.ID, Size: s.Size, Reason: reason}
			if p.Workspace == "" {
				p.Workspace = w.Dir
			}
			if !opts.DryRun {
				if err := os.RemoveAll(s.Dir); err != nil {
					return out, err
				}
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// RebuildEnvironment throws the workspace's built environment away so the next
// start makes it again.
//
// It is the environment and nothing else: the sandboxes' own disks are
// untouched, so everything written and not yet applied survives. That is the
// difference between this and a reset, and it is the whole reason it exists —
// a toolchain that needs rebuilding should not cost an afternoon's work.
func RebuildEnvironment(workdir string) error {
	dir, err := defaultStateDir(WorkspaceKey(workdir))
	if err != nil {
		return err
	}
	return rebuildEnvironmentIn(dir)
}

func rebuildEnvironmentIn(dir stateDir) error {
	entries, err := os.ReadDir(string(dir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		// The image itself, and the note saying it was built. The image is
		// named for what this platform's machine will attach, which is not
		// the same everywhere — a rebuild that deleted only the raw one would
		// leave a Windows workspace quietly building on the old environment.
		name := e.Name()
		if !isEnvDisk(name) && !(strings.HasPrefix(name, "env-") && strings.HasSuffix(name, ".built")) {
			continue
		}
		if err := os.Remove(dir.path(name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// treeSize is what a directory actually occupies.
func treeSize(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		total += diskUsage(p)
		return nil
	})
	return total
}

// FormatSize is a byte count a person reads.
func FormatSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%dM", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%dK", n/(1<<10))
	case n == 0:
		return "--"
	}
	return fmt.Sprintf("%dB", n)
}
