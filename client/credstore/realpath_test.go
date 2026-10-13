package credstore

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/popstarapps/john/keychain"
	"github.com/popstarapps/john/registryauth"
)

// The production path against this machine's actual keychain.
//
// keychain/real_test.go proves a backend can hold a secret. This proves the
// two packages that keep secrets put them in it and take them out again — and
// those are different claims: a backend that round-trips perfectly is still
// wrong if Save leaves the token in the file beside it, or if Load cannot
// reassemble what Save took apart.
//
// It cannot use JOHN_HOME, which is what every other test here uses, because
// JOHN_HOME is precisely what turns the real store off (see
// keychain.realStoreWanted). So it writes to the developer's own ~/.john, and
// the price of that is the guard below: it refuses to run when either file
// already exists rather than write over a credential someone is signed in
// with. Sign out first if you want to run it on a machine that is signed in.
//
//	JOHN_KEYCHAIN_TEST=1 go test ./client/credstore -run TheRealKeychain -v
//
// Run it on each of macOS, Windows and a Linux desktop, beside the backend
// test. It leaves nothing behind: both keychain items and both files are gone
// at the end, and the files only if this test was the one that made them.
func TestTheRealKeychainHoldsTheProductionPath(t *testing.T) {
	if os.Getenv("JOHN_KEYCHAIN_TEST") == "" {
		t.Skip("set JOHN_KEYCHAIN_TEST=1 to exercise this machine's real keychain")
	}
	if h := os.Getenv("JOHN_HOME"); h != "" {
		t.Fatalf("JOHN_HOME=%s turns the real store off — unset it to run this", h)
	}
	if !keychain.Available() {
		t.Fatal("no keychain on this machine")
	}
	t.Logf("keychain: %s", keychain.Name())

	credPath, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	regPath, err := registryauth.StorePath()
	if err != nil {
		t.Fatal(err)
	}
	// The guard. Not a skip: someone who set the variable asked for this to
	// run, and "your machine is signed in" is a thing they need told.
	for _, p := range []string{credPath, regPath} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s exists — this test would overwrite it; sign out first", p)
		}
	}
	const host = "registry.selftest.invalid"
	t.Cleanup(func() {
		Clear()
		registryauth.Delete(host)
		os.Remove(credPath)
		os.Remove(regPath)
	})

	// A token with the characters that break a badly quoted write, since this
	// is the path a real one takes.
	want := &Credentials{
		BackendURL: "https://john.example.com",
		Token:      `tok_"quoted"_$HOME_\slash`,
		Email:      "oliver@example.com",
		Refresh:    "ref_'single'_`backtick`",
		ExpiresAt:  time.Now().Add(time.Hour).Round(time.Second),
	}
	if err := Save(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	// What is not secret stays in the file, and the secrets do not.
	var onDisk Credentials
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("reading %s: %v", credPath, err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Token != "" || onDisk.Refresh != "" {
		t.Errorf("a secret was left in %s: %s", credPath, raw)
	}
	if onDisk.BackendURL != want.BackendURL || onDisk.Email != want.Email {
		t.Errorf("the file lost what is not secret: %s", raw)
	}

	// And they are in the store, under the two names the package documents.
	if got, err := keychain.Get(tokenItem); err != nil || got != want.Token {
		t.Errorf("%s in the keychain: %q, %v", tokenItem, got, err)
	}
	if got, err := keychain.Get(refreshItem); err != nil || got != want.Refresh {
		t.Errorf("%s in the keychain: %q, %v", refreshItem, got, err)
	}

	// Load puts the two halves back together.
	got, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Token != want.Token || got.Refresh != want.Refresh {
		t.Errorf("load reassembled %q/%q, want %q/%q",
			got.Token, got.Refresh, want.Token, want.Refresh)
	}
	if got.BackendURL != want.BackendURL || got.Email != want.Email {
		t.Errorf("load lost the unsecret half: %+v", got)
	}

	// A save with no refresh token takes the old one out of the store rather
	// than leaving a stale secret behind it. This is the shape that found the
	// Linux delete bug, reached the way production reaches it.
	noRefresh := *want
	noRefresh.Refresh = ""
	if err := Save(&noRefresh); err != nil {
		t.Fatalf("save without a refresh token: %v", err)
	}
	if _, err := keychain.Get(refreshItem); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("the refresh token outlived a save without one: %v", err)
	}

	// A registry password takes the same split.
	const pw = `p@ss "word" $HOME`
	if err := registryauth.Save(host, registryauth.Credential{Username: "oliver", Secret: pw}); err != nil {
		t.Fatalf("registry save: %v", err)
	}
	regRaw, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatalf("reading %s: %v", regPath, err)
	}
	var reg struct {
		Registries map[string]struct{ Username, Secret string } `json:"registries"`
	}
	if err := json.Unmarshal(regRaw, &reg); err != nil {
		t.Fatal(err)
	}
	if e := reg.Registries[host]; e.Username != "oliver" || e.Secret != "" {
		t.Errorf("%s kept username=%q secret=%q, want the username and no secret",
			regPath, e.Username, e.Secret)
	}
	if cred, err := registryauth.Resolve(host); err != nil || cred.Secret != pw {
		t.Errorf("resolve: %q, %v — want the password back", cred.Secret, err)
	}

	// Saving a second registry must not drag the first one's secret out of the
	// keychain and into the file on the way past.
	const other = "registry.other.invalid"
	t.Cleanup(func() { registryauth.Delete(other) })
	if err := registryauth.Save(other, registryauth.Credential{Username: "someone", Secret: "second"}); err != nil {
		t.Fatalf("second registry save: %v", err)
	}
	regRaw, _ = os.ReadFile(regPath)
	if err := json.Unmarshal(regRaw, &reg); err != nil {
		t.Fatal(err)
	}
	for h, e := range reg.Registries {
		if e.Secret != "" {
			t.Errorf("saving %s put %s's secret in the file: %s", other, h, regRaw)
		}
	}

	if _, err := registryauth.Delete(host); err != nil {
		t.Fatalf("registry delete: %v", err)
	}
	if _, err := keychain.Get("registry:" + host); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("the registry password outlived the logout: %v", err)
	}

	// Clear empties both places.
	if err := Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := keychain.Get(tokenItem); !errors.Is(err, keychain.ErrNotFound) {
		t.Errorf("the machine token outlived Clear: %v", err)
	}
	if _, err := os.Stat(credPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s outlived Clear: %v", credPath, err)
	}
	if _, err := Load(); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("after Clear, Load says %v — want not signed in", err)
	}
}
