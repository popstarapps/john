package sandbox

import "fmt"

// errNoMachine is what a build with no machine for this platform answers: a
// processor john builds no guest image for, or an operating system with no
// hypervisor john knows how to drive. It is a distinct, honest error rather
// than a boot that fails in some interesting way.
func errNoMachine(name string) error {
	return fmt.Errorf("sandbox: john has no virtual machine for %s; -sandbox loopback runs without isolation, which is not a sandbox and says so", name)
}
