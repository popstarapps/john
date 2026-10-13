package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The machine's choice, made once. Plain `john` is the command people
// actually type, and it needs to know which john this machine is running:
// solo, with a backend in the same process, or signed in to an
// organization's. Asking every time would be noise, and guessing would be
// worse — the two behave differently enough that a wrong guess is confusing
// rather than convenient.
//
// This is deliberately not part of Credentials. A standalone machine holds
// no credentials at all, and a machine that has been signed out still has a
// mode; conflating them would make one of those states unrepresentable.

// Machine modes.
const (
	ModeStandalone = "standalone"
	ModeBackend    = "backend"
)

// What became of the offer to grant this machine the right to create virtual
// machines. It is recorded so the question is asked once rather than at every
// start; an explicit `john -sandbox vm` asks again regardless, because typing
// it is a developer saying they want the machine.
const (
	SandboxAdminGranted  = "granted"
	SandboxAdminDeclined = "declined"
)

// Setup is the per-machine choice, at ~/.john/setup.json.
type Setup struct {
	Mode string `json:"mode"`
	// SandboxAdmin is empty until the question has been put.
	SandboxAdmin string `json:"sandbox_admin,omitempty"`
}

// ErrNoSetup means this machine has never been told which john to run.
var ErrNoSetup = errors.New("credstore: this machine has no john setup yet")

// SetupPath is where the choice lives, beside the credentials.
func SetupPath() (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "setup.json"), nil
}

// LoadSetup reads the machine's choice, or ErrNoSetup on a fresh machine.
func LoadSetup() (*Setup, error) {
	p, err := SetupPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSetup
	}
	if err != nil {
		return nil, err
	}
	var s Setup
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("credstore: unreadable %s: %w", p, err)
	}
	if s.Mode != ModeStandalone && s.Mode != ModeBackend {
		return nil, ErrNoSetup
	}
	return &s, nil
}

// SaveSetup records the machine's choice.
func SaveSetup(s *Setup) error {
	p, err := SetupPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}
