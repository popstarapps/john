package client

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// Apply is the `john apply` entrypoint: it opens the review-and-apply view for
// the current directory's sandbox, without a session. The sandbox and its
// accumulated changes live on this machine, so applying them needs no backend
// — the review is the control, and it happens here.
func Apply(args []string) int {
	fs := flag.NewFlagSet("john apply", flag.ExitOnError)
	var (
		plain  = fs.Bool("plain", false, "force the non-interactive renderer")
		ascii  = fs.Bool("ascii", false, "use the ASCII glyph set")
		sbMode = fs.String("sandbox", "auto", "sandbox implementation: auto, vm, loopback")
		sbInst = fs.String("sandbox-instance", "", "which of this workspace's sandboxes to use")
	)
	fs.Parse(args)

	workdir, _ := os.Getwd()
	sb, err := applySandbox(*sbMode, workdir, *sbInst)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john apply:", err)
		return 1
	}

	ctx := context.Background()
	st, err := sb.Status(ctx)
	if err != nil || st.State != sandbox.StateReady {
		fmt.Println("no sandbox for this directory yet — run john here first")
		return 0
	}
	diffs, err := sb.Diff(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john apply:", err)
		return 1
	}
	// Apply runs without a backend, on the protections the last session
	// persisted for this workspace. It is the last gate before the host tree,
	// so it is not a gate that gets to be skipped for want of a session.
	diffs, held := splitProtected(diffs)
	if len(held) > 0 {
		fmt.Println(heldBackNotice(held))
	}
	if len(diffs) == 0 {
		fmt.Println("nothing to apply · the sandbox matches your working directory")
		return 0
	}

	opts := terminalui.Options{ASCII: *ascii, Logo: false}
	if *plain {
		opts.Mode = terminalui.ModePlain
	}
	ui := terminalui.New(opts)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go runApplyFlow(runCtx, ui, sb, workdir, diffs)

	if err := ui.Run(runCtx); err != nil && runCtx.Err() == nil {
		fmt.Fprintln(os.Stderr, "john apply:", err)
		return 1
	}
	return 0
}

// runApplyFlow drives the review → confirm → apply interaction and then quits.
func runApplyFlow(ctx context.Context, ui *terminalui.UI, sb sandbox.Sandbox, workdir string, diffs []sandbox.FileDiff) {
	diffs2 := make(chan terminalui.DiffDecision, 1)
	confirms := make(chan terminalui.ConfirmDecision, 1)

	// A tiny action pump for this one-shot flow.
	go func() {
		for act := range ui.Actions() {
			switch t := act.(type) {
			case terminalui.DiffDecision:
				diffs2 <- t
			case terminalui.ConfirmDecision:
				confirms <- t
			}
		}
	}()

	skip, why := unselectedByDefault(ctx, sb)
	if note := unselectedNotice(diffs, skip, why); note != "" {
		ui.Send(terminalui.Notice{Text: note})
	}
	ui.Send(renderDiffReview("apply", "apply to "+displayPath(workdir), diffs, skip))
	var dec terminalui.DiffDecision
	select {
	case dec = <-diffs2:
	case <-ctx.Done():
		return
	}
	if !dec.Apply || len(dec.Paths) == 0 {
		ui.Send(terminalui.Notice{Text: "not applied · everything stays in the sandbox"})
		ui.Quit()
		return
	}

	ui.Send(terminalui.Confirm{
		ID:       "confirm",
		Title:    "APPLY TO WORKING DIRECTORY",
		Question: fmt.Sprintf("Write %s into %s?", plural(len(dec.Paths), "file"), displayPath(workdir)),
		Detail:   dec.Paths,
	})
	var c terminalui.ConfirmDecision
	select {
	case c = <-confirms:
	case <-ctx.Done():
		return
	}
	if c.Decision != terminalui.DecisionYes {
		ui.Send(terminalui.Notice{Text: "not applied · everything stays in the sandbox"})
		ui.Quit()
		return
	}

	ui.Send(terminalui.ToolStart{ID: "apply", Verb: "apply", Targets: dec.Paths})
	n, err := sb.Apply(ctx, dec.Paths)
	if err != nil {
		ui.Send(terminalui.ToolEnd{ID: "apply", Status: terminalui.ToolFail, Meta: err.Error()})
	} else {
		ui.Send(terminalui.ToolEnd{ID: "apply", Status: terminalui.ToolOK,
			Meta: appliedMeta(n, workdir)})
	}
	ui.Quit()
}

// appliedMeta is what an apply line ends with: how many files, and the
// working directory by its name alone. The whole path was printed in full
// beside a file list cut to make room for it — "C:\Projects\John\e2e\runs\…
// \project" — when the directory is the one john was started in, and the
// status line already says which.
func appliedMeta(n int, workdir string) string {
	return plural(n, "file") + " → " + filepath.Base(workdir)
}

// applySandbox resolves the sandbox for `john apply`. It never creates a
// fresh one: apply operates on what a session already built, so a workspace
// with no sandbox simply has nothing to apply.
//
// Whichever it resolves to, it enforces the protections the last session
// stored beside the sandbox. This runs with no backend and no snapshot, and it
// is the last gate before the developer's files.
func applySandbox(mode, workdir, wantInstance string) (sandbox.Sandbox, error) {
	switch mode {
	case "loopback":
		return sandbox.NewLoopbackVM(workdir)
	case "auto", "vm":
		// The same answer either way now that there is one sandbox. `auto`
		// stays spelled out because it is the default and because a flag whose
		// documented value has no case reads like an oversight.
		return sandbox.NewVM(workdir, sandbox.VMOptions{Version: buildVersion,
			Instance: wantInstance, RegistryCredentials: registryCredentials})
	default:
		return nil, fmt.Errorf("unknown -sandbox %q (auto, vm, loopback)", mode)
	}
}
