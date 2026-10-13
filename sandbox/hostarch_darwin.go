//go:build darwin

package sandbox

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// hostArch is the processor this machine actually has.
//
// On macOS the question is only interesting one way round: an arm64 build
// never runs on an Intel Mac, but an amd64 build runs happily on Apple Silicon
// under Rosetta and reports amd64 for itself the whole time. The kernel will
// say so if asked — sysctl.proc_translated is 1 in a translated process — and
// that is the one case worth catching, because the sandbox is a virtual
// machine and Rosetta does not translate a guest.
func hostArch() (string, error) {
	translated, err := unix.SysctlUint32("sysctl.proc_translated")
	if err != nil {
		// Older kernels do not have the key at all, which means no Rosetta.
		return runtime.GOARCH, nil
	}
	if translated == 1 {
		return "arm64", nil
	}
	return runtime.GOARCH, nil
}
