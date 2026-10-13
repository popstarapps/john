package client

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/popstarapps/john/registryauth"
)

// pipe is standard input with something already on it.
func pipe(t *testing.T, content string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// The way a script signs in, and the only way john will take a password that
// is not typed: a password on a command line is a password in the shell
// history and in every process list on the machine.
func TestAPasswordIsPipedInRatherThanPassedAsAFlag(t *testing.T) {
	cred, err := askForCredential("bob", true, pipe(t, "hunter2\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "bob" || cred.Secret != "hunter2" {
		t.Fatalf("%+v", cred)
	}

	// Piped with nobody named is a half-written command, not a login.
	if _, err := askForCredential("", true, pipe(t, "hunter2\n"), false); err == nil ||
		!strings.Contains(err.Error(), "--username") {
		t.Fatalf("err = %v", err)
	}

	// And an empty pipe is a script whose secret did not arrive, which must
	// not become an empty password sent to a registry.
	if _, err := askForCredential("bob", true, pipe(t, ""), false); err == nil {
		t.Fatal("an empty standard input was taken as a password")
	}
}

// With no terminal to ask on, john says what to type instead of putting up a
// prompt nobody can answer — the same rule the first-run question follows,
// because a script that hangs invisibly is worse than one that fails.
func TestWithNoTerminalJohnSaysWhatToPipe(t *testing.T) {
	_, err := askForCredential("bob", false, pipe(t, ""), false)
	if err == nil || !strings.Contains(err.Error(), "--password-stdin") {
		t.Fatalf("err = %v", err)
	}
}

// The wiring the sandbox actually uses: what `john registry login` stored is
// what the base-image pull signs in with.
func TestTheSandboxIsHandedWhatTheMachineSignedInWith(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	if err := registryauth.Save("ghcr.io",
		registryauth.Credential{Username: "bob", Secret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	got, err := registryCredentials("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "bob" || got.Secret != "s3cret" {
		t.Fatalf("%+v", got)
	}

	// A registry nobody signed in to is anonymous, not an error: that is
	// every public base image, which is most of them.
	got, err = registryCredentials("registry-1.docker.io")
	if err != nil || got.Username != "" || got.Secret != "" {
		t.Fatalf("%+v (%v)", got, err)
	}
}

// Flags typed after the argument they belong to still mean what they look
// like, INCLUDING a flag that takes a value.
//
// `john registry login host --username bob` is what a person types. Go's flag
// package stops at "host", so the flags have to be moved in front of it — and
// a flag that takes a value owns the argument after it, so moving it without
// "bob" made --username swallow the next flag instead. The version of this
// that only handled booleans was right until the first command here had a
// flag that was not one.
func TestFlagsTypedAfterTheArgumentStillCarryTheirValues(t *testing.T) {
	for _, args := range [][]string{
		{"host.example.com", "--username", "bob", "--password-stdin"},
		{"--username", "bob", "--password-stdin", "host.example.com"},
		{"--username=bob", "host.example.com", "--password-stdin"},
		{"host.example.com", "--password-stdin", "--username", "bob"},
	} {
		fs := flag.NewFlagSet("john registry login", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		username := fs.String("username", "", "")
		fromStdin := fs.Bool("password-stdin", false, "")
		if err := fs.Parse(hoistFlags(fs, args)); err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if *username != "bob" || !*fromStdin || fs.NArg() != 1 || fs.Arg(0) != "host.example.com" {
			t.Errorf("%v → username %q, stdin %v, args %v",
				args, *username, *fromStdin, fs.Args())
		}
	}
}
