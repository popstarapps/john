package registryauth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/popstarapps/john/keychain"
)

// A machine with a docker configuration and a john store, both empty until a
// test fills them in.
func newMachine(t *testing.T) (johnHome, dockerHome string) {
	t.Helper()
	johnHome, dockerHome = t.TempDir(), t.TempDir()
	t.Setenv("JOHN_HOME", johnHome)
	t.Setenv("DOCKER_CONFIG", dockerHome)
	return johnHome, dockerHome
}

func writeDockerConfig(t *testing.T, dir string, cfg map[string]any) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// The credential a person typed into john outranks the one that happens to be
// on the machine. Both are the same registry here, which is the only case
// where the order can be seen at all.
func TestWhatJohnWasToldOutranksWhatItFound(t *testing.T) {
	_, docker := newMachine(t)
	writeDockerConfig(t, docker, map[string]any{
		"auths": map[string]any{"ghcr.io": map[string]any{"auth": basicAuth("ambient", "p1")}},
	})
	if err := Save("ghcr.io", Credential{Username: "typed", Secret: "p2"}); err != nil {
		t.Fatal(err)
	}

	found, err := Lookup("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if found.Username != "typed" || found.Secret != "p2" {
		t.Fatalf("resolved %q, want the stored one", found.Username)
	}
	if found.From != "john registry login" {
		t.Errorf("from = %q", found.From)
	}

	// And forgetting john's own falls back to the machine's, rather than to
	// nothing: a logout from john is not a logout from docker.
	if removed, err := Delete("ghcr.io"); err != nil || !removed {
		t.Fatalf("delete = %v, %v", removed, err)
	}
	found, err = Lookup("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if found.Username != "ambient" {
		t.Fatalf("after logout resolved %q, want the docker credential", found.Username)
	}
}

// docker writes the password two ways and john reads both, because a config
// file written by an older docker, or by hand, is still a config file.
func TestADockerPasswordIsReadHoweverItWasWritten(t *testing.T) {
	_, docker := newMachine(t)
	writeDockerConfig(t, docker, map[string]any{
		"auths": map[string]any{
			"registry.internal:8443": map[string]any{"auth": basicAuth("bob", "s3cret")},
			"harbor.example.com":     map[string]any{"username": "alice", "password": "hunter2"},
			"stale.example.com":      map[string]any{},
		},
	})

	for _, c := range []struct{ host, user, secret string }{
		{"registry.internal:8443", "bob", "s3cret"},
		{"harbor.example.com", "alice", "hunter2"},
	} {
		got, err := Resolve(c.host)
		if err != nil {
			t.Fatalf("%s: %v", c.host, err)
		}
		if got.Username != c.user || got.Secret != c.secret {
			t.Errorf("%s → %q/%q", c.host, got.Username, got.Secret)
		}
	}

	// An entry `docker logout` emptied is not a credential, and offering it
	// would turn an anonymous pull that works into a 401 that does not.
	if got, err := Resolve("stale.example.com"); err != nil || !got.Empty() {
		t.Errorf("an emptied entry resolved to %+v (%v)", got, err)
	}
}

// Docker Hub is one registry however it is spelled. The name in a FROM line,
// the name the registry answers on, and the name docker has stored it under
// since before that are three different strings.
func TestDockerHubIsOneRegistryHoweverItIsSpelled(t *testing.T) {
	for _, s := range []string{
		"https://index.docker.io/v1/", "index.docker.io", "docker.io",
		"registry-1.docker.io", "registry.hub.docker.com",
	} {
		if got := canonicalHost(s); got != dockerHubHost {
			t.Errorf("canonicalHost(%q) = %q", s, got)
		}
	}
	if got := serverFor(dockerHubHost); got != dockerHubKey {
		t.Errorf("a helper would be asked about %q", got)
	}

	_, docker := newMachine(t)
	writeDockerConfig(t, docker, map[string]any{
		"auths": map[string]any{dockerHubKey: map[string]any{"auth": basicAuth("bob", "p")}},
	})
	got, err := Resolve("registry-1.docker.io")
	if err != nil || got.Username != "bob" {
		t.Fatalf("the legacy Docker Hub key did not answer for the modern host: %+v (%v)", got, err)
	}
}

// An identity token is not a password: it is exchanged at the token endpoint,
// and sending it as one would fail against every registry that issues them.
func TestAnIdentityTokenSaysThatItIsOne(t *testing.T) {
	_, docker := newMachine(t)
	writeDockerConfig(t, docker, map[string]any{
		"auths": map[string]any{
			"ghcr.io": map[string]any{"username": "<token>", "identitytoken": "eyJ0"},
		},
	})
	got, err := Resolve("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Identity || got.Secret != "eyJ0" {
		t.Fatalf("%+v", got)
	}
}

