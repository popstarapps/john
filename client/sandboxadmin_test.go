package client

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/sandbox"
)

// fakeSetup is a step of the shape the sandbox describes, with a Take that
// records whether it ran. The real one exists on one platform and only on a
// machine that cannot yet start a sandbox; what a developer is shown, and what
// each answer does, is the same everywhere.
func fakeSetup(t *testing.T, taken *bool, err error) *sandbox.AdminSetup {
	t.Helper()
	return &sandbox.AdminSetup{
		Reason:   "this machine will not let it",
		Question: "Let john ask for permission now?",
		Detail:   []string{"one consent prompt", "nothing stays elevated"},
		Done:     "Granted. Sign out and back in.",
		Take: func() error {
			*taken = true
			return err
		},
	}
}

// Yes takes the step, says what is left to do, and is remembered.
func TestSayingYesTakesTheStepAndRemembersIt(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	if err := credstore.SaveSetup(&credstore.Setup{Mode: credstore.ModeStandalone}); err != nil {
		t.Fatal(err)
	}

	var taken bool
	var out bytes.Buffer
	if !putSandboxAdmin(strings.NewReader("y\n"), &out, fakeSetup(t, &taken, nil)) {
		t.Fatal("answering yes did not report the step as taken")
	}
	if !taken {
		t.Fatal("answering yes did not run the step")
	}
	if !strings.Contains(out.String(), "Sign out and back in") {
		t.Fatalf("never said what is left to do:\n%s", out.String())
	}
	if !sandboxAdminAsked() {
		t.Fatal("the answer was not remembered")
	}
}

// The default is yes, because a developer who has read the question and
// pressed enter wants the sandbox john is for.
func TestPressingEnterTakesTheStep(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	var taken bool
	var out bytes.Buffer
	if !putSandboxAdmin(strings.NewReader("\n"), &out, fakeSetup(t, &taken, nil)) || !taken {
		t.Fatalf("enter did not take the step:\n%s", out.String())
	}
}

// No changes nothing, says so, and is remembered so the question is asked
// once — with the way back said out loud, because a machine that has answered
// no has no sandbox of its own and needs to know that is recoverable.
func TestSayingNoChangesNothingAndIsRemembered(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	if err := credstore.SaveSetup(&credstore.Setup{Mode: credstore.ModeStandalone}); err != nil {
		t.Fatal(err)
	}

	var taken bool
	var out bytes.Buffer
	if putSandboxAdmin(strings.NewReader("n\n"), &out, fakeSetup(t, &taken, nil)) {
		t.Fatal("answering no reported the step as taken")
	}
	if taken {
		t.Fatal("answering no ran the step anyway")
	}
	if !strings.Contains(out.String(), "-sandbox vm") {
		t.Fatalf("never said how to ask again:\n%s", out.String())
	}
	s, err := credstore.LoadSetup()
	if err != nil || s.SandboxAdmin != credstore.SandboxAdminDeclined {
		t.Fatalf("the refusal was not remembered: %+v %v", s, err)
	}
}

// Refusing the operating system's own prompt is an answer, not a failure, and
// it is NOT remembered: a consent prompt is as often mis-clicked as decided.
func TestRefusingTheConsentPromptIsNotRemembered(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	if err := credstore.SaveSetup(&credstore.Setup{Mode: credstore.ModeStandalone}); err != nil {
		t.Fatal(err)
	}

	var taken bool
	var out bytes.Buffer
	if putSandboxAdmin(strings.NewReader("y\n"), &out, fakeSetup(t, &taken, sandbox.ErrAdminDeclined)) {
		t.Fatal("a declined consent prompt reported the step as taken")
	}
	if strings.Contains(out.String(), "could not take that step") {
		t.Fatalf("a declined prompt was reported as a failure:\n%s", out.String())
	}
	if sandboxAdminAsked() {
		t.Fatal("a declined consent prompt was remembered as an answer")
	}
}

// A step that genuinely fails says so and does not pretend it worked.
func TestAStepThatFailsSaysSo(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	var taken bool
	var out bytes.Buffer
	if putSandboxAdmin(strings.NewReader("y\n"), &out, fakeSetup(t, &taken, errors.New("nope"))) {
		t.Fatal("a failed step reported itself as taken")
	}
	if !strings.Contains(out.String(), "nope") {
		t.Fatalf("never said why it failed:\n%s", out.String())
	}
	if sandboxAdminAsked() {
		t.Fatal("a failed step was remembered as an answer")
	}
}

// A machine with nothing to grant is asked nothing at all. That is every
// machine that can already start a sandbox, and every platform but one.
func TestAMachineWithNothingToGrantIsAskedNothing(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	var out bytes.Buffer
	if putSandboxAdmin(strings.NewReader(""), &out, nil) {
		t.Fatal("reported a step taken where there was none")
	}
	if out.Len() != 0 {
		t.Fatalf("said something on a machine with nothing to ask:\n%s", out.String())
	}
}

// Nobody at the terminal, or asked before: no question, and no attempt to
// work out whether there would have been one.
func TestTheQuestionIsAskedOnceAndOnlyOfAPerson(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	for _, c := range []struct {
		name        string
		interactive bool
		asked       bool
	}{
		{"a script", false, false},
		{"asked before", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if offerSandboxAdmin(strings.NewReader("y\n"), &out, c.interactive, t.TempDir(), c.asked) {
				t.Fatal("took an administrative step nobody was asked about")
			}
			if out.Len() != 0 {
				t.Fatalf("asked anyway:\n%s", out.String())
			}
		})
	}
}
