//go:build !windows

package sandbox

import "os"

// Every machine but Hyper-V attaches a sparse raw image, so that is what the
// sandbox's disks are here: a file that costs what has been written to it
// rather than what it could hold, which is what makes a hundred-gigabyte
// sandbox a limit rather than an allocation.
const diskExt = ".img"

// diskFormat is what to call that format to a tool that reads several.
const diskFormat = "raw"

func makeDisk(path string, want int64) error { return sparseDisk(path, want) }

// sparseDisk makes a disk image of at least want bytes, growing one that is
// already there and never shrinking it. Shrinking one out from under a sandbox
// that has written to it would lose the work the disk is kept for, and the
// filesystem on it would not survive being cut short either.
func sparseDisk(path string, want int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() < want {
		return f.Truncate(want)
	}
	return nil
}
