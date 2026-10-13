package registryauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ~/.docker/config.json, which is the file john reads rather than inventing a
// place of its own.
//
// Three of its fields matter. `auths` holds a base64 user:password per
// registry, and sometimes an identity token instead. `credHelpers` names a
// helper program for one registry, and `credsStore` names one for all the
// rest — those two are how every cloud registry works, and the reason this
// package runs programs at all.
type dockerConfig struct {
	Auths       map[string]dockerAuth `json:"auths"`
	CredsStore  string                `json:"credsStore"`
	CredHelpers map[string]string     `json:"credHelpers"`
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// dockerConfigPath honours DOCKER_CONFIG, which is how CI and anything using a
// throwaway config directory points tools at it.
func dockerConfigPath() (string, error) {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("registryauth: %w", err)
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

// loadDockerConfig reads Docker credentials and returns the config and path.
// Missing files return a nil config. Read and parse failures return an error.
func loadDockerConfig() (*dockerConfig, string, error) {
	path, err := dockerConfigPath()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, path, nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("registryauth: reading %s: %w", path, err)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, path, fmt.Errorf("registryauth: %s is not valid JSON: %w", path, err)
	}
	return &cfg, path, nil
}

// helperFor names the helper program for one registry: the one named for it,
// or the one named for everything.
func (c *dockerConfig) helperFor(host string) string {
	for key, name := range c.CredHelpers {
		if canonicalHost(key) == host && name != "" {
			return name
		}
	}
	return c.CredsStore
}

// auth reads a stored password for one registry.
func (c *dockerConfig) auth(host string) (Credential, bool) {
	for key, a := range c.Auths {
		if canonicalHost(key) != host {
			continue
		}
		if a.IdentityToken != "" {
			// docker stores the username beside an identity token and does
			// not use it; the token is the whole credential.
			return Credential{Username: a.Username, Secret: a.IdentityToken, Identity: true}, true
		}
		user, pass := a.Username, a.Password
		if a.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Auth))
			if err != nil {
				// Not fatal: an entry john cannot read is one registry that
				// falls through to anonymous, not a machine that cannot pull.
				continue
			}
			if u, p, ok := strings.Cut(string(decoded), ":"); ok {
				user, pass = u, p
			}
		}
		if user == "" && pass == "" {
			// An empty entry is what `docker logout` leaves behind.
			continue
		}
		return Credential{Username: user, Secret: pass}, true
	}
	return Credential{}, false
}
