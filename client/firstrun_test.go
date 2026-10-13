package client

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/popstarapps/john/client/credstore"
)

// The question is asked once. After that, plain `john` is a bare command.
func TestTheFirstRunAsksOnceAndThenNeverAgain(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	var out bytes.Buffer
	mode, err := machineMode(strings.NewReader("1\n"), &out, true)
	if err != nil {
		t.Fatalf("machineMode: %v", err)
	}
	if mode != credstore.ModeStandalone {
		t.Fatalf("answering 1 chose %q", mode)
	}
	if !strings.Contains(out.String(), "Standalone") {
		t.Fatalf("the question never mentioned the choice:\n%s", out.String())
	}

	// Second run: nothing is read, nothing is printed.
	out.Reset()
	mode, err = machineMode(strings.NewReader(""), &out, true)
	if err != nil {
		t.Fatalf("second machineMode: %v", err)
	}
	if mode != credstore.ModeStandalone {
		t.Fatalf("the answer was not remembered: %q", mode)
	}
	if out.Len() != 0 {
		t.Fatalf("asked again on a machine that had already answered:\n%s", out.String())
	}
}

// Choosing an organization needs a URL, which is the one thing that cannot be
// guessed — so it hands over to the command that takes one rather than
// growing a second sign-in path.
func TestChoosingAnOrganizationPointsAtLogin(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	var out bytes.Buffer
	if _, err := machineMode(strings.NewReader("2\n"), &out, true); err != errNeedsLogin {
		t.Fatalf("choosing 2 = %v, want errNeedsLogin", err)
	}
	if !strings.Contains(out.String(), "john login https://") {
		t.Fatalf("never said what to run:\n%s", out.String())
	}
	// And nothing was recorded: the machine is not set up until it signs in.
	if _, err := credstore.LoadSetup(); err != credstore.ErrNoSetup {
		t.Fatalf("recorded a mode before signing in: %v", err)
	}
}

func TestAnUnrecognisedAnswerAsksAgain(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	var out bytes.Buffer
	mode, err := machineMode(strings.NewReader("yes please\n\n1\n"), &out, true)
	if err != nil {
		t.Fatalf("machineMode: %v", err)
	}
	if mode != credstore.ModeStandalone {
		t.Fatalf("mode = %q", mode)
	}
	if n := strings.Count(out.String(), "Please answer 1 or 2."); n != 2 {
		t.Fatalf("re-asked %d times, want 2:\n%s", n, out.String())
	}
}

// A prompt nobody can answer would hang a script or a CI job forever, so a
// non-terminal gets told what to run instead.
func TestWithNoTerminalItSaysWhatToRunRatherThanAsking(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	var out bytes.Buffer
	_, err := machineMode(strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("a non-interactive first run was allowed to continue")
	}
	if !strings.Contains(err.Error(), "john login standalone") {
		t.Fatalf("error does not say what to run: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("prompted with nobody there to answer:\n%s", out.String())
	}
}

// A machine signed in before this question existed has already answered it.
// Asking would be a regression dressed up as a feature.
func TestAMachineThatAlreadySignedInIsNotAsked(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	if err := credstore.Save(&credstore.Credentials{
		BackendURL: "https://john.acme.example",
		Token:      "tok",
		Email:      "dev@acme.example",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	mode, err := machineMode(strings.NewReader(""), &out, true)
	if err != nil {
		t.Fatalf("machineMode: %v", err)
	}
	if mode != credstore.ModeBackend {
		t.Fatalf("mode = %q, want backend", mode)
	}
	if out.Len() != 0 {
		t.Fatalf("asked a machine that was already signed in:\n%s", out.String())
	}
	// And it was written down, so the inference happens once.
	s, err := credstore.LoadSetup()
	if err != nil || s.Mode != credstore.ModeBackend {
		t.Fatalf("LoadSetup = %+v, %v", s, err)
	}
}

func TestLoginStandaloneSetsTheMachineUpWithoutSigningInAnywhere(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	if code := Login([]string{"standalone"}); code != 0 {
		t.Fatalf("john login standalone = %d", code)
	}
	s, err := credstore.LoadSetup()
	if err != nil || s.Mode != credstore.ModeStandalone {
		t.Fatalf("LoadSetup = %+v, %v", s, err)
	}
	// No credentials were invented: standalone signs its own user in when the
	// session starts, and there is nothing to store here.
	if _, err := credstore.Load(); err != credstore.ErrNotLoggedIn {
		t.Fatalf("standalone setup stored credentials: %v", err)
	}
}

// Choosing standalone again is not news, and saying "now runs" would suggest
// something changed.
func TestLoginStandaloneTwiceSaysItAlreadyWas(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	say := func() string {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout := os.Stdout
		os.Stdout = w
		code := Login([]string{"standalone"})
		os.Stdout = stdout
		w.Close()
		out, _ := io.ReadAll(r)
		if code != 0 {
			t.Fatalf("john login standalone = %d", code)
		}
		return string(out)
	}
	if out := say(); !strings.Contains(out, "now runs john standalone") {
		t.Fatalf("first time: %q", out)
	}
	if out := say(); !strings.Contains(out, "already runs john standalone") {
		t.Fatalf("second time: %q", out)
	}
}

// The argument is positional now. Something that is neither a URL nor
// "standalone" is a typo, and saying so beats trying to sign in to it.
func TestLoginRejectsSomethingThatIsNeitherAUrlNorStandalone(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	if code := Login([]string{"standalne"}); code != 2 {
		t.Fatalf("john login standalne = %d, want 2", code)
	}
	if _, err := credstore.LoadSetup(); err != credstore.ErrNoSetup {
		t.Fatalf("a typo changed this machine's setup: %v", err)
	}
}

func TestLoginWithNothingToGoOnSaysWhatItNeeds(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	if code := Login(nil); code != 2 {
		t.Fatalf("john login on a fresh machine = %d, want 2", code)
	}
}
