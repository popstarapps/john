package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/popstarapps/john/policy"
)

// union is the sandbox's workspace: the host working directory as a live,
// read-only lower layer, and everything the agent writes in an upper layer on
// the sandbox's own disk. It is the copy-on-write mount the readme describes,
// and it is where the two directions the developer asked for are decided —
// a host edit is visible inside the sandbox immediately, and nothing the
// sandbox writes is visible on the host until Apply.
//
// The union is implemented here rather than handed to kernel overlayfs
// deliberately. overlayfs documents a lower directory that changes while
// mounted as undefined behaviour, and a lower directory that changes while
// mounted is precisely the requirement. Owning the merge means the semantics
// are ours to define and ours to test, and it puts the enumeration of changes
// — which is the diff the developer reviews — in one place with the rules
// that decide what may reach the host.
//
// The upper layer keeps written files and deletion markers in two parallel
// trees rather than mixing markers in beside the files:
//
//	<upper>/f/…   what the sandbox wrote, path for path
//	<upper>/w/…   what the sandbox deleted, path for path
//
// overlayfs puts its whiteouts next to the files as `.wh.`-prefixed names,
// which needs an escaping scheme so that a real file cannot be mistaken for a
// marker. Two trees need none: no name in the workspace can ever be read as a
// deletion, so an agent cannot make a file disappear from the diff by creating
// one with the right name. Enumerating the diff is then two walks and no
// parsing.
type union struct {
	lower lowerFS
	upper string

	mu   sync.RWMutex
	prot policy.Protections
}

// lowerFS is the read-only layer under the union. In the sandbox it is the
// host working directory reached over the guest link; in tests it is a hostFS
// called directly, which is the same code answering the same questions.
type lowerFS interface {
	Stat(ctx context.Context, rel string) (lowerStat, error)
	ReadDir(ctx context.Context, rel string) ([]lowerStat, error)
	ReadAt(ctx context.Context, rel string, off int64, n int) ([]byte, error)
}

