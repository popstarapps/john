//go:build linux

package sandbox

import (
	"context"
	"errors"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/popstarapps/john/policy"
)

// The union, mounted. Everything above this file decides what the workspace
// looks like; this file is the part that makes an ordinary process see it —
// `go build`, `npm install`, `grep -r`, a test runner, an editor invoked from
// a shell. None of them know there are two layers, which is the whole point:
// the sandbox is not a special place where some tools work.
//
// Only the guest runs this. It is Linux-only because the sandbox is Linux on
// every platform john supports — a macOS developer's sandbox is a Linux
// virtual machine, and the agent inside it is a Linux binary.
type fuseMount struct {
	server *fuse.Server
}

func mountUnion(u *union, mountpoint string) (*fuseMount, error) {
	root := &unionNode{u: u, rel: ""}
	server, err := fs.Mount(mountpoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName: "john",
			Name:   "john",
			// The sandbox runs as root in its own machine and the workspace
			// is its own; there is no second user to keep out.
			AllowOther: false,
			// Mount with the syscall rather than by running fusermount. The
			// agent is root inside its own machine, so the setuid helper buys
			// nothing — and a guest image that had to carry it would be
			// carrying a setuid binary into the sandbox for no reason. It
			// falls back to the helper where the syscall is refused, which is
			// what happens under the loopback machine on a developer's
			// laptop, where the agent is an ordinary process.
			DirectMount: true,
			Debug:       false,
		},
		// The lower layer is live: the host tree can change under us at any
		// moment, so nothing about it may be cached across calls.
		EntryTimeout: nil,
		AttrTimeout:  nil,
	})
	if err != nil {
		return nil, err
	}
	return &fuseMount{server: server}, nil
}

func (m *fuseMount) Close() error {
	if m.server == nil {
		return nil
	}
	return m.server.Unmount()
}

// unionNode is one path in the merged workspace. Nodes are addressed by
// workspace-relative path rather than by inode: the lower layer is a remote
// tree that can change between calls, so a path is the only durable identity
// either side agrees on.
type unionNode struct {
	fs.Inode
	u   *union
	rel string
}

var (
	_ fs.NodeLookuper   = (*unionNode)(nil)
	_ fs.NodeGetattrer  = (*unionNode)(nil)
	_ fs.NodeReaddirer  = (*unionNode)(nil)
	_ fs.NodeOpener     = (*unionNode)(nil)
	_ fs.NodeCreater    = (*unionNode)(nil)
	_ fs.NodeMkdirer    = (*unionNode)(nil)
	_ fs.NodeUnlinker   = (*unionNode)(nil)
	_ fs.NodeRmdirer    = (*unionNode)(nil)
	_ fs.NodeRenamer    = (*unionNode)(nil)
	_ fs.NodeSetattrer  = (*unionNode)(nil)
	_ fs.NodeSymlinker  = (*unionNode)(nil)
	_ fs.NodeReadlinker = (*unionNode)(nil)
	_ fs.NodeReader     = (*unionNode)(nil)
	_ fs.NodeWriter     = (*unionNode)(nil)
)

func (n *unionNode) child(name string) string { return join(n.rel, name) }

// errno turns an error from the union into the number the kernel expects. A
// workspace-protection refusal becomes EACCES, which is what tooling inside
// the sandbox understands: a read-only file is a read-only file, and the
// build system reports it as one rather than crashing on something novel.
func errno(err error) syscall.Errno {
	var pe *policy.ProtectedError
	var en syscall.Errno
	switch {
	case err == nil:
		return 0
	case errors.As(err, &en):
		// The union said exactly which error it is: ENOTEMPTY, EISDIR and
		// the rest mean something to the program asking, and EIO does not.
		return en
	case errors.As(err, &pe):
		if pe.Mode == policy.ModeExclude {
			return syscall.ENOENT
		}
		return syscall.EACCES
	case errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, os.ErrExist):
		return syscall.EEXIST
	case errors.Is(err, os.ErrPermission):
		return syscall.EACCES
	}
	return syscall.EIO
}

// fileType is the kind bit the kernel is told about an entry. A symbolic link
// is one because its target says so: nothing else in the union carries one.
func fileType(st lowerStat) uint32 {
	switch {
	case st.Dir:
		return syscall.S_IFDIR
	case st.Link != "":
		return syscall.S_IFLNK
	}
	return syscall.S_IFREG
}

func fillAttr(out *fuse.Attr, st lowerStat) {
	out.Size = uint64(st.Size)
	out.Mode = st.Perm | fileType(st)
	out.Mtime = uint64(st.ModUnix)
	out.Atime = out.Mtime
	out.Ctime = out.Mtime
	out.Nlink = 1
}

