//go:build !linux || john_guest || (!amd64 && !arm64)

package sandbox

import "errors"

// Linux is the only platform whose machine needs a process of its own.
// Virtualization.framework and Hyper-V both give john a virtual machine it can
// hold a handle to and keep working alongside, so there is nothing for a
// second process to do there.
func runMachineMonitor([]string) error {
	return errors.New("sandbox: the sandbox's machine does not run in a process of its own on this platform")
}
