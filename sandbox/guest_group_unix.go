//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// inOwnGroup starts a command as the leader of a process group of its own,
// and has cancelling it kill the whole group. Killing only the shell left what
// it ran behind: Esc on `sleep 300; echo finished` reported the command
// cancelled while the sleep ran on, and a build interrupted the same way would
// keep writing to the workspace after the developer had stopped it.
//
// It is called after everything else that sets the command's SysProcAttr,
// which it adds to rather than replaces.
func inOwnGroup(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		// The group's id is the leader's pid, and a negative pid names the
		// group.
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
