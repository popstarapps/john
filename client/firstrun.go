package client

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/popstarapps/john/client/credstore"
)

// The first time someone types `john` there is a question that has to be
// asked once and then never again: is this machine running john on its own,
// or signed in to an organization's backend? The two are genuinely different
// products — one has an administrator, policies and a shared trace store; the
// other has none of that — so guessing would be worse than asking.
//
// After the answer it is recorded, and plain `john` is a bare command with no
// arguments forever after.

// machineMode returns the mode this machine runs in, asking once on a fresh
// machine. A terminal gets the question; anything else — a script, CI, a
// pipe — gets an error telling it what to run, because a prompt nobody can
// answer would hang.
func machineMode(in io.Reader, out io.Writer, interactive bool) (string, error) {
	setup, err := credstore.LoadSetup()
	if err == nil {
		return setup.Mode, nil
	}
	if err != credstore.ErrNoSetup {
		return "", err
	}

	// A machine that signed in before this question existed has already
	// answered it. Do not ask again.
	if c, cerr := credstore.Load(); cerr == nil && c.BackendURL != "" {
		_ = credstore.SaveSetup(&credstore.Setup{Mode: credstore.ModeBackend})
		return credstore.ModeBackend, nil
	}

	if !interactive {
		return "", fmt.Errorf("this machine has no john setup yet — run `john login standalone`,\n" +
			"  or `john login https://<your-john-backend>` to join an organization")
	}
	return askMode(in, out)
}

// askMode puts the choice to the developer and records the answer.
func askMode(in io.Reader, out io.Writer) (string, error) {
	fmt.Fprint(out, `
Welcome to john. How should this machine run?

  1. Standalone — john runs on its own here, using your own model API keys.
  2. Organization — sign in to a john backend that holds the models and the
     policies, run by whoever administers it.

You can change this later with `+"`john login`"+`.

`)

	r := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "Choose [1/2]: ")
		line, err := r.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return "", fmt.Errorf("no answer — run `john login standalone` or `john login <url>`")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "1", "standalone", "s":
			if err := credstore.SaveSetup(&credstore.Setup{Mode: credstore.ModeStandalone}); err != nil {
				return "", err
			}
			fmt.Fprintln(out, "\nRunning standalone. `john login <url>` joins an organization later.")
			return credstore.ModeStandalone, nil
		case "2", "organization", "org", "o":
			// The URL is the thing we do not have and cannot guess, so this
			// hands over to the command that takes one rather than growing a
			// second sign-in path here.
			fmt.Fprintln(out, "\nRun `john login https://<your-john-backend>` to sign in.")
			return "", errNeedsLogin
		default:
			fmt.Fprintln(out, "Please answer 1 or 2.")
		}
	}
}

// errNeedsLogin ends the run quietly: the developer has just been told
// exactly what to type, and repeating it as an error would be noise.
var errNeedsLogin = fmt.Errorf("")

// stdinIsATerminal reports whether there is someone there to answer.
func stdinIsATerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}
