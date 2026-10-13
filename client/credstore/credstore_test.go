package credstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/popstarapps/john/keychain"
)

func machine(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("JOHN_HOME", home)
	return home
}

func readFile(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The pair goes to the keychain; the file keeps what a developer opens it for.
func TestTheTokensGoToTheKeychainAndTheRestStaysReadable(t *testing.T) {
	home := machine(t)
	kc := keychain.NewMemory()
	defer keychain.Use(kc)()

	in := &Credentials{
		BackendURL: "https://john.acme.com",
		Email:      "bob@acme.com",
		Token:      "tok_access",
		Refresh:    "tok_refresh",
		ExpiresAt:  time.Now().Add(12 * time.Hour).UTC(),
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}

	onDisk := readFile(t, home)
	for _, secret := range []string{"tok_access", "tok_refresh"} {
		if strings.Contains(onDisk, secret) {
			t.Fatalf("%s is in the file as well:\n%s", secret, onDisk)
		}
	}
	if !strings.Contains(onDisk, "john.acme.com") || !strings.Contains(onDisk, "bob@acme.com") {
		t.Fatalf("the file no longer says where or as whom:\n%s", onDisk)
	}
	if !kc.Has(tokenItem) || !kc.Has(refreshItem) {
		t.Fatal("a token did not reach the keychain")
	}

	back, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if back.Token != "tok_access" || back.Refresh != "tok_refresh" {
		t.Fatalf("loaded %+v", back)
	}

	// Signing out takes both copies with it.
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if kc.Has(tokenItem) || kc.Has(refreshItem) {
		t.Fatal("a token outlived the sign-out in the keychain")
	}
	if _, err := Load(); err != ErrNotLoggedIn {
		t.Fatalf("after Clear: %v", err)
	}
}

// A machine signed in before any of this keeps its file, and moves it the
// first time anything saves — which every sign-in and every token refresh
// does, so it is not a migration anybody has to run.
func TestACredentialWrittenBeforeTheKeychainIsMoved(t *testing.T) {
	home := machine(t)
	kc := keychain.NewMemory()
	defer keychain.Use(kc)()

	old := `{"backend_url":"https://john.acme.com","token":"tok_old","email":"bob@acme.com"}`
	if err := os.WriteFile(filepath.Join(home, "credentials.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "tok_old" {
		t.Fatalf("the old file was not read: %+v", c)
	}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	if got, err := kc.Get(tokenItem); err != nil || got != "tok_old" {
		t.Fatalf("keychain holds %q, %v", got, err)
	}
	if strings.Contains(readFile(t, home), "tok_old") {
		t.Fatal("the token is still in the file")
	}
}

// A keychain that will not take a secret is not a reason to fail a sign-in.
func TestAKeychainThatRefusesFallsBackToTheFile(t *testing.T) {
	home := machine(t)
	kc := keychain.NewMemory()
	kc.Mangle = func(string) string { return "not what you gave me" }
	defer keychain.Use(kc)()

	if err := Save(&Credentials{BackendURL: "https://john.acme.com", Token: "tok"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(readFile(t, home), "tok") {
		t.Fatal("the token went nowhere: not the keychain, not the file")
	}
	if c, err := Load(); err != nil || c.Token != "tok" {
		t.Fatalf("loaded %+v, %v", c, err)
	}
}
