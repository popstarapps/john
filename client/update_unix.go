//go:build !windows

package client

import (
	"os"
	"syscall"
)

// replaceBinary moves the downloaded build over the running one. Unix lets a
// running program's file be replaced underneath it: the process keeps the
// inode it started from, and the rename is atomic, so there is never a moment
// where the path holds half a binary.
func replaceBinary(exe, downloaded string) error {
	return os.Rename(downloaded, exe)
}

// relaunch hands this process over to the new binary. execve replaces the
// image in place, keeping the pid, the terminal and the file descriptors — so
// from the developer's side john simply carries on, rather than a second john
// appearing underneath the first.
func relaunch(exe string, args []string) (int, error) {
	err := syscall.Exec(exe, args, os.Environ())
	// Exec only returns when it failed.
	return 1, err
}

// sweepAfterUpdate has nothing to do on Unix: replaceBinary leaves nothing
// behind.
func sweepAfterUpdate() {}
