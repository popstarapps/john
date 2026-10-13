//go:build !linux

package sandbox

import (
	"errors"
	"os/exec"
)

// A process limit is a cgroup, and a cgroup is Linux. This exists so the
// client — which is not Linux — compiles the package that describes the guest.
// The only machine that runs the agent off Linux is the loopback one, which
// isolates nothing and limits nothing.
type processLimit struct{}

func newProcessLimit(n int) (*processLimit, error) {
	if n <= 0 {
		return nil, nil
	}
	return nil, errors.New("a process limit can only be applied inside the Linux sandbox")
}

func (p *processLimit) apply(*exec.Cmd) {}
func (p *processLimit) close()          {}
