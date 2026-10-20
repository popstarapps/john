// Package client is the john CLI: the terminal session, login, and the
// standalone mode that runs a backend in-process. It renders the session and
// executes tool calls in the sandbox; the agent loop lives in the backend,
// and nothing in this package can reach a model provider.
package client

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// Run is the `john` session entrypoint — the bare command, with no arguments,
// which is what people actually type. It runs whichever john this machine is
// set up for, asking once on a machine that has never been told.
func Run(args []string) int {
	sweepAfterUpdate()

	fs := flag.NewFlagSet("john", flag.ExitOnError)
	var (
		plain   = fs.Bool("plain", false, "force the non-interactive renderer")
		ascii   = fs.Bool("ascii", false, "use the ASCII glyph set")
		backend = fs.String("backend", "", "backend URL override (default: where `john login` signed in)")
		sbMode  = fs.String("sandbox", "auto", "sandbox implementation: auto, vm, loopback")
		sbInst  = fs.String("sandbox-instance", "", "which of this workspace's sandboxes to use")
		spinner = fs.String("spinner", "random", "busy spinner: random, braille, bar, arc, circle, dot, …")
		effect  = fs.String("effect", "random", "busy label effect: random, none, shimmer, pulse, flow, …")
		preview = fs.Bool("busy-preview", false, "animate every busy style and exit")
	)
	fs.Parse(args)

	if *preview {
		previewBusy()
		return 0
	}

	// An explicit --backend is someone who knows exactly what they want; it
	// does not ask, and it does not change what this machine is set up for.
	if *backend == "" {
		mode, err := machineMode(os.Stdin, os.Stdout, stdinIsATerminal())
		if err != nil {
			if err != errNeedsLogin {
				fmt.Fprintln(os.Stderr, "john:", err)
			}
			return 1
		}
		if mode == credstore.ModeStandalone {
			return standalone(standaloneOptions{
				Plain: *plain, ASCII: *ascii, Sandbox: *sbMode, SandboxInstance: *sbInst,
				Spinner: *spinner, Effect: *effect,
			})
		}
	}

	creds, err := credstore.Load()
	if err != nil && *backend == "" {
		fmt.Fprintln(os.Stderr, "john:", err)
		return 1
	}
	// The access token lives hours, so a machine that has not run john since
	// yesterday starts by renewing. Only a sign-in that is genuinely over
	// stops us here; anything else is transient and the token in hand may
	// still be good. A backend that is down while the token has run out is
	// waited out on a terminal, since it is usually back within seconds.
	if creds != nil {
		held := creds
		try := func() (*credstore.Credentials, error) { return ensureFresh(held) }
		if stdinIsATerminal() {
			theme := terminalui.DefaultTheme(os.Stderr)
			if *ascii {
				theme.SetASCII()
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			creds, err = waitForBackend(ctx, os.Stderr, theme, try)
			stop()
		} else {
			creds, err = try()
		}
		if errors.Is(err, context.Canceled) {
			return 130
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "john:", err)
			return 1
		}
	}
	opts := Options{
		Plain:   *plain,
		ASCII:   *ascii,
		Version: buildVersion,
	}
	if creds != nil {
		opts.BackendURL, opts.Token, opts.Email = creds.BackendURL, creds.Token, creds.Email
		opts.Credentials = creds
	}
	if *backend != "" {
		opts.BackendURL = *backend
	}

	// Ask the backend what it runs before opening a session against it. The
	// handshake refuses a mismatch too, and that refusal is the guarantee —
	// but catching it here means the question is asked on a plain terminal,
	// before a UI is drawn and torn down again.
	if u := checkVersion(opts.BackendURL, opts.Version, opts.Token); u != nil {
		return runUpdate(u, os.Stdin, os.Stdout, stdinIsATerminal())
	}

	opts.Workdir, _ = os.Getwd()

	sb, err := namedSandbox(*sbMode, opts.Workdir, *sbInst)
	if err != nil {
		if err != errSignInAgain {
			fmt.Fprintln(os.Stderr, "john:", err)
		}
		return 2
	}
	opts.Sandbox = sb

	opts.BusySpinner, opts.BusyEffect = *spinner, *effect
	return Session(opts)
}

// buildVersion is stamped by main; a plain `go test ./client` sees "dev".
var buildVersion = "dev"

// errSignInAgain ends the run quietly: the developer has just been told, in
// more words than an error line would carry, that the permission they now have
// arrives with their next Windows sign-in. Repeating it as an error would be
// noise.
var errSignInAgain = errors.New("")

// SetVersion lets the root command pass the build version down.
func SetVersion(v string) { buildVersion = v }

// namedSandbox resolves the -sandbox flag. "auto" returns nil, which leaves
// the choice to the client — the strongest sandbox the machine can give.
func namedSandbox(mode, workdir, wantInstance string) (sandbox.Sandbox, error) {
	switch mode {
	case "auto":
		// The choice is Session's, and so is the question that comes before
		// it — standalone reaches Session without coming through here.
		return nil, nil
	case "vm":
		vm, err := sandbox.NewVM(workdir, sandbox.VMOptions{Version: buildVersion,
			DefaultEnvironment: sandbox.StarterEnvironment(), Instance: wantInstance,
			RegistryCredentials: registryCredentials})
		if err != nil {
			return nil, err
		}
		if err := vm.HypervisorAvailable(); err != nil {
			// Naming the machine is a developer saying they want it, so this
			// asks even on a machine that turned the offer down before.
			if !offerSandboxAdmin(os.Stdin, os.Stdout, stdinIsATerminal(), workdir, false) {
				return nil, err
			}
			// The step is taken and it still cannot start one now: the
			// privilege arrives with the next sign-in, not with this process.
			return nil, errSignInAgain
		}
		return vm, nil
	case "loopback":
		// Not a sandbox. It runs the agent as an ordinary process on this
		// machine so that john can be developed where there is no hypervisor,
		// and it says so every time rather than letting anyone forget.
		fmt.Fprintln(os.Stderr, "john: -sandbox loopback isolates nothing — tool calls run on this machine")
		return sandbox.NewLoopbackVM(workdir)
	default:
		return nil, fmt.Errorf("unknown -sandbox %q (auto, vm, loopback)", mode)
	}
}
