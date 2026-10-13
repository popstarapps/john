//go:build unix

package sandbox

import (
	"os"
	"syscall"
)

// diskUsage is what a file occupies rather than how long it is.
//
// The sandbox's disks are sparse — a hundred-gigabyte image holding four
// hundred megabytes — so reporting their length would tell a developer they
// have terabytes of sandboxes and no way to find out otherwise.
func diskUsage(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return info.Size()
}
