//go:build !unix

package sandbox

import "os/exec"

// inOwnGroup is the Unix process group a cancelled command is killed with.
// The only guest that runs off Unix is the loopback machine's, in-process on
// Windows, which isolates nothing; there the shell alone is killed.
func inOwnGroup(*exec.Cmd) {}
