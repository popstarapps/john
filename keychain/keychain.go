// Package keychain stores John's machine tokens and registry passwords in the
// operating system's credential store.
//
// The keychain is used when available; JOHN_KEYCHAIN=off disables it.
// Every write is read back to verify that the secret can be recovered. If the
// keychain is unavailable or verification fails, callers store the secret in
// a file with mode 0600. Non-secret metadata, such as the backend URL and
// account name, remains in ~/.john/credentials.json.
package keychain

import (
	"errors"
	"os"
	"strings"
	"sync"
)

// ErrNotFound means the keychain has no such item. It is not an error about
// the keychain: it is the answer for a machine that has not signed in.
var ErrNotFound = errors.New("keychain: no such item")

// Backend is one operating system's secret store. Implementations are per
// platform and are chosen at start; tests install their own with Use.
type Backend interface {
	// Name is what to call this store in something a person reads.
	Name() string
	// Available reports whether this machine can actually use it. A backend
	// exists on a platform; it is available on a machine.
	Available() bool
	Get(name string) (string, error)
	Set(name, secret string) error
	Delete(name string) error
}

var (
	mu      sync.Mutex
	current Backend = platformBackend()
)

// Use installs a backend and returns a function restoring the previous one.
// It is how the tests exercise the seam without a keychain, and how a test
// can be sure the file fallback is the file fallback.
func Use(b Backend) (restore func()) {
	mu.Lock()
	defer mu.Unlock()
	prev := current
	current = b
	return func() {
		mu.Lock()
		defer mu.Unlock()
		current = prev
	}
}

func backend() Backend {
	mu.Lock()
	defer mu.Unlock()
	return current
}

// Available reports whether secrets should go to the keychain on this machine.
//
// JOHN_KEYCHAIN=off (or 0, no, none, false) turns it off. That exists for the
// developer whose keychain asks for a password more often than they can stand,
// and for a headless machine whose store is present but locked — a credential
// that cannot be read is worse than one kept in a file the owner can read.
func Available() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("JOHN_KEYCHAIN"))) {
	case "off", "0", "no", "none", "false":
		return false
	}
	b := backend()
	return b != nil && b.Available()
}

// Name is what the keychain on this machine is called, for a line telling a
// developer where their credential went. It is empty when there is none.
func Name() string {
	if !Available() {
		return ""
	}
	return backend().Name()
}

// Get reads a secret, answering ErrNotFound when there is none.
func Get(name string) (string, error) {
	if !Available() {
		return "", ErrNotFound
	}
	return backend().Get(name)
}

// Set stores a secret and reads it back before reporting success.
//
// The read-back is the whole reason a backend that has never run on this
// machine is safe to have here. A store that accepts a write and returns
// something else — a quoting rule nobody guessed right, a length limit, a
// daemon that is present but refusing — is discovered at the moment the
// credential is saved, by the code that still has the credential in hand, and
// the caller writes it to its file instead.
func Set(name, secret string) error {
	if !Available() {
		return ErrNotFound
	}
	b := backend()
	if err := b.Set(name, secret); err != nil {
		return err
	}
	got, err := b.Get(name)
	if err != nil {
		return err
	}
	if got != secret {
		// Leave nothing half-written behind: an item holding a mangled secret
		// would be found by the next Get and believed.
		b.Delete(name)
		return errors.New(b.Name() + " did not return the secret it was given")
	}
	return nil
}

// Delete forgets a secret. A secret that is not there is not an error: the
// outcome asked for is that it is gone.
func Delete(name string) error {
	if !Available() {
		return nil
	}
	err := backend().Delete(name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// realStoreWanted is asked by the three real backends, and by none of the
// ones tests install.
//
// JOHN_HOME is what says "keep john's state in this directory" — it is how the
// tests run, and how a machine with an unusual home is set up. A secret in the
// operating system's store is not in that directory, so honouring the variable
// means not using one. It also means `go test` on a developer's own laptop
// cannot write to, or overwrite, the credential they are actually signed in
// with.
func realStoreWanted() bool { return os.Getenv("JOHN_HOME") == "" }

// Memory is a keychain in a map. It is here rather than in a test file
// because the packages that need it — the two that keep secrets — are not this
// one, and a seam nobody else can install is not a seam.
type Memory struct {
	mu    sync.Mutex
	items map[string]string
	// Mangle makes Get answer with something other than what Set was given,
	// which is what a backend with a quoting bug does.
	Mangle func(string) string
	// Fail makes Set fail, which is what a locked store does.
	Fail error
}

func NewMemory() *Memory { return &Memory{items: map[string]string{}} }

func (m *Memory) Name() string    { return "an in-memory keychain" }
func (m *Memory) Available() bool { return true }

func (m *Memory) Get(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.items[name]
	if !ok {
		return "", ErrNotFound
	}
	if m.Mangle != nil {
		return m.Mangle(v), nil
	}
	return v, nil
}

func (m *Memory) Set(name, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	m.items[name] = secret
	return nil
}

func (m *Memory) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, name)
	return nil
}

// Has reports whether an item is stored, for a test asserting where a secret
// ended up rather than what it is.
func (m *Memory) Has(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.items[name]
	return ok
}

// missingKeychainAdvice names what a machine would need before john could use
// its keychain. It is for the one test that talks to the real store: a
// developer who ran it deliberately and got nothing needs to know whether that
// is a missing package or a missing desktop session.
func missingKeychainAdvice() string {
	if os.Getenv("JOHN_HOME") != "" {
		return "JOHN_HOME is set, which turns the real store off"
	}
	return missingBackendAdvice()
}

// none is the backend for a platform with no store john knows how to use.
type none struct{}

func (none) Name() string               { return "" }
func (none) Available() bool            { return false }
func (none) Get(string) (string, error) { return "", ErrNotFound }
func (none) Set(string, string) error   { return ErrNotFound }
func (none) Delete(string) error        { return nil }
