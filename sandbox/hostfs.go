package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/popstarapps/john/policy"
)

// hostFS serves the lower layer of the sandbox's workspace: a live, filtered,
// read-only view of the host working directory, answered over the guest link.
//
// Read-only is not a check here, it is the absence of a code path. There is no
// write, no create, no unlink and no chmod operation in this file, so the
// invariant the package doc states — that nothing but Apply modifies the host
// working directory — is true because the code to violate it does not exist.
// The promise is kept while the tree is mounted live, rather than by keeping
// the tree out of the sandbox altogether.
//
// Excluded paths are not served. Not refused, not hidden behind a permission
// bit: a stat of an excluded path answers "no such file", exactly as if it
// were not on the disk, and its parent's listing does not name it. That makes
// exclusion enforceable against a root shell inside the sandbox, which no
// guest-side rule can be, and it makes it live — an administrator tightening a
// policy mid-session takes a path away from a machine that is already running.
//
// Symlinks cross as symlinks: the target is handed over as the string it is
// and is never resolved here. A checkout that uses them — a pnpm workspace, a
// node_modules/.bin, a monorepo linking one package into another — is a normal
// thing to hand an agent, and serving a link as "no such file" makes a project
// look broken in a way nothing in it explains.
//
// Resolving a link is the guest's job, and doing it here would be the way out
// of the workspace: the operating system would follow `link/passwd` through a
// link the developer's tree happens to contain and read whatever it names, and
// it would walk around an excluded path just as easily. So this file never
// follows one — resolve refuses a path whose parents lead through a link, and
// the guest asks again for what the link resolves to.
//
// Devices, sockets and fifos are still not served: only regular files,
// directories and symlinks cross.
type hostFS struct {
	root string

	mu   sync.RWMutex
	prot policy.Protections
}

func newHostFS(root string, prot policy.Protections) *hostFS {
	return &hostFS{root: root, prot: prot}
}

func (h *hostFS) setProtections(p policy.Protections) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prot = p
}

func (h *hostFS) protections() policy.Protections {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.prot
}

// resolve turns a workspace-relative path into a host path, refusing anything
// that escapes the workspace and anything a policy excludes. The empty path is
// the workspace root itself, which is always visible.
func (h *hostFS) resolve(rel string) (string, error) {
	if rel == "" || rel == "." {
		return h.root, nil
	}
	p, err := CleanPath(rel)
	if err != nil {
		return "", err
	}
	if h.protections().Excluded(p) {
		return "", fmt.Errorf("%s: %w", p, fs.ErrNotExist)
	}
	// Walk the parents rather than joining the whole path in one go: joining
	// leaves the following to the operating system, which follows links, and
	// a single link in the developer's tree would then be a path to anything
	// on the host. The last element may be a link — serving it is the point —
	// but nothing above it may be.
	host := h.root
	segs := strings.Split(p, "/")
	for _, seg := range segs[:len(segs)-1] {
		host = filepath.Join(host, seg)
		info, err := os.Lstat(host)
		if err != nil {
			return "", err
		}
		if info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return "", fmt.Errorf("%s: leads through a symbolic link: %w", p, fs.ErrNotExist)
		}
	}
	return filepath.Join(host, segs[len(segs)-1]), nil
}

// linkTarget puts a link target read from the host in the guest's form. On
// Windows the host answers with its own separators and, for an absolute
// target, a volume: `..\lib\cli.js`, `C:\Users\bob`, `\\server\share`. Read
// with the guest's rules, the first is one strange filename rather than a
// climb, and the others are not absolute at all, so the check that refuses a
// link out of the workspace would wave them through. Slashed, and rooted when
// they carry a volume, they mean to that check what they mean to Windows.
// Everywhere else the target is already in the guest's form.
func linkTarget(target string) string {
	if filepath.Separator == '/' {
		return target
	}
	slashed := filepath.ToSlash(target)
	if filepath.VolumeName(target) != "" && !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return slashed
}

