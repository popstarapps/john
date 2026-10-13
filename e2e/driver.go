package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The developer at the keyboard, when the script runs out.
//
// A step's first message is fixed, so runs compare. What comes after is not:
// the agent asks "would you like me to create a React project?", writes an
// HTML page instead of the app, gives up on a blocked host. A person answers
// that and carries on; a fixed script sends its next line regardless, and the
// rest of the run measures nothing. So while a step's check has not passed,
// `claude -p` is shown the goal and what john drew during the step, and
// writes what a developer would type next — or says it is time to stop.
//
// It plays the developer only. It has no tools, and whatever it says goes in
// as typed text, through the same prompt box anyone uses.

const driverPrompt = `You are a software developer using "john", a coding agent in a terminal, for the first time.
You are trying to get one thing done:

  %s

Below is what the terminal showed during this attempt: your messages (after ❯), the agent's replies (after →),
the commands it ran (● succeeded, ✗ failed), and notices from john itself. Network access the agent asked for has
already been approved by you each time it came up — you never need to approve anything by hand.

Reply with exactly the next message you would type to the agent: one or two plain sentences, like a person
typing — answer what it asked, or tell it what to do next to reach the goal. No quotes, no preamble.
If the goal is plainly impossible from here (the agent or john is broken, or keeps failing at the same thing),
reply with a single line starting "GIVE UP:" and the reason.

--- terminal ---
%s`

// nextMessage asks claude -p for the developer's next message. ok is false
// when it gave up; the reason is then in msg.
func (r *runner) nextMessage(goal, screen string) (msg string, ok bool, err error) {
	if _, err := exec.LookPath("claude"); err != nil || r.opts.driver == "none" {
		// No one to ask: the developer who just says yes.
		return "Yes, please go ahead and do that — whatever it takes to reach the goal: " + goal, true, nil
	}
	if len(screen) > 12000 {
		screen = "…\n" + screen[len(screen)-12000:]
	}
	dir, err := os.MkdirTemp("", "john-e2e-driver-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", r.opts.driver, "--tools", "")
	cmd.Dir = dir // nothing to read: it answers from the terminal it is shown
	cmd.Env = r.env
	cmd.Stdin = strings.NewReader(fmt.Sprintf(driverPrompt, goal, screen))
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("claude -p: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	reply := strings.TrimSpace(out.String())
	// One line: an Enter in the middle would send half of it.
	reply = strings.Join(strings.Fields(reply), " ")
	if reply == "" {
		return "", false, errors.New("claude -p answered nothing")
	}
	if why, gaveUp := strings.CutPrefix(reply, "GIVE UP:"); gaveUp {
		return strings.TrimSpace(why), false, nil
	}
	return reply, true, nil
}

// pursue works at one goal the way a developer would: the fixed first
// message, approvals as they come up, and then — while done says it is not
// done — whatever the developer would say next, up to nudges times.
func (r *runner) pursue(s *step, goal, first string, done func() bool, nudges int) error {
	mark := r.term.Mark()
	if err := r.promptApproving(s, first); err != nil {
		return err
	}
	for i := 0; i < nudges && !done(); i++ {
		next, ok, err := r.nextMessage(goal, r.term.Since(mark))
		if err != nil {
			return err
		}
		if !ok {
			s.Notes = append(s.Notes, "the developer gave up: "+next)
			return fmt.Errorf("gave up: %s", next)
		}
		if err := r.promptApproving(s, next); err != nil {
			return err
		}
	}
	if !done() {
		return fmt.Errorf("not done after %d nudges", nudges)
	}
	return nil
}
