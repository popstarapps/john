//go:build unix

package sandbox

import (
	"os"
	"syscall"
)

// takeLock claims the sandbox for this process, or reports that somebody else
// has it. The lock is held by the open file, so the operating system releases
// it if john is killed — a sandbox must never be left unusable because the
// process that had it did not get to tidy up.
func takeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errSandboxBusy
	}
	return f, nil
}

func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}