// lowerStat is what one entry looks like to the guest. Perm carries the host's
// permission bits so that an executable script stays executable inside the
// sandbox; ReadOnly is set when a policy says the guest may not change it, so
// the union can present it without a write bit without having to ask again.
//
// Link is the target of a symbolic link, and a non-empty Link is what makes
// this entry one — a link always has a target, so there is no state to keep
// beside it and no entry that is a link without saying what it points at.
type lowerStat struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Perm     uint32 `json:"perm"`
	Dir      bool   `json:"dir"`
	ModUnix  int64  `json:"mtime"`
	ReadOnly bool   `json:"ro,omitempty"`
	Link     string `json:"link,omitempty"`
}

func (h *hostFS) stat(rel string) (lowerStat, error) {
	p, err := h.resolve(rel)
	if err != nil {
		return lowerStat{}, err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return lowerStat{}, err
	}
	st, ok := h.describe(rel, p, info)
	if !ok {
		return lowerStat{}, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	return st, nil
}

// describe converts a host entry, reporting false for the kinds the sandbox
// does not serve.
func (h *hostFS) describe(rel, host string, info fs.FileInfo) (lowerStat, bool) {
	st := lowerStat{
		Name:     info.Name(),
		Size:     info.Size(),
		Perm:     uint32(info.Mode().Perm()),
		Dir:      info.IsDir(),
		ModUnix:  info.ModTime().Unix(),
		ReadOnly: h.protections().CheckWrite(rel) != nil,
	}
	switch {
	case info.IsDir():
	case info.Mode().IsRegular():
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(host)
		if err != nil || target == "" {
			return lowerStat{}, false
		}
		target = linkTarget(target)
		// A link's own bits are not a thing anyone reads on Linux, and its
		// size is the length of what it points at. Both are stated here so
		// the guest never has to ask the host a second question about it.
		st.Link, st.Size, st.Perm = target, int64(len(target)), 0o777
	default:
		return lowerStat{}, false
	}
	return st, true
}

func (h *hostFS) readdir(rel string) ([]lowerStat, error) {
	dir, err := h.resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prot := h.protections()
	out := make([]lowerStat, 0, len(entries))
	for _, e := range entries {
		child := e.Name()
		if rel != "" && rel != "." {
			child = rel + "/" + e.Name()
		}
		if prot.Excluded(child) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // vanished between listing and stat; it is simply gone
		}
		if st, ok := h.describe(child, filepath.Join(dir, e.Name()), info); ok {
			out = append(out, st)
		}
	}
	return out, nil
}

// readAt serves a range of a file. The guest reads in chunks, so a large file
// never has to fit in a frame on either side.
func (h *hostFS) readAt(rel string, off int64, n int) ([]byte, error) {
	p, err := h.resolve(rel)
	if err != nil {
		return nil, err
	}
	if n < 0 || n > maxFrame {
		return nil, fmt.Errorf("sandbox: read of %d bytes", n)
	}
	// Lstat before opening: open follows a link, so a read of a path that is
	// one would answer with whatever it points at, anywhere on the host. The
	// guest reads a link by being told its target and asking again. Nothing
	// the sandbox can do changes the developer's tree between these two
	// calls — Apply is the only thing that writes it, and it is not the
	// sandbox — so the window between them is not one the guest can use.
	if info, err := os.Lstat(p); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:got], nil
}

// serve answers one lower-layer request. The guest is the client here: it is
// the side that dials, and the host only ever answers.
func (h *hostFS) serve(ctx context.Context, op string, body json.RawMessage, _ [][]byte) (any, [][]byte, error) {
	switch op {
	case "stat":
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		st, err := h.stat(req.Path)
		return st, nil, err

	case "readdir":
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		entries, err := h.readdir(req.Path)
		if err != nil {
			return nil, nil, err
		}
		return struct {
			Entries []lowerStat `json:"entries"`
		}{entries}, nil, nil

	case "read":
		var req struct {
			Path string `json:"path"`
			Off  int64  `json:"off"`
			N    int    `json:"n"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, nil, err
		}
		data, err := h.readAt(req.Path, req.Off, req.N)
		if err != nil {
			return nil, nil, err
		}
		return nil, [][]byte{data}, nil

	default:
		return nil, nil, fmt.Errorf("sandbox: unknown lower-layer op %q", op)
	}
}
