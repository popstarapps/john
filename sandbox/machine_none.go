//go:build !linux && !darwin && !windows

package sandbox

import "context"

// Everywhere else there is no hypervisor to speak of, and saying so is more
// useful than guessing at one.
type noHypervisor struct{}

func platformHypervisor() hypervisor { return &noHypervisor{} }

func (n *noHypervisor) Name() string { return "none" }

func (n *noHypervisor) Available() error { return errNoMachine("this platform") }

func (n *noHypervisor) Boot(context.Context, bootConfig) (machine, error) {
	return nil, errNoMachine("this platform")
}
