package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/popstarapps/john/policy"
)

// Fake is an in-memory Sandbox over a real host directory: reads fall through
// to the host, writes land in an overlay map, and Diff/Apply/Reset behave
// exactly like the real sandbox. It is a test double: the parts of the client
// that need no machine run on it without booting one.
type Fake struct {
	workdir string

	// ExecFn scripts command execution. Nil means every command succeeds
	// with empty output.
	ExecFn func(cmd, cwd string) ExecResult
	// ReadFn, when set, runs at the start of every Read, which then carries
	// on as usual — a hook for holding reads, which are the tool calls that
	// still run together.
	ReadFn func(path string)
	// Wedged makes every command fail with ErrStoppedAnswering, as a
	// sandbox that stopped answering does, until Restart clears it.
	Wedged bool

	mu       sync.Mutex
	built    bool
	overlay  map[string][]byte // path → content; nil slice means deleted
	order    []string          // insertion order, so diffs render stably
	res      Resources
	prot     policy.Protections
	stateDir string
	stopped  bool
	restarts int
}

// NewFake creates a fake sandbox over a host directory.
func NewFake(workdir string) *Fake {
	return &Fake{workdir: workdir, overlay: map[string][]byte{}, res: DefaultResources()}
}

// Protect installs the workspace-protection policies. The fake enforces them
// exactly as the real sandbox does — a protection that held in one
// implementation and not in another would be no protection at all.
func (f *Fake) Protect(p policy.Protections) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prot = p
	return nil
}

func (f *Fake) protections() policy.Protections {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prot
}

func (f *Fake) Status(ctx context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := StateNone
	if f.built {
		st = StateReady
	}
	return Status{State: st, Image: "fake", Resources: f.res}, nil
}

func (f *Fake) Build(ctx context.Context, progress func(Progress)) error {
	if progress != nil {
		progress(Progress{Step: 1, Steps: 1, Label: "preparing sandbox"})
	}
	f.mu.Lock()
	f.built = true
	f.mu.Unlock()
	return nil
}

func (f *Fake) Read(ctx context.Context, path string, maxBytes int) (Content, error) {
	if f.ReadFn != nil {
		f.ReadFn(path)
	}
	p, err := f.follow(path, true)
	if err != nil {
		return Content{}, err
	}
	if err := f.protections().CheckRead(p); err != nil {
		return Content{}, err
	}
	f.mu.Lock()
	data, inOverlay := f.overlay[p]
	f.mu.Unlock()
	if inOverlay {
		if data == nil {
			return Content{}, fmt.Errorf("sandbox: %s: no such file", p)
		}
	} else {
		host, err := hostPathFor(f.workdir, p, false)
		if err != nil {
			return Content{}, err
		}
		if data, err = os.ReadFile(host); err != nil {
			return Content{}, err
		}
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return Content{Data: data[:maxBytes], Truncated: true}, nil
	}
	return Content{Data: data}, nil
}

func (f *Fake) Write(ctx context.Context, path string, content []byte) error {
	p, err := f.follow(path, true)
	if err != nil {
		return err
	}
	if err := f.protections().CheckWrite(p); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, seen := f.overlay[p]; !seen {
		f.order = append(f.order, p)
	}
	f.overlay[p] = append([]byte(nil), content...)
	return nil
}

func (f *Fake) Delete(ctx context.Context, path string) error {
	// The link itself is what a delete removes, so the last element is the
	// one thing not followed — the same rule the real sandbox keeps.
	p, err := f.follow(path, false)
	if err != nil {
		return err
	}
	if err := f.protections().CheckDelete(p); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, seen := f.overlay[p]; !seen {
		f.order = append(f.order, p)
	}
	f.overlay[p] = nil
	return nil
}

