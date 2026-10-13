//go:build linux

package keychain

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// libsecret, through secret-tool.
//
// The desktop keyring — GNOME Keyring, KWallet's secret service — reached the
// way its own tools reach it. Talking D-Bus directly would mean a dependency
// and a protocol to keep up with, for the same three operations.
//
// Two things make it unavailable rather than broken, and both are ordinary:
// a Linux machine with no desktop has no secret service at all, and a server
// or a container has no session bus for one to live on. Either way the secret
// goes to the file, which is where it was before this existed.
type secretTool struct{}

func platformBackend() Backend { return secretTool{} }

func (secretTool) Name() string { return "the desktop keyring (libsecret)" }

func (secretTool) Available() bool {
	if !realStoreWanted() || os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return false
	}
	_, err := exec.LookPath("secret-tool")
	return err == nil
}

func (secretTool) Get(name string) (string, error) {
	out, err := exec.Command("secret-tool", "lookup", "service", "john", "account", name).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", ErrNotFound
		}
		return "", err
	}
	// secret-tool writes the secret with no trailing newline, so nothing is
	// trimmed here: a secret may legitimately end in one.
	return string(out), nil
}

func (secretTool) Set(name, secret string) error {
	cmd := exec.Command("secret-tool", "store", "--label=john ("+name+")",
		"service", "john", "account", name)
	cmd.Stdin = strings.NewReader(secret)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return errors.New("secret-tool: " + strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (secretTool) Delete(name string) error {
	err := exec.Command("secret-tool", "clear", "service", "john", "account", name).Run()
	// `secret-tool clear` exits 1 when nothing matched, which is the ordinary
	// case for a machine that never had the item — a sign-out that only ever
	// held an access token, or a registry logged out twice. It uses the same
	// exit status for a real failure, so the two cannot be told apart; not
	// found is the reading that matches what Delete promises, which is that
	// the item is gone.
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ErrNotFound
	}
	return err
}

// missingBackendAdvice separates the two ordinary reasons a Linux machine has
// no keyring, because they need different things done about them.
func missingBackendAdvice() string {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return "there is no session bus here (DBUS_SESSION_BUS_ADDRESS is unset), " +
			"so there is no desktop keyring to talk to — this is a server or an ssh session"
	}
	return "secret-tool is not installed (Debian/Ubuntu: libsecret-tools, Fedora: libsecret)"
}
