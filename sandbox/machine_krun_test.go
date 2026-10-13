//go:build linux && (amd64 || arm64)

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The test binary is also the machine.
//
// The Linux machine starts a second copy of whatever executable john is, as
// `john __machine`, and runs the virtual machine there — see machine_krun.go.
// Under `go test` that executable is this test binary, so this is where it
// answers to the same argument. Without it every test that boots a real
// machine would start a child that ran the test suite again.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__machine" {
		if err := RunMachineMonitor(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "john:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The sandbox on the machine john actually ships on Linux.
//
// It runs where this computer can start a virtual machine — which means
// /dev/kvm readable by this user and libkrun installed — so `go test` on a
// machine with neither is not enough:
//
//	make guest-image
//	gunzip -kf build/guest/x86_64/kernel.gz
//	JOHN_GUEST_KERNEL=build/guest/x86_64/kernel \
//	JOHN_GUEST_INITRAMFS=build/guest/x86_64/initramfs.cpio.gz \
//	  go test ./sandbox -run 'RealMachine|EverySandbox' -v
//
// The gunzip is not a detail: `make guest-image` writes only kernel.gz, and
// the variable wants the kernel. `make test-machine` does the whole of this
// and is what to run when you do not want to think about it.
//
// libkrun has to be 1.17 or later and built with BLK=1 — its block device is
// behind a Cargo feature that is off by default, and without it a sandbox has
// no disk. There is no Debian or Ubuntu package at the time of writing;
// Fedora has one, and the readme's Requirements has the build for the rest.
// 1.19.5 passes these on real KVM (26 Sep 2026).
//
// WSL 2 on Windows 11 is a working host for all of this: /dev/kvm and nested
// virtualization are there by default. Build the guest image with its OUTPUT
// on the WSL filesystem rather than under /mnt/c — the initramfs has symlinks
// and mode bits in it, and DrvFs is not the place for either.
//
// A machine that cannot start one skips rather than fails, the same way a Mac
// with an unsigned binary does and the same way any machine with no guest
// image does: the alternative is a test suite that is red on every Linux where
// somebody has not installed a virtual machine monitor.
func platformHarnessOrNil(t *testing.T) (hypervisor, string) {
	t.Helper()
	// The console normally goes with the test's temporary directory. Naming a
	// file keeps it, which is what you want the second time a boot fails.
	console := os.Getenv("JOHN_GUEST_CONSOLE")
	if console == "" {
		console = filepath.Join(t.TempDir(), "console.log")
	}
	m := &krun{
		Kernel:    os.Getenv("JOHN_GUEST_KERNEL"),
		Initramfs: os.Getenv("JOHN_GUEST_INITRAMFS"),
		Console:   console,
	}
	if err := m.Available(); err != nil {
		t.Logf("skipping the kvm machine: %v", err)
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
// shipping path on Linux end to end: libkrun, KVM, vsock, and the kernel and
// initramfs out of the binary itself.
func platformHarnessBare(t *testing.T) (hypervisor, string) {
	t.Helper()
	console := filepath.Join(t.TempDir(), "console.log")
	m := &krun{Console: console}
	if m.Available() != nil {
		return nil, ""
	}
	return m, console
}