// follow resolves the symbolic links in a path against the host tree, so that
// a tool call on the fake sees the same workspace a tool call on a machine
// does. It is the fake's whole answer to links: reads and writes land on what
// a link names, and a link out of the workspace is refused rather than
// followed, because a test double that could be walked out of would be
// proving the wrong thing.
func (f *Fake) follow(path string, followFinal bool) (string, error) {
	p, err := CleanPath(path)
	if err != nil {
		return "", err
	}
	host, err := hostPathFor(f.workdir, p, followFinal)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(f.workdir, host)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func (f *Fake) Exec(ctx context.Context, cmd, cwd string, timeout time.Duration) (ExecResult, error) {
	f.mu.Lock()
	wedged := f.Wedged
	f.mu.Unlock()
	if wedged {
		return ExecResult{}, fmt.Errorf("%w (it did not answer a ping)", ErrStoppedAnswering)
	}
	if f.ExecFn != nil {
		return f.ExecFn(cmd, cwd), nil
	}
	return ExecResult{}, nil
}

func (f *Fake) Diff(ctx context.Context) ([]FileDiff, error) {
	f.mu.Lock()
	paths := append([]string(nil), f.order...)
	overlay := make(map[string][]byte, len(f.overlay))
	for k, v := range f.overlay {
		overlay[k] = v
	}
	prot := f.prot
	f.mu.Unlock()

	var out []FileDiff
	for _, p := range paths {
		after := overlay[p]
		host, err := hostPathFor(f.workdir, p, false)
		if err != nil {
			out = append(out, FileDiff{Path: p, Deleted: after == nil, Protected: heldSymlinked})
			continue
		}
		before, err := readHostRegular(host)
		hostHas := err == nil
		switch {
		case after == nil && !hostHas:
			continue // deleted a file the host never had
		case after == nil:
			out = append(out, FileDiff{Path: p, Before: before, Deleted: true, Binary: looksBinary(before)})
		case !hostHas:
			out = append(out, FileDiff{Path: p, After: after, Created: true, Binary: looksBinary(after)})
		case string(before) == string(after):
			continue // drifted back to identical
		default:
			out = append(out, FileDiff{Path: p, Before: before, After: after,
				Binary: looksBinary(before) || looksBinary(after)})
		}
		out[len(out)-1].Protected = out[len(out)-1].protectedBy(prot)
	}
	return out, nil
}

func (f *Fake) Apply(ctx context.Context, paths []string) (int, error) {
	diffs, err := f.Diff(ctx)
	if err != nil {
		return 0, err
	}
	selected := map[string]bool{}
	for _, p := range paths {
		cp, err := CleanPath(p)
		if err != nil {
			return 0, err
		}
		selected[cp] = true
	}
	applied := 0
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range diffs {
		if paths != nil && !selected[d.Path] {
			continue
		}
		if d.Protected != "" {
			continue // the policies refuse this one; see VM.Apply
		}
		host, err := hostPathFor(f.workdir, d.Path, false)
		if err != nil {
			return applied, err
		}
		if d.Deleted {
			if err := os.Remove(host); err != nil {
				return applied, err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
				return applied, err
			}
			if err := os.WriteFile(host, d.After, 0o644); err != nil {
				return applied, err
			}
		}
		delete(f.overlay, d.Path)
		applied++
	}
	f.order = pruneOrder(f.order, f.overlay)
	return applied, nil
}

func (f *Fake) Reset(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overlay = map[string][]byte{}
	f.order = nil
	f.built = false
	f.res = DefaultResources()
	return nil
}

// StateDir is a directory of this double's own, made on demand. A test double
// must not write into the developer's ~/.john: `go test ./client` would leave
// a sandbox directory behind for every temporary workspace it made, which is
// how that directory becomes something nobody can read.
func (f *Fake) StateDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateDir == "" {
		dir, err := os.MkdirTemp("", "john-fake-")
		if err != nil {
			return filepath.Join(os.TempDir(), "john-fake")
		}
		f.stateDir = dir
	}
	return f.stateDir
}

func (f *Fake) Resources() Resources {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.res
}

func (f *Fake) SetResources(r Resources) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.res = r
	return nil
}

// Pending counts unapplied changes, for the status line.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for range f.overlay {
		n++
	}
	return n
}

func pruneOrder(order []string, overlay map[string][]byte) []string {
	out := order[:0]
	for _, p := range order {
		if _, ok := overlay[p]; ok {
			out = append(out, p)
		}
	}
	return out
}

// Stop records that the session stopped the sandbox, as it must for a real
// machine to settle its disk; Stopped reports it.
func (f *Fake) Stop(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

// Stopped reports whether Stop was called.
func (f *Fake) Stopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// Changed is the paths Diff offers.
func (f *Fake) Changed(ctx context.Context) ([]string, error) {
	diffs, err := f.Diff(ctx)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, d := range diffs {
		if d.Protected == "" {
			paths = append(paths, d.Path)
		}
	}
	return paths, nil
}

// Restart starts the sandbox again, as a VM does after it stopped answering.
func (f *Fake) Restart(ctx context.Context, progress func(Progress)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Wedged = false
	f.restarts++
	return nil
}

// Restarts is how many times Restart was called.
func (f *Fake) Restarts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts
}
