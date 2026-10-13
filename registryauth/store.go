package registryauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/popstarapps/john/keychain"
)

// secretItem is what one registry's password is called in the keychain. The
// registry is in the name because a developer may be signed in to several.
func secretItem(host string) string { return "registry:" + host }

// john's own store, written by `john registry login`.
//
// It exists for the machine that has no docker installed, which is most of
// them now that john builds the image itself. The shape is docker's, so a
// developer reading both files sees the same thing twice rather than two
// designs.
//
// The password goes to the operating system's keychain where there is one,
// under the registry it belongs to, and the file keeps the username and which
// registries have been signed in to. That is the same split the machine token
// takes, and it means a registry served by a docker credential HELPER and one
// served by `john registry login` both end up in the same place — the
// developer's keychain — rather than one of them in a file.
//
// It holds a password only when a person typed one. Nothing a credential
// helper hands back is ever stored here: those tokens expire in minutes.

const storeFile = "registries.json"

type store struct {
	Registries map[string]storeEntry `json:"registries"`
}

type storeEntry struct {
	Username string `json:"username"`
	Secret   string `json:"secret"`
	Identity bool   `json:"identity,omitempty"`
}

// credential fills in the secret, which the file holds only on a machine with
// no keychain — or for a login that predates one, which stays where it is
// until the next `john registry login` for that registry moves it.
//
// The keychain is read here rather than when the file is loaded, so that
// saving one registry cannot write another registry's secret back into the
// file on the way past.
func (e storeEntry) credential(key string) Credential {
	secret := e.Secret
	if secret == "" {
		if fromKeychain, err := keychain.Get(secretItem(key)); err == nil {
			secret = fromKeychain
		}
	}
	return Credential{Username: e.Username, Secret: secret, Identity: e.Identity}
}

// StorePath is where the file lives, so an error message can name it.
func StorePath() (string, error) {
	home, err := johnHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, storeFile), nil
}

func loadStore() (store, error) {
	s := store{Registries: map[string]storeEntry{}}
	path, err := StorePath()
	if err != nil {
		return s, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("registryauth: reading %s: %w", path, err)
	}
	var onDisk store
	if err := json.Unmarshal(data, &onDisk); err != nil {
		return s, fmt.Errorf("registryauth: %s is not valid JSON: %w", path, err)
	}
	for host, e := range onDisk.Registries {
		s.Registries[canonicalHost(host)] = e
	}
	return s, nil
}

func saveStore(s store) error {
	path, err := StorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Save records a credential for one registry, replacing whatever was there.
// The password goes to the keychain when this machine has one, and to the
// file when it does not — a keychain that will not take it is a reason to
// keep the credential the old way, not a reason to refuse the login.
func Save(host string, c Credential) error {
	if c.Empty() {
		return errors.New("registryauth: nothing to save")
	}
	s, err := loadStore()
	if err != nil {
		return err
	}
	key := canonicalHost(host)
	entry := storeEntry(c)
	if keychain.Available() && keychain.Set(secretItem(key), c.Secret) == nil {
		entry.Secret = ""
	}
	s.Registries[key] = entry
	return saveStore(s)
}

// Delete forgets one registry, and says whether there was one to forget.
func Delete(host string) (bool, error) {
	s, err := loadStore()
	if err != nil {
		return false, err
	}
	key := canonicalHost(host)
	if _, ok := s.Registries[key]; !ok {
		return false, nil
	}
	delete(s.Registries, key)
	keychain.Delete(secretItem(key))
	return true, saveStore(s)
}
