//go:build windows

package sandbox

import (
	"os"

	"golang.org/x/sys/windows"
)

// takeLock claims the sandbox for this process. As on Unix the lock lives on
// the open handle, so Windows drops it when the process goes.
func takeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
	if err != nil {
		f.Close()
		return nil, errSandboxBusy
	}
	return f, nil
}

func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	var overlapped windows.Overlapped
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
	f.Close()
}
