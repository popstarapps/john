package keychain

import (
	"errors"
	"strings"
	"testing"
)

func TestASecretGoesInAndComesBack(t *testing.T) {
	defer Use(NewMemory())()

	if _, err := Get("machine-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty keychain answered %v, want not found", err)
	}
	if err := Set("machine-token", "tok_abc"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := Get("machine-token"); err != nil || got != "tok_abc" {
		t.Fatalf("get = %q, %v", got, err)
	}
	if err := Delete("machine-token"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := Get("machine-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	// Deleting what is not there is the outcome asked for, not an error.
	if err := Delete("machine-token"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// The property that makes three backends nobody can run on one machine safe to
// ship: a store that does not return what it was given is caught while the
// caller still holds the credential, and is left holding nothing.
func TestAStoreThatMangesASecretIsNotBelieved(t *testing.T) {
	m := NewMemory()
	m.Mangle = func(s string) string { return strings.ReplaceAll(s, `"`, "") }
	defer Use(m)()

	err := Set("registry:ghcr.io", `pa"ss`)
	if err == nil {
		t.Fatal("a mangled round trip was accepted")
	}
	if !strings.Contains(err.Error(), "did not return the secret") {
		t.Fatalf("error was %v — it should say what went wrong", err)
	}
	if m.Has("registry:ghcr.io") {
		t.Fatal("a half-written item was left behind for the next Get to believe")
	}
}

func TestAStoreThatRefusesIsReportedRatherThanIgnored(t *testing.T) {
	m := NewMemory()
	m.Fail = errors.New("the keychain is locked")
	defer Use(m)()

	if err := Set("machine-token", "tok"); err == nil {
		t.Fatal("a refused write was reported as success")
	}
}

func TestWithNoKeychainEverythingSaysSo(t *testing.T) {
	defer Use(none{})()

	if Available() {
		t.Fatal("no keychain, but Available said yes")
	}
	if Name() != "" {
		t.Fatalf("Name = %q with no keychain", Name())
	}
	if _, err := Get("machine-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v", err)
	}
	if err := Set("machine-token", "tok"); err == nil {
		t.Fatal("set succeeded with nowhere to put it")
	}
	// Clearing credentials must not fail on a machine that never had a
	// keychain to clear them from.
	if err := Delete("machine-token"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

func TestTheEscapeHatchTurnsItOff(t *testing.T) {
	defer Use(NewMemory())()
	if !Available() {
		t.Fatal("the in-memory keychain is not available")
	}
	for _, v := range []string{"off", "0", "no", "NONE", "false"} {
		t.Setenv("JOHN_KEYCHAIN", v)
		if Available() {
			t.Fatalf("JOHN_KEYCHAIN=%s did not turn the keychain off", v)
		}
	}
	t.Setenv("JOHN_KEYCHAIN", "")
	if !Available() {
		t.Fatal("an empty JOHN_KEYCHAIN turned the keychain off")
	}
}

// JOHN_HOME says where john's state lives, so a store outside it is not one
// this run should be using. It is also what keeps `go test` on a developer's
// own machine away from the credential they are actually signed in with.
func TestJohnHomeKeepsARealStoreOutOfIt(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())
	if realStoreWanted() {
		t.Fatal("a real keychain would have been used inside JOHN_HOME")
	}
	t.Setenv("JOHN_HOME", "")
	if !realStoreWanted() {
		t.Fatal("no JOHN_HOME, and a real keychain was still refused")
	}
}
