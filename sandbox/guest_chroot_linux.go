//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// mountEnvOverlay puts a writable layer of this sandbox's own over the shared
// environment, and is what makes sharing it safe.
//
// The environment is one disk for the whole workspace, because it is expensive
// and identical for everyone. But a session has every right to install
// something into it — that is what a sandbox is for — and what it installs
// must be its own rather than everybody's. So the shared image is the lower
// layer, this sandbox's disk holds the upper, and the two are merged: reads
// come from the environment, writes land here.
func mountEnvOverlay(l envLayout) error {
	if mounted(l.dir) {
		return nil
	}
	for _, d := range []string{l.dir, l.upper, l.work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	opts := "lowerdir=" + l.base + ",upperdir=" + l.upper + ",workdir=" + l.work
	if err := syscall.Mount("overlay", l.dir, "overlay", 0, opts); err != nil {
		return fmt.Errorf("sandbox: the environment could not be layered: %w", err)
	}
	return nil
}

// unmountIfMounted takes a filesystem down, so what is under it can be
// replaced. Nothing mounted is not an error: this is called on the way to
// removing a directory that may never have had anything on it.
func unmountIfMounted(path string) {
	if mounted(path) {
		syscall.Unmount(path, 0)
	}
}

// unmountEnvironment takes the environment down in the order it went up.
//
// The kernel filesystems a build and a session both need are mounted inside
// it, and a filesystem with something mounted under it does not unmount — so
// without this the environment disk stays busy, is never flushed, and the next
// sandbox finds a journal to replay on a disk it may only read.
func unmountEnvironment(l envLayout) {
	for _, root := range []string{l.dir, l.base} {
		for _, sub := range []string{"/tmp", "/dev/shm", "/dev/pts", "/dev", "/sys", "/proc"} {
			unmountIfMounted(filepath.Join(root, sub))
		}
	}
	unmountIfMounted(l.dir)
	unmountIfMounted(l.base)
}

// syncDisks flushes everything written to the sandbox's disk, so a machine
// that is powered off next loses nothing.
func syncDisks() error {
	syscall.Sync()
	return nil
}

// envSupported reports whether this is a place an environment can be built and
// entered at all, for an agent whose root is root.
//
// Linux is half the answer. The other half is that the agent has to own the
// filesystem it is building into: an environment is a root filesystem that
// gets mounted, chrooted into and installed with a package manager, and the
// only place that is a reasonable thing to do is inside a machine whose whole
// filesystem exists to be one. An empty root is exactly that claim — a real
// machine owns its filesystem and says so by leaving it empty, and the
// loopback machine, which is a process on the developer's own operating
// system, does not.
//
// So the loopback machine answers no here, on Linux as well as everywhere
// else, and the sandbox comes up without an environment and says why. Before
// this it answered yes on Linux and the build got as far as making
// directories on the developer's own root.
//
// Being root is the rest of it: mounting and chrooting are root operations,
// and a process that is not root cannot do either. Inside a machine the agent
// is pid 1 and root, so this costs a real machine nothing.
func envSupported(root string) bool { return root == "" && os.Geteuid() == 0 }

// setChroot makes a command run inside the environment's root filesystem.
func setChroot(c *exec.Cmd, root string) error {
	c.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
	return nil
}

// mountKernelFilesystems gives the environment the filesystems a package
// manager assumes exist. `apt-get` reads /proc for its own pid and writes
// /dev/null more often than it writes anything else; a build without them
// fails in ways that read as broken packages rather than a missing /proc.
//
// They are mounted once and left: a command run in the sandbox afterwards
// needs them for exactly the same reasons.
//
// keep is a path under root that must stay visible, and it is the workspace —
// which, with an environment installed, is mounted inside it at the host's own
// path. A project kept under /tmp would otherwise disappear the first time a
// command ran. The workspace goes up when the sandbox is built and this runs
// at the first command after it, so a tmpfs over <root>/tmp lands on top of a
// mount that is already there and hides it. What the command then meets is a
// chroot whose working directory does not exist, which the kernel reports
// against the program rather than the directory — `fork/exec /bin/sh: no such
// file or directory`, for a shell that is present and fine.
//
// Not mounting it costs the environment nothing it needed: the directory is
// still there, still writable, on the sandbox's own disk.
//
// /dev/pts and /dev/shm go on top of /dev, which is why they come after it.
// Without /dev/pts nothing can open a pseudo-terminal — `script`, `expect`,
// Python's pty, apt's own log ("Is /dev/pts mounted?"); without /dev/shm,
// programs that share memory between processes fail, Chrome among them. Each
// devpts mount is an instance of its own, and the /dev/ptmx beside it opens
// that one.
func mountKernelFilesystems(root, keep string) error {
	for _, m := range []struct{ fstype, target, data string }{
		{"proc", "/proc", ""},
		{"sysfs", "/sys", ""},
		{"devtmpfs", "/dev", ""},
		{"devpts", "/dev/pts", "mode=0620,ptmxmode=0666"},
		{"tmpfs", "/dev/shm", "mode=1777"},
		{"tmpfs", "/tmp", ""},
	} {
		target := filepath.Join(root, m.target)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
		if keep != "" && within(target, keep) {
			continue
		}
		if mounted(target) {
			continue
		}
		// A failure here is not fatal on its own — a build that does not need
		// /sys should not be stopped because the kernel would not give it.
		syscall.Mount(m.fstype, target, m.fstype, 0, m.data)
	}
	return nil
}

// mounted reports whether anything is mounted at path, so this is idempotent.
func mounted(path string) bool {
	var here, up syscall.Stat_t
	if err := syscall.Stat(path, &here); err != nil {
		return false
	}
	if err := syscall.Stat(filepath.Dir(path), &up); err != nil {
		return false
	}
	return here.Dev != up.Dev
}
