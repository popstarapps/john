//go:build !linux

package sandbox

import "fmt"

// The agent runs inside the sandbox, and the sandbox is Linux on every
// platform john supports — a macOS developer's sandbox is a Linux virtual
// machine. This file exists so the client, which is not Linux, still compiles
// the package that describes the guest.
//
// It is reached only under the loopback machine, which runs the agent as an
// ordinary process on the developer's own operating system. That is not a
// sandbox and never was; what it loses here is the workspace mount, and so a
// shell. Everything else the agent does works, so this answers with a reason
// the caller can recognise rather than an error it can only report.

type fuseMount struct{}

func (m *fuseMount) Close() error { return nil }

func mountUnion(*union, string) (*fuseMount, error) {
	return nil, fmt.Errorf("%w: mounting the workspace needs FUSE, which is Linux; "+
		"the sandbox itself is a Linux virtual machine and has it", errNoWorkspaceMount)
}
