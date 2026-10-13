//go:build john_guest

// The agent that runs inside the sandbox, built on its own.
//
// It is the same source and the same release as the john a developer runs —
// `john __guest` in the full binary reaches the identical code — but built
// without the parts a sandbox has no use for: the backend, the web console,
// the terminal UI, the policy signing, SQLite. Everything the agent needs is
// the sandbox package and nothing else, and the linker drops the rest.
//
// That is worth a build tag because this binary is unpacked into memory on
// every sandbox start, and because the one place john deliberately runs
// untrusted code should carry as little as it can. It is not a second
// implementation: there is no code here that the full binary does not also
// have.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/popstarapps/john/sandbox"
)

var version = "dev" // set with -ldflags "-X main.version=…"

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "__guest" {
		args = args[1:]
	}
	os.Exit(guest(args))
}

// guest runs the sandbox-side agent, reading where to reach the host from the
// kernel command line when a machine started it and from an argument when the
// loopback machine did.
func guest(args []string) int {
	var cfg sandbox.GuestConfig
	var err error
	switch len(args) {
	case 0:
		cfg, err = sandbox.GuestConfigFromCmdline()
	case 1:
		err = jsonUnmarshalConfig(args[0], &cfg)
	default:
		fmt.Fprintln(os.Stderr, "john: __guest is started by the sandbox, not by hand")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "john:", err)
		return 2
	}
	if err := sandbox.RunGuestAgent(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "john:", err)
		return 1
	}
	return 0
}
