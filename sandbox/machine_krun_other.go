//go:build linux && (john_guest || (!amd64 && !arm64))

package sandbox

import "context"

// A Linux build with no machine in it, for the two builds that have none.
//
// The agent is one of them. `john __guest` runs inside the sandbox and starts
// nothing, so it has no use for a hypervisor — and more than no use: the
// machine reaches libkrun through the dynamic loader, which makes the binary
// that carries it dynamically linked, and the initramfs the agent is unpacked
// into has no loader at all. An agent built with the machine in it does not
// run: `/bin/john: not found`, for a binary that is there. So the john_guest
// tag drops it, the way it drops the backend and the console.
//
// The other is a processor john builds no guest image for. The machine itself
// would work anywhere KVM does — libkrun runs on more than two architectures —
// but the sandbox boots an image, the image is built for aarch64 and x86-64
// (see sandbox/guest/mkimage.sh), and a machine with nothing to boot is not a
// machine. Saying so is more useful than a build that does not compile.
type krun struct{}

func platformHypervisor() hypervisor { return &krun{} }

func (k *krun) Name() string { return "kvm" }

func (k *krun) Available() error { return errNoMachine("kvm on this processor") }

func (k *krun) Boot(context.Context, bootConfig) (machine, error) {
	return nil, errNoMachine("kvm on this processor")
}
