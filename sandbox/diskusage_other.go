//go:build !unix

package sandbox

import "os"

// diskUsage falls back to the file's length where the platform does not say
// how much of it is actually written.
func diskUsage(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