func newUnion(lower lowerFS, upper string, prot policy.Protections) (*union, error) {
	for _, sub := range []string{"f", "w"} {
		if err := os.MkdirAll(filepath.Join(upper, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &union{lower: lower, upper: upper, prot: prot}, nil
}

func (u *union) setProtections(p policy.Protections) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.prot = p
}

func (u *union) protections() policy.Protections {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.prot
}

func (u *union) filePath(rel string) string {
	return filepath.Join(u.upper, "f", filepath.FromSlash(rel))
}

func (u *union) whitePath(rel string) string {
	return filepath.Join(u.upper, "w", filepath.FromSlash(rel))
}

// hidden reports whether a deletion marker covers this path — at the path
// itself or at any directory above it, since deleting a directory deletes
// what is under it. A marker hides the lower layer only: something written
// into the upper afterwards is visible again, which is what makes
// delete-then-recreate behave the way a filesystem should.
//
// Only a regular file is a marker. The marker tree has directories in it for
// the same reason any tree does — to hold what is below them — and reading one
// as a deletion would hide a whole subtree the moment a single file in it was
// removed.
func (u *union) hidden(rel string) bool {
	if rel == "" || rel == "." {
		return false
	}
	for p := rel; ; {
		if info, err := os.Lstat(u.whitePath(p)); err == nil && !info.IsDir() {
			return true
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return false
		}
		p = p[:i]
	}
}

// resolve answers what is at a path: the upper layer if it has anything
// there, otherwise the lower layer unless a marker hides it.
func (u *union) resolve(ctx context.Context, rel string) (st lowerStat, fromUpper bool, err error) {
	if rel == "" || rel == "." {
		return lowerStat{Name: ".", Dir: true, Perm: 0o755}, false, nil
	}
	if info, lerr := os.Lstat(u.filePath(rel)); lerr == nil {
		return u.describeUpper(rel, path.Base(rel), info), true, nil
	}
	if u.hidden(rel) {
		return lowerStat{}, false, fmt.Errorf("%s: %w", rel, fs.ErrNotExist)
	}
	st, err = u.lower.Stat(ctx, rel)
	return st, false, err
}

// describeUpper turns an entry the sandbox wrote into the same shape the
// lower layer answers in, so that neither the mount nor a tool call can tell
// which layer a path came from without asking.
func (u *union) describeUpper(rel, name string, info os.FileInfo) lowerStat {
	st := lowerStat{
		Name:    name,
		Size:    info.Size(),
		Perm:    uint32(info.Mode().Perm()),
		Dir:     info.IsDir(),
		ModUnix: info.ModTime().Unix(),
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// Readlink can only fail here if the entry stopped being a link
		// between the two calls, and nothing but this sandbox writes the
		// upper layer. An unreadable link is reported as an empty file
		// rather than as a link with no target.
		if target, err := os.Readlink(u.filePath(rel)); err == nil && target != "" {
			target = linkTarget(target)
			st.Link, st.Size, st.Perm = target, int64(len(target)), 0o777
		}
	}
	return st
}

// Stat is resolve for callers that do not care which layer answered.
func (u *union) Stat(ctx context.Context, rel string) (lowerStat, error) {
	st, _, err := u.resolve(ctx, rel)
	return st, err
}

// ReadDir merges the two layers. The upper wins on name collisions, markers
// remove lower entries, and a marker on the directory itself means the lower
// contributes nothing at all.
func (u *union) ReadDir(ctx context.Context, rel string) ([]lowerStat, error) {
	byName := map[string]lowerStat{}

	if !u.hidden(rel) {
		lowerEntries, err := u.lower.ReadDir(ctx, rel)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, e := range lowerEntries {
			if u.hidden(join(rel, e.Name)) {
				continue
			}
			byName[e.Name] = e
		}
	}

	entries, err := os.ReadDir(u.filePath(rel))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		byName[e.Name()] = u.describeUpper(join(rel, e.Name()), e.Name(), info)
	}

	if len(byName) == 0 {
		// Nothing on either side: say so only if the directory is not there,
		// so that an empty directory reads as empty rather than as missing.
		if _, _, err := u.resolve(ctx, rel); err != nil {
			return nil, err
		}
	}
	out := make([]lowerStat, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReadAt reads through whichever layer holds the file.
func (u *union) ReadAt(ctx context.Context, rel string, off int64, n int) ([]byte, error) {
	_, fromUpper, err := u.resolve(ctx, rel)
	if err != nil {
		return nil, err
	}
	if !fromUpper {
		return u.lower.ReadAt(ctx, rel, off, n)
	}
	f, err := os.Open(u.filePath(rel))
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

// ReadFile reads a whole file through the union, in chunks so that neither
// side has to hold a large file in one frame.
func (u *union) ReadFile(ctx context.Context, rel string) ([]byte, error) {
	st, fromUpper, err := u.resolve(ctx, rel)
	if err != nil {
		return nil, err
	}
	if st.Dir {
		return nil, fmt.Errorf("sandbox: %s is a directory", rel)
	}
	if st.Link != "" {
		return nil, fmt.Errorf("sandbox: %s is a symbolic link", rel)
	}
	if fromUpper {
		return os.ReadFile(u.filePath(rel))
	}
	const chunk = 1 << 20
	var out []byte
	for off := int64(0); ; {
		part, err := u.lower.ReadAt(ctx, rel, off, chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
		if len(part) < chunk {
			return out, nil
		}
		off += int64(len(part))
	}
}

// copyUp brings a file that lives only in the lower layer into the upper one,
// so it can be written to. A file already in the upper is left alone; a file
// in neither is created empty.
func (u *union) copyUp(ctx context.Context, rel string) error {
	if _, err := os.Lstat(u.filePath(rel)); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(u.filePath(rel)), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	var data []byte
	if !u.hidden(rel) {
		if st, err := u.lower.Stat(ctx, rel); err == nil && !st.Dir {
			if st.Link != "" {
				// A link is brought up as a link. Copying it up as its
				// contents would silently turn it into a file.
				return os.Symlink(st.Link, u.filePath(rel))
			}
			if data, err = u.ReadFile(ctx, rel); err != nil {
				return err
			}
			// A read-only path is copied up without its write bit, so that
			// ordinary tooling inside the sandbox meets the same wall the
			// edit tool does. The bit is a courtesy; Apply is the control.
			perm = os.FileMode(st.Perm)
			if st.ReadOnly {
				perm &^= 0o222
			}
		}
	}
	return os.WriteFile(u.filePath(rel), data, perm)
}

// WriteFile replaces a file's contents, refusing a path a policy protects.
func (u *union) WriteFile(ctx context.Context, rel string, content []byte) error {
	if err := u.protections().CheckWrite(rel); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.filePath(rel)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(u.filePath(rel), content, 0o644); err != nil {
		return err
	}
	return u.unhide(rel)
}

// WriteAt writes into a file at an offset, copying it up first. This is the
// path a shell command inside the sandbox takes; WriteFile is the path the
// edit tool takes.
func (u *union) WriteAt(ctx context.Context, rel string, off int64, data []byte) (int, error) {
	if err := u.protections().CheckWrite(rel); err != nil {
		return 0, err
	}
	if err := u.copyUp(ctx, rel); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(u.filePath(rel), os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := f.WriteAt(data, off)
	if err != nil {
		return n, err
	}
	return n, u.unhide(rel)
}

// Truncate resizes a file, copying it up first.
func (u *union) Truncate(ctx context.Context, rel string, size int64) error {
	if err := u.protections().CheckWrite(rel); err != nil {
		return err
	}
	if err := u.copyUp(ctx, rel); err != nil {
		return err
	}
	if err := os.Truncate(u.filePath(rel), size); err != nil {
		return err
	}
	return u.unhide(rel)
}

// Create makes an empty file, refusing a protected path.
func (u *union) Create(ctx context.Context, rel string, perm os.FileMode) error {
	if err := u.protections().CheckWrite(rel); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.filePath(rel)), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(u.filePath(rel), os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	f.Close()
	return u.unhide(rel)
}

// Mkdir makes a directory in the upper layer.
func (u *union) Mkdir(ctx context.Context, rel string, perm os.FileMode) error {
	if err := u.protections().CheckWrite(rel); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.filePath(rel)), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(u.filePath(rel), perm); err != nil && !os.IsExist(err) {
		return err
	}
	return u.unhide(rel)
}

// Remove deletes a path. A path the upper layer holds is removed there; a
// path the lower layer still provides is covered with a marker, since the
// lower layer is the developer's tree and is never written.
func (u *union) Remove(ctx context.Context, rel string, dir bool) error {
	if err := u.protections().CheckDelete(rel); err != nil {
		return err
	}
	st, _, err := u.resolve(ctx, rel)
	if err != nil {
		return err
	}
	if dir && !st.Dir {
		return fmt.Errorf("%s: %w", rel, syscall.ENOTDIR)
	}
	if !dir && st.Dir {
		return fmt.Errorf("%s: %w", rel, syscall.EISDIR)
	}
	if dir {
		// A directory is only removable when it is empty in the merged view,
		// which is what a caller running rmdir expects to be checked — and
		// expects to be told about as ENOTEMPTY, not as an I/O error: Node's
		// rmSync tries rmdir first and empties the directory only on
		// ENOTEMPTY, so any other answer fails every `vite build` after the
		// first.
		entries, err := u.ReadDir(ctx, rel)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("%s: %w", rel, syscall.ENOTEMPTY)
		}
	}
	if err := os.RemoveAll(u.filePath(rel)); err != nil {
		return err
	}
	// Only mark it deleted if the lower layer would otherwise still show it.
	if !u.hidden(rel) {
		if _, err := u.lower.Stat(ctx, rel); err == nil {
			return u.hide(rel)
		}
	}
	return nil
}

// Rename moves a path within the workspace. It is a copy and a delete rather
// than a rename whenever the source lives in the lower layer, because the
// lower layer cannot be changed.
func (u *union) Rename(ctx context.Context, from, to string) error {
	if err := u.protections().CheckDelete(from); err != nil {
		return err
	}
	if err := u.protections().CheckWrite(to); err != nil {
		return err
	}
	st, fromUpper, err := u.resolve(ctx, from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.filePath(to)), 0o755); err != nil {
		return err
	}
	switch {
	case fromUpper:
		if err := os.RemoveAll(u.filePath(to)); err != nil {
			return err
		}
		if err := os.Rename(u.filePath(from), u.filePath(to)); err != nil {
			return err
		}
	case st.Dir:
		if err := u.copyTree(ctx, from, to); err != nil {
			return err
		}
	case st.Link != "":
		if err := os.Symlink(st.Link, u.filePath(to)); err != nil {
			return err
		}
	default:
		data, err := u.ReadFile(ctx, from)
		if err != nil {
			return err
		}
		if err := os.WriteFile(u.filePath(to), data, os.FileMode(st.Perm)); err != nil {
			return err
		}
	}
	if err := u.unhide(to); err != nil {
		return err
	}
	if !fromUpper || u.stillInLower(ctx, from) {
		return u.hide(from)
	}
	return nil
}

func (u *union) stillInLower(ctx context.Context, rel string) bool {
	if u.hidden(rel) {
		return false
	}
	_, err := u.lower.Stat(ctx, rel)
	return err == nil
}

// copyTree materialises a lower directory into the upper layer under a new
// name, which is what renaming a directory out of the lower layer means.
func (u *union) copyTree(ctx context.Context, from, to string) error {
	entries, err := u.ReadDir(ctx, from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(u.filePath(to), 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		src, dst := join(from, e.Name), join(to, e.Name)
		if e.Dir {
			if err := u.copyTree(ctx, src, dst); err != nil {
				return err
			}
			continue
		}
		if e.Link != "" {
			// A link is moved as the link it is. Copying what it points at
			// would turn one file into two that no longer follow each other.
			if err := os.Symlink(e.Link, u.filePath(dst)); err != nil {
				return err
			}
			continue
		}
		data, err := u.ReadFile(ctx, src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(u.filePath(dst), data, os.FileMode(e.Perm)); err != nil {
			return err
		}
	}
	return nil
}

// Chmod changes the permission bits of a file in the upper layer. A file that
// only exists in the lower layer is copied up first — the bits belong to the
// sandbox's copy, never to the developer's.
func (u *union) Chmod(ctx context.Context, rel string, perm os.FileMode) error {
	st, fromUpper, err := u.resolve(ctx, rel)
	if err != nil {
		return err
	}
	if !fromUpper {
		if st.Dir {
			if err := u.Mkdir(ctx, rel, perm); err != nil {
				return err
			}
		} else if err := u.copyUp(ctx, rel); err != nil {
			return err
		}
	}
	return os.Chmod(u.filePath(rel), perm)
}

// Symlink creates a symbolic link in the upper layer. The target is stored
// exactly as it was given and is never resolved here: a link is resolved by
// whoever walks it, which inside the sandbox is the guest's own kernel walking
// the mounted workspace. Storing it resolved would make `npm install` write
// links that mean something different from the ones it asked for.
func (u *union) Symlink(ctx context.Context, target, rel string) error {
	if err := u.protections().CheckWrite(rel); err != nil {
		return err
	}
	if target == "" {
		return fmt.Errorf("sandbox: %s: empty symlink target", rel)
	}
	if err := os.MkdirAll(filepath.Dir(u.filePath(rel)), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(target, u.filePath(rel)); err != nil {
		return err
	}
	return u.unhide(rel)
}

// Readlink answers what a link points at, from whichever layer holds it.
func (u *union) Readlink(ctx context.Context, rel string) (string, error) {
	st, _, err := u.resolve(ctx, rel)
	if err != nil {
		return "", err
	}
	if st.Link == "" {
		return "", fmt.Errorf("sandbox: %s is not a symbolic link: %w", rel, syscall.EINVAL)
	}
	return st.Link, nil
}

// maxSymlinkHops is how many links follow will walk before giving up, the
// same limit Linux uses. A cycle is the reason for a limit and the reason the
// error says so.
const maxSymlinkHops = 40

// follow resolves the symbolic links in a path, the way the kernel does for
// an ordinary process. The mount does not need it — the guest kernel walks
// the workspace itself, one component at a time — but a tool call arrives as
// a whole path and nothing else would resolve it, so a read of `link/file`
// would answer "no such file" for something an `ls` in the same sandbox
// shows.
//
// A link is followed only within the workspace. An absolute target, or a
// relative one that climbs out through the top, is refused by name rather
// than followed: outside the workspace the union has nothing to answer with,
// and quietly reading the guest's own filesystem instead would make a tool
// call and a shell command in the same sandbox disagree about what a path is.
//
// followFinal is what separates the calls that act on the file a link points
// at — reading it, writing it — from the one that acts on the link itself:
// deleting `link` removes the link, not what it names.
func (u *union) follow(ctx context.Context, rel string, followFinal bool) (string, error) {
	p, err := CleanPath(rel)
	if err != nil {
		return "", err
	}
	segs := strings.Split(p, "/")
	at, hops := "", 0
	for i := 0; i < len(segs); i++ {
		at = join(at, segs[i])
		if i == len(segs)-1 && !followFinal {
			break
		}
		for {
			st, _, err := u.resolve(ctx, at)
			if err != nil || st.Link == "" {
				// Not a link, or not there at all: what it is is the real
				// operation's answer to give, not this walk's.
				break
			}
			if hops++; hops > maxSymlinkHops {
				return "", fmt.Errorf("sandbox: %s: %w", rel, syscall.ELOOP)
			}
			next, err := resolveLink(at, st.Link)
			if err != nil {
				return "", err
			}
			at = next
		}
	}
	return at, nil
}

// resolveLink applies one link's target to the path it was found at, refusing
// a target that leaves the workspace.
func resolveLink(at, target string) (string, error) {
	if strings.HasPrefix(target, "/") {
		return "", fmt.Errorf("sandbox: %s points at %s, which is outside the workspace", at, target)
	}
	next := path.Clean(path.Join(path.Dir(at), target))
	if next == ".." || strings.HasPrefix(next, "../") {
		return "", fmt.Errorf("sandbox: %s points at %s, which is outside the workspace", at, target)
	}
	if next == "." {
		return "", fmt.Errorf("sandbox: %s points at the workspace itself", at)
	}
	return next, nil
}

// hide records that the sandbox deleted a path the developer's tree still
// has. Marking a directory subsumes every marker inside it: the directory is
// gone, so what was noted about its contents is no longer a separate fact.
func (u *union) hide(rel string) error {
	if err := os.MkdirAll(filepath.Dir(u.whitePath(rel)), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(u.whitePath(rel)); err == nil && info.IsDir() {
		if err := os.RemoveAll(u.whitePath(rel)); err != nil {
			return err
		}
	}
	return os.WriteFile(u.whitePath(rel), nil, 0o644)
}

// unhide clears the marker at exactly this path, if there is one. A marker
// on a directory above it is left alone: writing a/b back does not undelete
// everything else that was under a.
func (u *union) unhide(rel string) error {
	info, err := os.Lstat(u.whitePath(rel))
	if err != nil || info.IsDir() {
		return nil
	}
	if err := os.Remove(u.whitePath(rel)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// settle hands a path back to the developer's tree once it has been applied
// there: the sandbox's copy goes, so what the sandbox sees at that path is the
// host's file again. Kept, the copy hid every later edit the developer made
// to the file, and the next apply wrote the sandbox's stale version over it.
//
// Only a copy still holding what was applied is dropped — hash is that
// content's — because the sandbox may have written the file again since the
// diff was read, and then its copy is new work. An applied deletion clears
// the marker that hid the path, if the sandbox has not made it again.
func (u *union) settle(rel, hash string, deleted bool) error {
	p := u.filePath(rel)
	info, err := os.Lstat(p)
	if deleted {
		if err == nil {
			return nil // made again since: that is the sandbox's now
		}
		return u.unhide(rel)
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != hash {
		return nil
	}
	return os.Remove(p)
}

// change is one entry of what the sandbox has done to the workspace, as the
// sandbox sees it. The host decides what it means: a hash equal to the host's
// is not a change at all, and a deletion of something the host does not have
// is nothing either.
type change struct {
	Path    string `json:"path"`
	Deleted bool   `json:"deleted,omitempty"`
	Hash    string `json:"hash,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// Changes enumerates the upper layer. This is the whole of what the sandbox
// has changed, and it costs two walks of the sandbox's own writes rather than
// a hash sweep of the entire project on both sides — the diff is the upper
// layer, which is the point of building the union rather than borrowing one.
//
// Only regular files are reported. A symlink or a device node the sandbox
// created is real inside the sandbox and stays there; nothing but regular
// files has ever been applied to the host tree, and a diff offering something
// Apply would refuse is a diff that lies.
func (u *union) Changes() ([]change, error) {
	var out []change

	root := filepath.Join(u.upper, "f")
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
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
		sum := sha256.Sum256(data)
		out = append(out, change{Path: filepath.ToSlash(rel),
			Hash: hex.EncodeToString(sum[:]), Size: info.Size()})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	wroot := filepath.Join(u.upper, "w")
	err = filepath.WalkDir(wroot, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, err := filepath.Rel(wroot, p)
		if err != nil {
			return err
		}
		out = append(out, change{Path: filepath.ToSlash(rel), Deleted: true})
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func join(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	return dir + "/" + name
}
