package registryauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Credential helpers, which is how every cloud registry is actually signed in
// to.
//
// The protocol is docker's and is two lines long: run docker-credential-<name>
// with the argument `get`, write the registry to its standard input, and read
// a JSON object back. ECR, Artifact Registry and ACR all answer with a token
// that expires in minutes, which is why nothing a helper returns is ever
// written to disk here — a cached one would be a stale-credential bug that
// looks exactly like a broken registry.

// helperTimeout is generous because some helpers reach the network (ECR mints
// a token through an API call) and short enough that a wedged one fails the
// build rather than hanging it.
const helperTimeout = 30 * time.Second

// helperOutput is what the protocol says comes back.
type helperOutput struct {
	ServerURL string
	Username  string
	Secret    string
}

// tokenUsername is the username a helper answers with when the secret is an
// identity token rather than a password. It is a convention, and it is the
// only way the caller can tell the two apart.
const tokenUsername = "<token>"

// runHelper asks one helper for one registry. The bool is false when the
// helper simply has nothing for that registry, which is not an error.
func runHelper(ctx context.Context, name, server string) (Credential, bool, error) {
	if err := validHelperName(name); err != nil {
		return Credential{}, false, err
	}
	bin := "docker-credential-" + name
	path, err := exec.LookPath(bin)
	if err != nil {
		return Credential{}, false, fmt.Errorf("registryauth: the docker configuration names "+
			"%s as its credential helper and it is not installed on this machine (%w)", bin, err)
	}

	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "get")
	cmd.Stdin = strings.NewReader(server)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()

	// "credentials not found" is the protocol's way of saying this machine has
	// not signed in to that registry, and every helper writes it. It is the
	// answer for any registry a developer has not logged in to, so it must not
	// be an error or a public pull on a machine with a keychain would fail.
	if notFound(out.String(), errOut.String()) {
		return Credential{}, false, nil
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Credential{}, false, fmt.Errorf("registryauth: %s did not answer within %s",
				bin, helperTimeout)
		}
		// The helper's own words, never its standard output — that is where
		// the secret would be.
		return Credential{}, false, fmt.Errorf("registryauth: %s failed for %s: %v: %s",
			bin, server, err, firstLine(errOut.String()))
	}

	var got helperOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		return Credential{}, false, fmt.Errorf("registryauth: %s answered with something that is "+
			"not the credential-helper protocol (%d bytes of it)", bin, out.Len())
	}
	if got.Secret == "" {
		return Credential{}, false, nil
	}
	if got.Username == tokenUsername {
		return Credential{Secret: got.Secret, Identity: true}, true, nil
	}
	return Credential{Username: got.Username, Secret: got.Secret}, true, nil
}

// validHelperName keeps a configuration file from naming a program rather than
// a helper. The name is joined to "docker-credential-" and resolved through
// PATH like every other tool does it, so anything with a separator or a dot
// segment in it is refused before it can become a path.
func validHelperName(name string) error {
	if name == "" {
		return errors.New("registryauth: the docker configuration names an empty credential helper")
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("registryauth: %q is not a credential helper name — john runs "+
				"docker-credential-<name> from PATH, and a name is letters, digits, - and _", name)
		}
	}
	return nil
}

func notFound(out, errOut string) bool {
	s := strings.ToLower(out + "\n" + errOut)
	return strings.Contains(s, "credentials not found") ||
		strings.Contains(s, "no credentials found") ||
		strings.Contains(s, "credentials not found in native keychain")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
