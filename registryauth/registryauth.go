// Package registryauth is how john signs in to a container registry.
//
// It reads the credential the developer already has rather than asking for a
// second copy of it. `docker login` has been the way to sign in to a registry
// for a decade, and the file it writes is where every other tool looks; a
// developer who can pull the image can already pull it, and asking them to
// paste the same password into john would only mean two copies of a secret
// going stale at different rates.
//
// Reading the file is not enough on its own. ECR, Artifact Registry and ACR
// store no password at all: they name a helper program, and the helper mints a
// token that lives minutes. So the helper is the only thing that CAN produce a
// working credential for most organizations, and a reader that parsed the
// config and never ran one would work on a demo and fail at every customer.
//
// The order for one host is: what `john registry login` stored, then docker's
// credHelpers entry for that host, then its credsStore, then a password in its
// auths. john's own store comes first because typing `john registry login` is
// an explicit act about one registry and ambient configuration should not
// quietly outrank it.
//
// WHERE THE CREDENTIAL IS USED matters as much as where it comes from. The
// base image is pulled by the HOST, because that is where the proxy is: the
// credential is read here, spent on an Authorization header there, and never
// enters the sandbox. The machine is handed layers, not a password.
package registryauth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Credential is what a registry is signed in to with.
//
// Identity says the secret is a token the registry itself issued rather than a
// password: it is exchanged at the token endpoint for an access token instead
// of being sent as one. docker writes it as `identitytoken`, and a credential
// helper says the same thing by answering with the username `<token>`.
type Credential struct {
	Username string
	Secret   string
	Identity bool
}

// Empty means there is nothing to sign in with, which is not an error: most
// base images are public and anonymous access is the ordinary case.
func (c Credential) Empty() bool { return c.Username == "" && c.Secret == "" }

// Found is a credential and where it came from. The second half is for
// `john registry list`, which has to be able to say why a registry works
// without printing anything secret.
type Found struct {
	Credential
	From string
}

// Resolve finds the credential for one registry host, or the zero Credential
// when there is none to find.
func Resolve(host string) (Credential, error) {
	f, err := Lookup(host)
	return f.Credential, err
}

// Lookup is Resolve, and says where the answer came from.
func Lookup(host string) (Found, error) {
	return lookup(context.Background(), host)
}

func lookup(ctx context.Context, host string) (Found, error) {
	key := canonicalHost(host)

	// What this machine was explicitly told, first.
	stored, err := loadStore()
	if err != nil {
		return Found{}, err
	}
	if c, ok := stored.Registries[key]; ok {
		return Found{c.credential(key), "john registry login"}, nil
	}

	cfg, path, err := loadDockerConfig()
	if err != nil {
		return Found{}, err
	}
	if cfg == nil {
		return Found{}, nil
	}
	if name := cfg.helperFor(key); name != "" {
		cred, ok, err := runHelper(ctx, name, serverFor(key))
		if err != nil {
			return Found{}, err
		}
		if !ok {
			// A helper that holds nothing for this registry is a registry
			// this machine has not signed in to, not a broken machine.
			return Found{}, nil
		}
		return Found{cred, "docker-credential-" + name}, nil
	}
	if cred, ok := cfg.auth(key); ok {
		return Found{cred, path}, nil
	}
	return Found{}, nil
}

// Listing is one configured registry, for `john registry list`. Username is
// empty for a registry served by a helper: asking would pop a keychain prompt,
// and listing what is configured should not cost the developer a password.
type Listing struct {
	Host     string
	From     string
	Username string
}

// Configured is every registry this machine has a credential for, without
// spending any of them.
func Configured() ([]Listing, error) {
	var out []Listing
	seen := map[string]bool{}
	add := func(l Listing) {
		if !seen[l.Host] {
			seen[l.Host] = true
			out = append(out, l)
		}
	}

	stored, err := loadStore()
	if err != nil {
		return nil, err
	}
	for host, e := range stored.Registries {
		add(Listing{Host: host, From: "john registry login", Username: e.Username})
	}

	cfg, path, err := loadDockerConfig()
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		for host, name := range cfg.CredHelpers {
			add(Listing{Host: canonicalHost(host), From: "docker-credential-" + name})
		}
		for host := range cfg.Auths {
			cred, _ := cfg.auth(canonicalHost(host))
			add(Listing{Host: canonicalHost(host), From: path, Username: cred.Username})
		}
		if cfg.CredsStore != "" {
			add(Listing{Host: "(any other registry)", From: "docker-credential-" + cfg.CredsStore})
		}
	}
	return out, nil
}

// dockerHubHost is what an unqualified image name resolves to, and
// dockerHubKey is what docker has stored Docker Hub under since before the
// registry moved to that name. Every helper and config file on a developer's
// machine still uses the old key, so a lookup for the modern host has to ask
// for the old one.
const (
	dockerHubHost = "registry-1.docker.io"
	dockerHubKey  = "https://index.docker.io/v1/"
)

// canonicalHost reduces both a registry host and a config file's key to the
// same string, so that "https://index.docker.io/v1/", "index.docker.io" and
// "registry-1.docker.io" are one registry and not three.
func canonicalHost(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s, _, _ = strings.Cut(s, "/")
	if s == "index.docker.io" || s == "docker.io" || s == "registry.hub.docker.com" {
		return dockerHubHost
	}
	return s
}

// serverFor is what a credential helper is asked about. Helpers key on the
// string docker gave them, which for Docker Hub is the legacy index URL.
func serverFor(host string) string {
	if host == dockerHubHost {
		return dockerHubKey
	}
	return host
}

// johnHome is ~/.john, or JOHN_HOME where a test sets it. The same rule as
// client/credstore, repeated rather than imported: this package is used by the
// sandbox, which has no business depending on the client.
func johnHome() (string, error) {
	if h := os.Getenv("JOHN_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("registryauth: %w", err)
	}
	return filepath.Join(home, ".john"), nil
}
