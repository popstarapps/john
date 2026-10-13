package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The reviewer is `claude -p`, reading the run the way a person debugging it
// would: the report, the screen, the decoded trace and the guest console,
// with john's source beside them to say where a problem comes from. It reads
// and does nothing else — the tools it is given cannot change a file.
//
// It looks for john's bugs, not the model's. A model that claims a timed-out
// install succeeded is worth a line; john telling it "EOF" and nothing more is
// the finding.
const reviewPrompt = `You are reviewing an automated end-to-end run of john, the program whose source is in %[1]s.

john runs a coding agent in a sandbox VM, with a standalone backend, a terminal UI, a network proxy whose
blocked requests become access requests an administrator approves, and an apply step that writes the
sandbox's changes to the working directory. A harness drove it like a first-time developer would: chose
standalone, added a model, waited for the sandbox, typed text beyond ASCII, a pasted block and a long line,
asked for a React tic-tac-toe app, approved network access as the agent hit blocks, installed Node and npm
packages, ran the build, interrupted a long command with Esc, had an access request rejected, allowed a host
that does not exist, restarted john, built again, switched models through a keyword routing policy, applied
the result, applied one file of two, edited a file on the host and applied what was left, changed
.john/Dockerfile and rebuilt, and killed john mid-command before resuming the session.

The run's files are in the current directory:
  report.md            the steps, which passed, every tool call and its exit code
  screen.txt           each john session exactly as the terminal drew it
  trace.jsonl          the backend's record of every session, decrypted (prompts, tool calls and results,
                       proxy decisions, assistant messages, turn ends)
  guest-console-*.log  the sandbox kernel's console
  project/             the working directory after apply

Find problems with john itself — crashes, hangs, a sandbox that stops answering, errors that say too
little, wrong or confusing text on screen, rendering glitches, things that are slow for no reason, a
restart that loses state, an apply that writes the wrong thing, a proxy decision that makes no sense,
messages that are wrong for standalone mode. Read the source to say where each one comes from.
Mistakes the model made on its own are not findings, unless john gave it bad information that caused them.

Write the review as Markdown: a one-line verdict, then each finding as a heading with what happened (quote
the screen or trace, with where to find it), why it is john's fault (file:line), and what would fix it.
Most severe first. If the run was clean, say so in one line and stop.

Only your final message is kept, so it must be the whole review, complete on its own — not a summary of,
or a pointer to, anything you wrote earlier.`

func (r *runner) wantReview() bool {
	switch r.opts.review {
	case "no":
		return false
	case "yes":
		return true
	}
	_, err := exec.LookPath("claude")
	return err == nil
}

func (r *runner) reviewRun(repo string) error {
	// --tools, not only --allowedTools: allowing names what may run without
	// asking, and leaves the rest of the tool set there to be reached for.
	// A reviewer that handed the reading to background agents wrote its
	// review in a message part-way through and ended on "the full review is
	// in my previous message" — and only the last message is what -p prints.
	cmd := exec.Command("claude", "-p",
		"--tools", "Read,Grep,Glob",
		"--allowedTools", "Read,Grep,Glob",
		"--add-dir", repo)
	cmd.Dir = r.out
	cmd.Env = r.env // without ANTHROPIC_API_KEY: the review goes to claude's own sign-in
	cmd.Stdin = strings.NewReader(fmt.Sprintf(reviewPrompt, repo))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
	case <-time.After(30 * time.Minute):
		cmd.Process.Kill()
		return fmt.Errorf("claude -p did not finish in 30 minutes")
	}
	return os.WriteFile(filepath.Join(r.out, "review.md"), stdout.Bytes(), 0o644)
}
