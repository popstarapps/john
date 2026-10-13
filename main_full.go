//go:build !john_guest

// Command john is the single executable for the whole system: the terminal
// client, the self-hosted backend, and a standalone mode that runs both in one
// process. Which one you get is decided here and nowhere else.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/popstarapps/john/backend"
	"github.com/popstarapps/john/client"
	"github.com/popstarapps/john/sandbox"
)

var version = "dev" // set with -ldflags "-X main.version=…"

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}

	client.SetVersion(version)
	switch cmd {
	case "":
		os.Exit(client.Run(args))
	case "backend":
		os.Exit(backend.Run(args, version))
	case "login":
		os.Exit(client.Login(args))
	case "apply":
		os.Exit(client.Apply(args))
	case "console":
		os.Exit(client.Console(args))
	case "sandbox":
		os.Exit(client.Sandbox(args))
	case "registry":
		os.Exit(client.Registry(args))
	case "__guest":
		// The agent inside the sandbox. It is the same binary the developer
		// runs, handed to the machine at boot, so the agent is always exactly
		// the client that started it and there is no version skew between the
		// two ends of a protocol with no handshake.
		os.Exit(guest(args))
	case "__machine":
		// The sandbox's virtual machine, on Linux, where it runs in a process
		// of its own because the monitor john uses takes over the process it
		// is started on. It is the same binary for the same reason __guest is
		// — nothing to build or ship separately, and no version to skew.
		// Nobody types this either.
		os.Exit(machineMonitor(args))
	case "__sandbox-admin":
		// The one thing john ever does with administrator rights, and the
		// whole of what runs with them: it grants this user the privilege to
		// create virtual machines and exits. john started it after asking the
		// developer, and the operating system asked them again before running
		// it. Nobody types this.
		os.Exit(sandboxAdmin())
	case "version":
		fmt.Println("john", version)
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "john: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
}

// guest runs the sandbox-side agent. It is not a command anyone types: the
// machine starts it, with its configuration as a single JSON argument.
func guest(args []string) int {
	var cfg sandbox.GuestConfig
	var err error
	switch len(args) {
	case 0:
		// Booted by a machine: the configuration is on the kernel command
		// line, because at boot there is nowhere else it could have been put.
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

// machineMonitor runs the sandbox's virtual machine. It is not a command
// anyone types: the sandbox starts it, with the machine's configuration as a
// single argument, and on success it does not return — the monitor runs the
// machine on this process and ends it when the guest stops.
func machineMonitor(args []string) int {
	if err := sandbox.RunMachineMonitor(args); err != nil {
		fmt.Fprintln(os.Stderr, "john:", err)
		return 1
	}
	return 0
}

// sandboxAdmin is the elevated half of the offer john makes when this machine
// will not let it create a virtual machine. It is deliberately the whole of
// the elevated program: one call, and an exit code the unelevated john reads.
func sandboxAdmin() int {
	if err := sandbox.GrantAdminAccess(); err != nil {
		fmt.Fprintln(os.Stderr, "john:", err)
		return 1
	}
	return 0
}

func usage(w *os.File) {
	fmt.Fprint(w, `john — an agent that codes in a sandbox, under your organization's policy.

The agent loop runs in the backend, not on this machine. john holds your
session, runs the tool calls the backend asks for inside a sandbox, and shows
you what changed. It never holds a model credential, and nothing it runs
touches your working directory until you apply it.

USAGE

  john                    start a session in the current directory
  john apply              review and apply a sandbox's changes, without a session
  john sandbox            what this project's sandboxes hold, and reclaim them
  john registry           sign in to a registry, for a private base image
  john login <url>        sign in to your organization's backend
  john login standalone   run john on this machine alone, backend in-process
  john console            open the web console in a browser
  john backend            run the backend service (--setup to bootstrap,
                          --listen <address:port> to choose where)
  john version            print the version

  The first plain john on a machine asks which of the two it should be; after
  that it is remembered.

IN A SESSION

  Type to ask for something. @ mentions a file, ctrl+r searches your history,
  Esc interrupts a turn, and you can keep typing while john works.

  /apply       review what the sandbox changed and write it to your working
               directory — the only thing that ever does
  /rebuild     build the environment again, keeping what you have not applied
  /reset       discard the sandbox entirely, unapplied changes included
  /env         what the sandbox is built from; /env init starts a definition
  /resources   the sandbox's cpu, memory, disk and process limits
  /resume      earlier sessions in this workspace
  /new         start a new conversation; the current one stays in /resume
  /policies    the policies applied to this session
  /model       the model this session runs on; /model <name> or a pick
               from the list moves it to another your policies allow
  /quit        end the session

THE SANDBOX

  Tool calls run in a Linux virtual machine on this machine and nowhere else.
  It has no network interface: its only route out is john's proxy, which
  applies your organization's network policy and records what was asked for.

  Your project is mounted at the path it lives at, and what the sandbox writes
  stays inside it until you apply it. Protected paths are enforced there, so a
  command that tries to read an excluded file finds nothing to read.

  What the sandbox contains is your project's `+"`.john/Dockerfile`"+` — a
  development environment, checked in, the same for everyone on the project.
  `+"`/env init`"+` writes a starting point. It is deliberately not the Dockerfile at
  the root of the repository, which is usually the image you deploy.

  Its base image is pulled on this machine, so a private registry is signed in
  to here: john reads your docker credentials if you have them, and
  `+"`john registry login`"+` is for a machine that has none. The password is spent
  on this side and the sandbox is handed the layers.

  A sandbox outlives the session that made it, so closing your terminal does
  not lose what you had not applied. Two terminals in one project get two
  sandboxes; john asks which to carry on with when the answer is not obvious.
  `+"`john sandbox`"+` lists them.
`)
}
