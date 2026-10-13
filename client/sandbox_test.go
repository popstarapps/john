package client

import (
	"errors"
	"strings"
	"testing"
)

// A control that silently degrades is not a control. If this machine cannot
// start a virtual machine, john refuses rather than running the agent's
// commands directly on the developer's disk — the readme promises a sandbox,
// and a developer who believed it would have no way to notice.
func TestJohnRefusesToRunWithoutASandbox(t *testing.T) {
	// On a machine that can start a sandbox, pickSandbox lays down the
	// workspace's state directory under ~/.john/sandboxes. That is the real
	// home unless it is pointed elsewhere, and `john sandbox` would then list
	// a sandbox for a temp directory that no longer exists.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sb, err := pickSandbox(t.TempDir())
	if err == nil {
		// This machine has one; that is the other correct outcome, and what
		// matters is that it is a real one rather than the in-memory fake.
		if _, isFake := sb.(interface{ ExecScripted() }); isFake {
			t.Fatal("picked the in-memory sandbox automatically")
		}
		return
	}
	if sb != nil {
		t.Fatal("returned a sandbox alongside a refusal")
	}
}

// And the refusal says what to do about it. This is checked directly rather
// than through pickSandbox, because a machine that can start a sandbox never
// reaches the message — which is every developer's machine and was how this
// went stale once already.
//
// What is being checked is that john carries the machine's own words through.
// The reason is the machine's because it is the only thing that knows which of
// them it is — hardware virtualization turned off in the firmware, a group
// this user is not in, a library that is not installed — and each of those
// asks something different of the person reading it. Every hypervisor's
// Available() is written to be the second half of this message.
func TestTheRefusalSaysWhatToDoAboutIt(t *testing.T) {
	reason := errors.New("sandbox: libkrun is not installed. " +
		"Install it — on Fedora `dnf install libkrun`")
	err := errNoSandbox(reason)
	for _, want := range []string{"cannot start a sandbox", "libkrun is not installed", "dnf install libkrun"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	// And says it once. The machine's errors name the package they came from,
	// which reads as a stutter under a line that has already said this is
	// about the sandbox.
	if strings.Contains(err.Error(), "sandbox: libkrun") {
		t.Errorf("the refusal repeats the package prefix: %v", err)
	}
	// It must not offer a way to run outside one. There is none, and the
	// in-memory sandbox is a test double that executes nothing.
	if strings.Contains(err.Error(), "fake") {
		t.Errorf("the refusal offers the fake sandbox: %v", err)
	}
}
