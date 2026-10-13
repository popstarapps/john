//go:build darwin && cgo

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// The sandbox on the machine john actually ships on macOS.
//
// It runs where this Mac can start a virtual machine — which means hardware
// virtualization and a binary signed with com.apple.security.virtualization,
// so `go test` alone is not enough:
//
//	make guest-image
//	go test -c -o /tmp/sandbox.test ./sandbox
//	codesign --entitlements sandbox/machine_vz.plist -f -s - /tmp/sandbox.test
//	JOHN_GUEST_KERNEL=build/guest/aarch64/kernel \
//	JOHN_GUEST_INITRAMFS=build/guest/aarch64/initramfs.cpio.gz \
//	  /tmp/sandbox.test -test.run 'RealMachine|EverySandbox'
//
// An unsigned binary skips rather than fails, the same way a machine with no
// guest image does: the alternative is a test suite that is red on every Mac
// where somebody forgot a step that has nothing to do with the sandbox.
func platformHarnessOrNil(t *testing.T) (hypervisor, string) {
	t.Helper()
	// The console normally goes with the test's temporary directory. Naming a
	// file keeps it, which is what you want the second time a boot fails.
	console := os.Getenv("JOHN_GUEST_CONSOLE")
	if console == "" {
		console = filepath.Join(t.TempDir(), "console.log")
	}
	m := &vz{
		Kernel:    os.Getenv("JOHN_GUEST_KERNEL"),
		Initramfs: os.Getenv("JOHN_GUEST_INITRAMFS"),
		Console:   console,
	}
	if m.Available() != nil {
		return nil, ""
	}
	// An image either handed to it or carried by this build.
	if (m.Kernel == "" || m.Initramfs == "") && guestFS == nil {
		return nil, ""
	}
	return m, console
}

// platformHarnessBare is the platform's machine with no image handed to it, so
// that a test can check the binary boots the one it carries. It is the
// shipping path on macOS end to end: cgo, the entitlement, and the kernel and
// initramfs out of the binary itself.
func platformHarnessBare(t *testing.T) (hypervisor, string) {
	t.Helper()
	console := filepath.Join(t.TempDir(), "console.log")
	m := &vz{Console: console}
	if m.Available() != nil {
		return nil, ""
	}
	return m, console
}