// helperScript puts a docker-credential-<name> on PATH that answers with
// whatever the test says.
func helperScript(t *testing.T, name, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the helper protocol is the same on Windows, but a shell script is not")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "docker-credential-"+name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The helper is RUN, not read. ECR, Artifact Registry and ACR store no
// password at all — the helper mints a token that lives minutes — so a reader
// that only parsed the config file would work on a demo and fail everywhere
// real.
func TestACredentialHelperIsRunRatherThanRead(t *testing.T) {
	_, docker := newMachine(t)
	helperScript(t, "fake", `
read server
echo "{\"ServerURL\":\"$server\",\"Username\":\"AWS\",\"Secret\":\"minted-for-$server\"}"`)
	writeDockerConfig(t, docker, map[string]any{"credsStore": "fake"})

	found, err := Lookup("123.dkr.ecr.eu-west-1.amazonaws.com")
	if err != nil {
		t.Fatal(err)
	}
	if found.Username != "AWS" || !strings.HasPrefix(found.Secret, "minted-for-") {
		t.Fatalf("%+v", found.Credential)
	}
	if found.From != "docker-credential-fake" {
		t.Errorf("from = %q", found.From)
	}
	// The helper is asked about the registry by the name docker uses for it.
	if !strings.Contains(found.Secret, "123.dkr.ecr") {
		t.Errorf("the helper was asked about the wrong server: %q", found.Secret)
	}
}

// A helper that holds nothing for a registry is a registry this machine has
// not signed in to. If that were an error, a public pull on any machine with a
// keychain would fail.
func TestAHelperWithNothingToSayIsNotAnError(t *testing.T) {
	_, docker := newMachine(t)
	helperScript(t, "empty", `
read server
echo "credentials not found in native keychain" >&2
exit 1`)
	writeDockerConfig(t, docker, map[string]any{"credsStore": "empty"})

	got, err := Resolve("ghcr.io")
	if err != nil {
		t.Fatalf("a helper with nothing to say was an error: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("%+v", got)
	}
}

// credHelpers is per registry and outranks credsStore, which is the fallback
// for everything else. Getting this backwards would send an organization's
// registry password to the wrong helper.
func TestTheHelperForOneRegistryBeatsTheOneForAllOfThem(t *testing.T) {
	_, docker := newMachine(t)
	helperScript(t, "specific", `
read server
echo '{"Username":"specific","Secret":"s"}'`)
	// Both helpers live on the same PATH entry, so write the second one
	// beside the first.
	dir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	if err := os.WriteFile(filepath.Join(dir, "docker-credential-general"),
		[]byte("#!/bin/sh\nread s\necho '{\"Username\":\"general\",\"Secret\":\"s\"}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDockerConfig(t, docker, map[string]any{
		"credsStore":  "general",
		"credHelpers": map[string]any{"ghcr.io": "specific"},
	})

	if got, _ := Resolve("ghcr.io"); got.Username != "specific" {
		t.Errorf("ghcr.io went to %q", got.Username)
	}
	if got, _ := Resolve("other.example.com"); got.Username != "general" {
		t.Errorf("another registry went to %q", got.Username)
	}
}

// A helper answering with the token username means the secret is a token.
func TestAHelperCanAnswerWithAToken(t *testing.T) {
	_, docker := newMachine(t)
	helperScript(t, "tok", `
read s
echo '{"Username":"<token>","Secret":"eyJhbGci"}'`)
	writeDockerConfig(t, docker, map[string]any{"credsStore": "tok"})

	got, err := Resolve("registry.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Identity || got.Username != "" || got.Secret != "eyJhbGci" {
		t.Fatalf("%+v", got)
	}
}

// A configuration file names a helper, not a program. john joins the name to
// docker-credential- and resolves it through PATH like every other tool, so
// anything that could be a path is refused before it becomes one.
func TestAConfigurationCannotNameAProgramToRun(t *testing.T) {
	_, docker := newMachine(t)
	for _, name := range []string{"../../bin/sh", "/bin/sh", "a b", "a;b"} {
		writeDockerConfig(t, docker, map[string]any{"credsStore": name})
		_, err := Resolve("ghcr.io")
		if err == nil || !strings.Contains(err.Error(), "credential helper name") {
			t.Errorf("credsStore %q → %v", name, err)
		}
	}
}

// A helper that is named and not installed is the developer's own machine
// being misconfigured, and saying which program is missing is the whole fix.
func TestAMissingHelperNamesTheProgram(t *testing.T) {
	_, docker := newMachine(t)
	writeDockerConfig(t, docker, map[string]any{"credsStore": "notinstalledanywhere"})
	_, err := Resolve("ghcr.io")
	if err == nil || !strings.Contains(err.Error(), "docker-credential-notinstalledanywhere") {
		t.Fatalf("err = %v", err)
	}
}

// A config file that exists and cannot be read is an error rather than a
// shrug: it is the developer's own configuration, they can fix it, and a john
// that ignored it would sign in to nothing and blame the registry.
func TestAnUnreadableDockerConfigurationIsSaidOutLoud(t *testing.T) {
	_, docker := newMachine(t)
	if err := os.WriteFile(filepath.Join(docker, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve("ghcr.io")
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("err = %v", err)
	}
}

// The store is a secret on disk and is written the way the machine token is.
func TestTheStoreIsOwnerOnly(t *testing.T) {
	john, _ := newMachine(t)
	if err := Save("ghcr.io", Credential{Username: "bob", Secret: "p"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(john, storeFile))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if removed, err := Delete("nothing.example.com"); err != nil || removed {
		t.Errorf("deleting what was never there = %v, %v", removed, err)
	}
}

// Listing what is configured must not spend any of it: a helper is a keychain
// prompt, and asking `john registry list` should not cost a password.
func TestListingDoesNotRunTheHelpers(t *testing.T) {
	john, docker := newMachine(t)
	ran := filepath.Join(john, "the-helper-ran")
	helperScript(t, "explodes", "touch "+ran+`
echo "this helper must not be run" >&2
exit 3`)
	writeDockerConfig(t, docker, map[string]any{
		"credsStore":  "explodes",
		"credHelpers": map[string]any{"ghcr.io": "explodes"},
		"auths":       map[string]any{"harbor.example.com": map[string]any{"auth": basicAuth("bob", "p")}},
	})
	if err := Save("private.example.com", Credential{Username: "typed", Secret: "p"}); err != nil {
		t.Fatal(err)
	}

	listed, err := Configured()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"private.example.com":  "john registry login",
		"ghcr.io":              "docker-credential-explodes",
		"harbor.example.com":   "",
		"(any other registry)": "docker-credential-explodes",
	}
	for _, l := range listed {
		from, ok := want[l.Host]
		if !ok {
			t.Errorf("unexpected listing %+v", l)
			continue
		}
		if from != "" && l.From != from {
			t.Errorf("%s came from %q, want %q", l.Host, l.From, from)
		}
		delete(want, l.Host)
	}
	if len(want) != 0 {
		t.Errorf("not listed: %v", want)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("listing ran a credential helper, which on a real machine is a keychain prompt")
	}
}

// A password a person typed goes to the machine's keychain, and the file that
// remains says which registries are signed in to without saying with what.
func TestATypedPasswordGoesToTheKeychain(t *testing.T) {
	johnHome, _ := newMachine(t)
	kc := keychain.NewMemory()
	defer keychain.Use(kc)()

	if err := Save("ghcr.io", Credential{Username: "bob", Secret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	if !kc.Has("registry:ghcr.io") {
		t.Fatal("the password is not in the keychain")
	}
	onDisk, err := os.ReadFile(filepath.Join(johnHome, storeFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "s3cret") {
		t.Fatalf("the password is in the file as well:\n%s", onDisk)
	}
	if !strings.Contains(string(onDisk), "bob") {
		t.Fatalf("the file forgot who is signed in:\n%s", onDisk)
	}

	// And it is found again, which is the only thing the developer notices.
	found, err := lookup(t.Context(), "ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if found.Credential.Secret != "s3cret" || found.Credential.Username != "bob" {
		t.Fatalf("looked up %+v", found.Credential)
	}

	// Saving a second registry must not drag the first one's secret back into
	// the file on the way past.
	if err := Save("registry.internal", Credential{Username: "ann", Secret: "other"}); err != nil {
		t.Fatal(err)
	}
	onDisk, _ = os.ReadFile(filepath.Join(johnHome, storeFile))
	if strings.Contains(string(onDisk), "s3cret") || strings.Contains(string(onDisk), "other") {
		t.Fatalf("a password was written to the file:\n%s", onDisk)
	}

	// Logging out forgets it in both places.
	if gone, err := Delete("ghcr.io"); err != nil || !gone {
		t.Fatalf("delete = %v, %v", gone, err)
	}
	if kc.Has("registry:ghcr.io") {
		t.Fatal("the password outlived the logout in the keychain")
	}
}

// A machine with no keychain keeps working exactly as it did, which is the
// whole of what the fallback has to do.
func TestWithNoKeychainThePasswordStaysInTheFile(t *testing.T) {
	johnHome, _ := newMachine(t)
	defer keychain.Use(noKeychain{})()

	if err := Save("ghcr.io", Credential{Username: "bob", Secret: "s3cret"}); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join(johnHome, storeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "s3cret") {
		t.Fatalf("the password went nowhere:\n%s", onDisk)
	}
	found, err := lookup(t.Context(), "ghcr.io")
	if err != nil || found.Credential.Secret != "s3cret" {
		t.Fatalf("looked up %+v, %v", found.Credential, err)
	}
}

// noKeychain is a machine that has none: a Linux server with no session bus,
// or a platform john has no store for.
type noKeychain struct{}

func (noKeychain) Name() string               { return "" }
func (noKeychain) Available() bool            { return false }
func (noKeychain) Get(string) (string, error) { return "", keychain.ErrNotFound }
func (noKeychain) Set(string, string) error   { return keychain.ErrNotFound }
func (noKeychain) Delete(string) error        { return nil }
