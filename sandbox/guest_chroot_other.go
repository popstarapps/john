//go:build !linux

package sandbox

import (
	"errors"
	"os/exec"
)

// The environment is a Linux root filesystem and running inside one is a
// Linux operation. This exists so the client, which is not Linux, compiles the
// package that describes the guest — it is never reached, because the only
// machine that runs the agent off Linux is the loopback one, which builds no
// environment.
// Nothing to flush: the loopback machine writes to the developer's own
// filesystem, which nobody is about to power off.
func syncDisks() error { return nil }

func unmountIfMounted(string) {}

func unmountEnvironment(envLayout) {}

func mountEnvOverlay(envLayout) error {
	return errors.New("sandbox: an environment can only be entered inside the Linux sandbox")
}

func envSupported(string) bool { return false }

func setChroot(*exec.Cmd, string) error {
	return errors.New("sandbox: an environment can only be entered inside the Linux sandbox")
}

func mountKernelFilesystems(string, string) error {
	return errors.New("sandbox: an environment can only be built inside the Linux sandbox")
}
