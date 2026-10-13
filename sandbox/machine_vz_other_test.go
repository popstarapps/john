//go:build (!darwin || !cgo) && !windows && (!linux || (!amd64 && !arm64))

package sandbox

import "testing"

// Everywhere but a cgo macOS build, a Windows one, and a Linux one on a
// processor the guest image is built for, there is no platform machine to add
// to the set; the qemu harness is what covers a real kernel there.
func platformHarnessOrNil(*testing.T) (hypervisor, string) { return nil, "" }

func platformHarnessBare(*testing.T) (hypervisor, string) { return nil, "" }
