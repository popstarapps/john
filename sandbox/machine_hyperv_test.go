//go:build windows

package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The sandbox on the machine john actually ships on Windows.
//
// It runs where this computer can start a virtual machine — which means the
// Virtual Machine Platform feature turned on, hardware virtualization enabled
// in the firmware, and a token allowed to create a compute system, so `go
// test` from an ordinary terminal is not enough. With the tag the image comes
// out of the binary, so no JOHN_GUEST_* variable is needed:
//
//	wsl -d Ubuntu --cd . -- make guest-image
//	go test -c -tags embed_guest -o build\sandbox.test.exe .\sandbox
//	:: elevated, or signed in since being added to Hyper-V Administrators:
//	:: a token's groups are fixed when the logon session is made, so the
//	:: membership does not arrive at the moment it is granted
//	cd sandbox
//	..\build\sandbox.test.exe -test.v -test.run "RealMachine|EverySandbox"
//
// A machine that cannot start one skips rather than fails, the same way a Mac
// with an unsigned binary does and the same way any machine with no guest
// image does: the alternative is a test suite that is red on every Windows
// where somebody forgot a step that has nothing to do with the sandbox.
func platformHarnessOrNil(t *testing.T) (hypervisor, string) {
	t.Helper()
	// The console normally goes with the test's temporary directory. Naming a
	// file keeps it, which is what you want the second time a boot fails.
	console := os.Getenv("JOHN_GUEST_CONSOLE")
	if console == "" {
		console = filepath.Join(t.TempDir(), "console.log")
	}
	m := &hyperv{
		Kernel:    os.Getenv("JOHN_GUEST_KERNEL"),
		Initramfs: os.Getenv("JOHN_GUEST_INITRAMFS"),
		Console:   console,
	}
	if err := m.Available(); err != nil {
		t.Logf("skipping the hyper-v machine: %v", err)
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
// shipping path on Windows end to end: the Host Compute System API, hv_sock,
// and the kernel and initramfs out of the binary itself.
func platformHarnessBare(t *testing.T) (hypervisor, string) {
	t.Helper()
	console := filepath.Join(t.TempDir(), "console.log")
	m := &hyperv{Console: console}
	if m.Available() != nil {
		return nil, ""
	}
	return m, console
}

// The machine's real-time clock is UTC, which is what the Linux kernel reads
// it as. Without UseUtc Hyper-V keeps the host's local time there, and the
// guest booted at 14:20 "UTC" on a host where it was 12:20Z.
func TestTheHyperVMachineKeepsItsClockInUTC(t *testing.T) {
	raw, err := hcsDoc(hcsBoot{kernel: "kernel", initramfs: "initramfs", state: "state.vhdx"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		VirtualMachine struct {
			Chipset map[string]json.RawMessage
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if got := string(doc.VirtualMachine.Chipset["UseUtc"]); got != "true" {
		t.Fatalf("Chipset.UseUtc = %q, want true: %s", got, raw)
	}
}
