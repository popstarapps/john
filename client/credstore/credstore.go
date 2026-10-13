// Package credstore holds what `john login` leaves on a developer machine:
// the backend URL and the machine token.
//
// The two secrets go to the operating system's keychain where there is one,
// and to a 0600 file where there is not. What is not secret stays in the file
// either way — which backend this machine is signed in to, as whom, and until
// when — because that is what anyone opening the file is looking for, and a
// file that answers nothing invites a developer to go looking for the token.
//
// The machine token is the only credential john ever stores. There is no
// model provider key anywhere on the machine, because the client never talks
// to a model provider.
package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/popstarapps/john/keychain"
)

// What the two secrets are called in the keychain. They are separate items
// because they have separate lives: the access token is spent on every request
// and expires in hours, the refresh token is presented to one endpoint and has
// to survive a laptop being shut for a fortnight.
const (
	tokenItem   = "machine-token"
	refreshItem = "refresh-token"
)

// Credentials is the stored state: where this machine signed in, and the
// pair it holds.
//
// The two secrets are not interchangeable. Token goes out on every request
// and expires in hours; Refresh is presented only to the backend's refresh
// endpoint, rotates each time, and is the one that has to survive a laptop
// being shut for a fortnight.
type Credentials struct {
	BackendURL string `json:"backend_url"`
	Token      string `json:"token"`
	Email      string `json:"email"` // display only

	Refresh          string    `json:"refresh_token,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitzero"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitzero"`
}

// ErrNotLoggedIn means nothing is stored: run `john login`.
var ErrNotLoggedIn = errors.New("not signed in — run `john login`")

// Path returns the credentials file location, honouring JOHN_HOME for tests.
func Path() (string, error) {
	if h := os.Getenv("JOHN_HOME"); h != "" {
		return filepath.Join(h, "credentials.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".john", "credentials.json"), nil
}

// Load reads the stored credentials.
func Load() (*Credentials, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("credstore: unreadable %s: %w", p, err)
	}

	// A secret in the file means one of two things, and both are handled by
	// leaving it where it is: this machine has no keychain, or it has one and
	// the credential predates it. The second is migrated on the next Save,
	// which every sign-in and every refresh performs.
	if c.Token == "" {
		if tok, err := keychain.Get(tokenItem); err == nil {
			c.Token = tok
		}
	}
	if c.Refresh == "" {
		if ref, err := keychain.Get(refreshItem); err == nil {
			c.Refresh = ref
		}
	}

	if c.BackendURL == "" || c.Token == "" {
		return nil, ErrNotLoggedIn
	}
	return &c, nil
}

// Save stores the credentials: the secrets in the keychain where this machine
// has one, the rest in a file with owner-only permissions.
//
// A keychain that will not take a secret is not a reason to fail a sign-in. It
// is a reason to keep the secret the way john always did, which is what the
// file has always been — so the value is written there instead and the machine
// carries on.
func Save(c *Credentials) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}

	onDisk := *c
	if keychain.Available() {
		if keychain.Set(tokenItem, c.Token) == nil {
			onDisk.Token = ""
		}
		if c.Refresh == "" {
			keychain.Delete(refreshItem)
		} else if keychain.Set(refreshItem, c.Refresh) == nil {
			onDisk.Refresh = ""
		}
	}

	data, err := json.MarshalIndent(&onDisk, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// Clear removes stored credentials, from both places they can be.
func Clear() error {
	p, err := Path()
	if err != nil {
		return err
	}
	keychain.Delete(tokenItem)
	keychain.Delete(refreshItem)
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
