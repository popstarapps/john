package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

// Identity is this machine's trace key pair: the private half never leaves
// the machine, and the recipient is registered with the backend so sessions
// are wrapped for it.
//
// It is not a credential for reaching the backend — that is the machine token
// — and losing it costs read access to past sessions, nothing else. An admin
// holding the organization key can grant a new key access to old sessions.
type Identity struct {
	Private   string // AGE-SECRET-KEY-1…
	Recipient string // age1…
}

// IdentityPath is where the private half lives, beside the credentials.
func IdentityPath() (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "trace-identity.txt"), nil
}

// LoadIdentity reads the stored identity. It returns ErrNoIdentity when there
// is none, which is not an error worth reporting to anyone: EnsureIdentity
// makes one.
func LoadIdentity() (*Identity, error) {
	p, err := IdentityPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoIdentity
	}
	if err != nil {
		return nil, err
	}
	id, err := age.ParseX25519Identity(strings.TrimSpace(firstLine(string(data))))
	if err != nil {
		return nil, ErrNoIdentity
	}
	return &Identity{Private: id.String(), Recipient: id.Recipient().String()}, nil
}

// ErrNoIdentity means this machine has never generated a trace key.
var ErrNoIdentity = errors.New("credstore: no trace identity on this machine")

// EnsureIdentity returns the machine's identity, generating one on first use.
// It is stable across sign-ins: signing in again does not re-key, because
// re-keying would quietly cost the developer every session they could read.
func EnsureIdentity() (*Identity, error) {
	if id, err := LoadIdentity(); err == nil {
		return id, nil
	}
	gen, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	p, err := IdentityPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, []byte(gen.String()+"\n"), 0o600); err != nil {
		return nil, err
	}
	return &Identity{Private: gen.String(), Recipient: gen.Recipient().String()}, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
