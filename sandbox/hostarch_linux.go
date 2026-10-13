//go:build linux

package sandbox

import "golang.org/x/sys/unix"

// hostArch is the processor this machine actually has, which is not always the
// one this binary was built for.
func hostArch() (string, error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", err
	}
	return normalizeArch(nullTerminated(u.Machine[:])), nil
}