func (n *unionNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.child(name)
	st, err := n.u.Stat(ctx, rel)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	child := n.NewInode(ctx, &unionNode{u: n.u, rel: rel}, fs.StableAttr{Mode: fileType(st)})
	return child, 0
}

func (n *unionNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	st, err := n.u.Stat(ctx, n.rel)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, st)
	return 0
}

func (n *unionNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.u.ReadDir(ctx, n.rel)
	if err != nil {
		return nil, errno(err)
	}
	list := make([]fuse.DirEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, fuse.DirEntry{Name: e.Name, Mode: fileType(e)})
	}
	return fs.NewListDirStream(list), 0
}

func (n *unionNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	// Writing opens are checked here rather than at the first write, so a
	// protected file refuses to open for writing the way any read-only file
	// does and the tool reports it at the point a person expects.
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_TRUNC) != 0 {
		if err := n.u.protections().CheckWrite(n.rel); err != nil {
			return nil, 0, errno(err)
		}
		if flags&syscall.O_TRUNC != 0 {
			if err := n.u.Truncate(ctx, n.rel, 0); err != nil {
				return nil, 0, errno(err)
			}
		}
	}
	// No handle: every call carries the path, and the lower layer is remote
	// and live, so a cached descriptor would be a stale answer waiting to
	// happen.
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (n *unionNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, err := n.u.ReadAt(ctx, n.rel, off, len(dest))
	if err != nil {
		return nil, errno(err)
	}
	return fuse.ReadResultData(data), 0
}

func (n *unionNode) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	written, err := n.u.WriteAt(ctx, n.rel, off, data)
	if err != nil {
		return 0, errno(err)
	}
	return uint32(written), 0
}

func (n *unionNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	rel := n.child(name)
	if err := n.u.Create(ctx, rel, os.FileMode(mode)&os.ModePerm); err != nil {
		return nil, nil, 0, errno(err)
	}
	st, err := n.u.Stat(ctx, rel)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	fillAttr(&out.Attr, st)
	child := n.NewInode(ctx, &unionNode{u: n.u, rel: rel}, fs.StableAttr{Mode: syscall.S_IFREG})
	return child, nil, fuse.FOPEN_DIRECT_IO, 0
}

func (n *unionNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.child(name)
	if err := n.u.Mkdir(ctx, rel, os.FileMode(mode)&os.ModePerm); err != nil {
		return nil, errno(err)
	}
	st, err := n.u.Stat(ctx, rel)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	child := n.NewInode(ctx, &unionNode{u: n.u, rel: rel}, fs.StableAttr{Mode: syscall.S_IFDIR})
	return child, 0
}

func (n *unionNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return errno(n.u.Remove(ctx, n.child(name), false))
}

func (n *unionNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errno(n.u.Remove(ctx, n.child(name), true))
}

func (n *unionNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	target, ok := newParent.(*unionNode)
	if !ok {
		return syscall.EXDEV
	}
	return errno(n.u.Rename(ctx, n.child(name), target.child(newName)))
}

// Symlink and Readlink are what make a checkout that uses links work inside
// the sandbox, in both directions: a link the developer already has is walked
// like any other, and `npm install` — which does almost nothing else — can
// create the ones it needs.
//
// The kernel does the walking. It asks for the target and then looks up what
// it resolves to, so a link is never a path the union hands out: a relative
// one stays inside the mounted workspace, and an absolute one resolves in the
// sandbox's own filesystem, which is the sandbox's and not the developer's.
func (n *unionNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := n.child(name)
	if err := n.u.Symlink(ctx, target, rel); err != nil {
		return nil, errno(err)
	}
	st, err := n.u.Stat(ctx, rel)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, st)
	child := n.NewInode(ctx, &unionNode{u: n.u, rel: rel}, fs.StableAttr{Mode: syscall.S_IFLNK})
	return child, 0
}

func (n *unionNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := n.u.Readlink(ctx, n.rel)
	if err != nil {
		return nil, errno(err)
	}
	return []byte(target), 0
}

func (n *unionNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if size, ok := in.GetSize(); ok {
		if err := n.u.Truncate(ctx, n.rel, int64(size)); err != nil {
			return errno(err)
		}
	}
	if mode, ok := in.GetMode(); ok {
		if err := n.u.Chmod(ctx, n.rel, os.FileMode(mode)&os.ModePerm); err != nil {
			return errno(err)
		}
	}
	st, err := n.u.Stat(ctx, n.rel)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, st)
	return 0
}
