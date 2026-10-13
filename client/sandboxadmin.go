package client

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/sandbox"
)

// A machine that is one administrative step away from its own virtual machine.
//
// This is the second question john asks on a new machine and it has the same
// shape as the first: asked once, asked on a plain terminal before any UI is
// drawn over it, and the answer remembered. What is different is that john can
// carry the answer out — the step is a single change to which local group the
// developer is in, `john __sandbox-admin` is the whole of what runs with
// administrator rights to make it, and john needs none afterwards.
//
// The alternative was a paragraph of instructions in an error message. On the
// platform where this happens it is the difference between john working and
// not, on the very first run, so it is worth a question.

// offerSandboxAdmin puts the step to the developer and takes it if they say
// yes, reporting whether it was taken.
//
// It says nothing at all in every case where there is no question to ask: a
// machine that can already start a sandbox, one whose trouble is something
// else, one that has been asked before, and anything that is not a person at a
// terminal — a script, CI, a pipe. A prompt nobody can answer would hang a
// build.
func offerSandboxAdmin(in io.Reader, out io.Writer, interactive bool, workdir string, asked bool) bool {
	if !interactive || asked {
		return false
	}
	vm, err := sandbox.NewVM(workdir, sandbox.VMOptions{Version: buildVersion})
	if err != nil {
		return false
	}
	return putSandboxAdmin(in, out, sandbox.AdminSetupFor(vm.HypervisorAvailable()))
}

// putSandboxAdmin is the question itself, apart from finding out whether there
// is one to ask — so that what a developer sees can be tested on any machine,
// including all the ones with nothing to grant.
func putSandboxAdmin(in io.Reader, out io.Writer, setup *sandbox.AdminSetup) bool {
	if setup == nil {
		return false
	}

	fmt.Fprintf(out, "\njohn runs each session in a virtual machine of its own, and %s.\n\n", setup.Reason)
	for _, line := range setup.Detail {
		fmt.Fprintln(out, " ", line)
	}
	fmt.Fprintln(out)

	if !askYes(in, out, setup.Question+" [Y/n]: ") {
		rememberSandboxAdmin(credstore.SandboxAdminDeclined)
		fmt.Fprintln(out, "\nLeft as it is. john will use whatever sandbox this machine can give.")
		fmt.Fprintln(out, "`john -sandbox vm` asks again.")
		return false
	}

	if err := setup.Take(); err != nil {
		if errors.Is(err, sandbox.ErrAdminDeclined) {
			// They were asked by the operating system and said no, which is an
			// answer and not a failure. It is not remembered: refusing a
			// consent prompt is as often a mis-click as a decision.
			fmt.Fprintln(out, "\nNot granted. `john -sandbox vm` asks again.")
			return false
		}
		fmt.Fprintln(out, "\njohn could not take that step:", err)
		return false
	}

	rememberSandboxAdmin(credstore.SandboxAdminGranted)
	fmt.Fprintln(out, "\n"+setup.Done)
	return true
}

// rememberSandboxAdmin records the answer so the question is asked once. A
// machine that cannot record it is asked again, which is the harmless
// direction to fail in.
func rememberSandboxAdmin(state string) {
	s, err := credstore.LoadSetup()
	if err != nil {
		return
	}
	s.SandboxAdmin = state
	_ = credstore.SaveSetup(s)
}

// sandboxAdminAsked reports whether this machine has been offered the step
// before.
func sandboxAdminAsked() bool {
	s, err := credstore.LoadSetup()
	return err == nil && s.SandboxAdmin != ""
}

// askYes asks a yes-or-no question that defaults to yes, and reads a closed
// input as no rather than looping on it.
func askYes(in io.Reader, out io.Writer, prompt string) bool {
	r := bufio.NewReader(in)
	for {
		fmt.Fprint(out, prompt)
		line, err := r.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		}
		if err != nil {
			return false
		}
		fmt.Fprintln(out, "Please answer y or n.")
	}
}
