//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// The sandbox's process limit, from the inside.
//
// A developer sets it with /resources and john prints it back. Holding it is
// the agent's job, because the hypervisor hands out CPUs, memory and a disk
// and has nothing to say about a fork — so the number was carried all the way
// to the boot config and then applied by nobody, which is worse than not
// offering it.
//
// It is one cgroup shared by every command, so the limit is a property of the
// sandbox: it may have that many processes running, not that many per
// command. The agent itself stays outside it — its own threads are not the
// developer's budget, and an agent that cannot fork is an agent that cannot
// report why.
const cgroupMount = "/sys/fs/cgroup"

// processLimit is a cgroup that commands are started inside of.
type processLimit struct {
	dir string
	fd  int
}

// newProcessLimit prepares the cgroup, or reports why it could not.
//
// n of zero is no limit and no cgroup. A kernel with no cgroup2, or a machine
// where the agent is not the one that owns the filesystem, is an error the
// caller decides about: this is a resource guard rather than a boundary, and
// the machine's memory is the backstop either way.
func newProcessLimit(n int) (*processLimit, error) {
	if n <= 0 {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(cgroupMount, "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("no cgroup2 at %s: %w", cgroupMount, err)
	}
	// The pids controller has to be handed down before a child cgroup can use
	// it. The root cgroup is allowed to hold the agent and delegate at the
	// same time; every other cgroup is not, which is why commands go in a
	// child rather than beside the agent.
	if err := os.WriteFile(filepath.Join(cgroupMount, "cgroup.subtree_control"), []byte("+pids"), 0); err != nil {
		return nil, fmt.Errorf("the pids controller could not be delegated: %w", err)
	}
	dir := filepath.Join(cgroupMount, "john-commands")
	if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "pids.max"), []byte(strconv.Itoa(n)), 0); err != nil {
		return nil, fmt.Errorf("the process limit could not be set: %w", err)
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &processLimit{dir: dir, fd: fd}, nil
}

// apply starts the command inside the limit.
//
// The cgroup is chosen at clone time rather than by moving the process
// afterwards, because a process that is moved has already run: a command whose
// first act is to fork would be over the limit before anything applied to it.
func (p *processLimit) apply(c *exec.Cmd) {
	if p == nil {
		return
	}
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.UseCgroupFD = true
	c.SysProcAttr.CgroupFD = p.fd
}

func (p *processLimit) close() {
	if p == nil {
		return
	}
	syscall.Close(p.fd)
}
