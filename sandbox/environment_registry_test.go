package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnImageReferenceMeansWhatItMeansEverywhereElse(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"debian", "registry-1.docker.io/library/debian:latest"},
		{"debian:stable-slim", "registry-1.docker.io/library/debian:stable-slim"},
		{"myorg/tools:v2", "registry-1.docker.io/myorg/tools:v2"},
		{"ghcr.io/acme/env:1.0", "ghcr.io/acme/env:1.0"},
		{"localhost:5000/env:dev", "localhost:5000/env:dev"},
		{"registry.internal:8443/team/img", "registry.internal:8443/team/img:latest"},
		{"debian@sha256:abc", "registry-1.docker.io/library/debian@sha256:abc"},
	} {
		ref, err := parseImageRef(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got := ref.String(); got != c.want {
			t.Errorf("%s → %s, want %s", c.in, got, c.want)
		}
	}
}

// A registry served in-process, so the pull is tested without a network and
// without a virtual machine: token challenge, multi-architecture index, the
// manifest, and the image's own environment.
func fakeRegistry(t *testing.T, arches []string) (string, map[string]string) {
	host, blobs, _ := fakeRegistryWithAuth(t, arches, &registryAuth{})
	return host, blobs
}

// registryAuth is how one fake registry asks to be signed in to, and what it
// saw. Registries are split between the two schemes — Docker Hub, GHCR and the
// cloud registries answer Bearer, while Harbor and Artifactory answer Basic —
// so both are served here rather than assumed.
type registryAuth struct {
	// mode is "" for a registry anyone can read, "password" for a token
	// endpoint that wants the credential as Basic, "token" for one that wants
	// an identity token exchanged, and "basic" for a registry with no token
	// endpoint at all.
	mode     string
	user     string
	password string
	token    string

	tokenRequests int    // how many tokens were issued
	sawBasic      string // the username the token endpoint was given
	sawGrant      string // the grant type it was asked for
}

func fakeRegistryWithAuth(t *testing.T, arches []string, auth *registryAuth) (string, map[string]string, *registryAuth) {
	t.Helper()
	blobs := map[string]string{}
	put := func(digest, body string) { blobs[digest] = body }

	config := `{"config":{"Env":["PATH=/usr/local/bin:/usr/bin:/bin","LANG=C"],"WorkingDir":"/root"}}`
	put("sha256:config", config)
	put("sha256:layer1", "LAYER-ONE")

	manifest := `{"config":{"mediaType":"` + mediaOCIManifest + `","digest":"sha256:config","size":` +
		fmt.Sprint(len(config)) + `},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip",` +
		`"digest":"sha256:layer1","size":9}]}`
	put("sha256:manifest", manifest)

	var entries []string
	for _, a := range arches {
		entries = append(entries, `{"mediaType":"`+mediaOCIManifest+`","digest":"sha256:manifest",`+
			`"size":`+fmt.Sprint(len(manifest))+`,"platform":{"os":"linux","architecture":"`+a+`"}}`)
	}
	index := `{"manifests":[` + strings.Join(entries, ",") + `]}`

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		switch auth.mode {
		case "password":
			user, pass, ok := r.BasicAuth()
			auth.sawBasic = user
			if !ok || user != auth.user || pass != auth.password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case "token":
			r.ParseForm()
			auth.sawGrant = r.Form.Get("grant_type")
			if r.Method != http.MethodPost || r.Form.Get("refresh_token") != auth.token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		auth.tokenRequests++
		json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if auth.mode == "basic" {
			user, pass, ok := r.BasicAuth()
			if !ok || user != auth.user || pass != auth.password {
				w.Header().Set("Www-Authenticate", `Basic realm="fake"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		} else if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("Www-Authenticate",
				`Bearer realm="`+registryBase+`/token",service="fake",scope="repository:library/base:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v2/":
			// What a registry answers at its root: the thing a sign-in check
			// asks for, since it proves the credential and pulls nothing.
			w.Write([]byte("{}"))
		case strings.Contains(r.URL.Path, "/manifests/"):
			ref := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if ref == "sha256:manifest" {
				w.Header().Set("Content-Type", mediaOCIManifest)
				w.Write([]byte(manifest))
				return
			}
			w.Header().Set("Content-Type", mediaOCIIndex)
			w.Write([]byte(index))
		case strings.Contains(r.URL.Path, "/blobs/"):
			d := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			body, ok := blobs[d]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	registryBase = srv.URL
	return strings.TrimPrefix(srv.URL, "http://"), blobs, auth
}

var registryBase string

func TestPullingABaseImageTakesTheTokenTheArchAndTheImagesOwnEnvironment(t *testing.T) {
	host, _ := fakeRegistry(t, []string{"amd64", "arm64"})

	c := newRegistryClient("", nil, nil)
	// The fake registry is plain HTTP; point the client's scheme at it by
	// serving over the test server's own transport.
	c.http = &http.Client{Transport: rewriteToHTTP{}}

	ref, err := parseImageRef(host + "/library/base:v1")
	if err != nil {
		t.Fatal(err)
	}
	img, err := c.resolve(context.Background(), ref, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Layers) != 1 || img.Layers[0].Digest != "sha256:layer1" {
		t.Fatalf("layers = %+v", img.Layers)
	}
	if len(img.Env) != 2 || !strings.HasPrefix(img.Env[0], "PATH=") {
		t.Fatalf("the image's own environment did not come through: %q", img.Env)
	}
	if img.Workdir != "/root" {
		t.Errorf("workdir = %q", img.Workdir)
	}

	// An image with no build for this processor says which ones it has,
	// because the sandbox runs the host's own and cannot emulate another.
	if _, err := c.resolve(context.Background(), ref, "riscv64"); err == nil ||
		!strings.Contains(err.Error(), "no linux/riscv64") {
		t.Fatalf("wrong architecture error = %v", err)
	}
}

// rewriteToHTTP sends the client's https:// registry requests to the plain
// HTTP test server, which is the one thing a local registry cannot be.
type rewriteToHTTP struct{}

func (rewriteToHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme = "http"
	return http.DefaultTransport.RoundTrip(r)
}

// creds is a client that holds one credential, the way a developer's machine
// does once they have signed in.
func creds(c RegistryCredential) func(string) (RegistryCredential, error) {
	return func(string) (RegistryCredential, error) { return c, nil }
}

// The case any organization with an internal registry hits on their first
// .john/Dockerfile: the base image is private, and the credential the machine
// already holds is what fetches it.
func TestAPrivateBaseImageIsPulledWithTheCredentialTheMachineHolds(t *testing.T) {
	host, _, auth := fakeRegistryWithAuth(t, []string{"amd64", "arm64"}, &registryAuth{
		mode: "password", user: "bob", password: "s3cret",
	})

	c := newRegistryClient("", nil, creds(RegistryCredential{Username: "bob", Secret: "s3cret"}))
	ref, err := parseImageRef(host + "/acme/base:v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.resolve(context.Background(), ref, "arm64"); err != nil {
		t.Fatalf("a private pull with the right credential failed: %v", err)
	}
	if auth.sawBasic != "bob" {
		t.Errorf("the token endpoint was given %q", auth.sawBasic)
	}
	if auth.tokenRequests != 1 {
		t.Errorf("token requests = %d, want one for the repository", auth.tokenRequests)
	}

	// A token is issued for a scope, so a second repository is a second
	// token. Keying them by repository alone would send one registry's token
	// to another, which fails in a way that reads as a broken credential.
	other, err := parseImageRef(host + "/acme/other:v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.resolve(context.Background(), other, "arm64"); err != nil {
		t.Fatal(err)
	}
	if auth.tokenRequests != 2 {
		t.Errorf("token requests = %d, want one per repository", auth.tokenRequests)
	}
}

// With nothing to sign in with, the answer is how to give john something.
func TestAPrivateRegistryWithNoCredentialSaysHowToSignIn(t *testing.T) {
	host, _, _ := fakeRegistryWithAuth(t, []string{"arm64"}, &registryAuth{
		mode: "password", user: "bob", password: "s3cret",
	})
	c := newRegistryClient("", nil, nil)
	ref, err := parseImageRef(host + "/acme/base:v1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.resolve(context.Background(), ref, "arm64")
	if err == nil || !strings.Contains(err.Error(), "john registry login") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "docker") {
		t.Errorf("the message does not mention the credentials john can already read: %v", err)
	}
}

// A credential that is real and not enough is a different problem, and telling
// someone to sign in when they are signed in sends them to fix the wrong
// thing.
func TestACredentialThatIsRefusedIsNotAMissingOne(t *testing.T) {
	host, _, _ := fakeRegistryWithAuth(t, []string{"arm64"}, &registryAuth{
		mode: "password", user: "bob", password: "s3cret",
	})
	c := newRegistryClient("", nil, creds(RegistryCredential{Username: "bob", Secret: "wrong"}))
	ref, err := parseImageRef(host + "/acme/base:v1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.resolve(context.Background(), ref, "arm64")
	if err == nil || !strings.Contains(err.Error(), "refused the credential john holds") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "as bob") {
		t.Errorf("the message does not name the account: %v", err)
	}
	if strings.Contains(err.Error(), "registry login") {
		t.Errorf("a signed-in machine was told to sign in: %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("the secret is in the error message: %v", err)
	}
}

// The cloud registries hand out identity tokens rather than passwords, and a
// token is exchanged at the endpoint rather than sent as a password. Sending
// it as one fails against every registry that issues them.
func TestAnIdentityTokenIsExchangedRatherThanSent(t *testing.T) {
	host, _, auth := fakeRegistryWithAuth(t, []string{"arm64"}, &registryAuth{
		mode: "token", token: "eyJhbGci",
	})
	c := newRegistryClient("", nil, creds(RegistryCredential{Secret: "eyJhbGci", Identity: true}))
	ref, err := parseImageRef(host + "/acme/base:v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.resolve(context.Background(), ref, "arm64"); err != nil {
		t.Fatalf("an identity token did not pull: %v", err)
	}
	if auth.sawGrant != "refresh_token" {
		t.Errorf("grant type = %q", auth.sawGrant)
	}
}

// Harbor, Artifactory and most on-premises registries answer Basic, which john
// used to refuse by name. There is no token endpoint: the credential goes on
// the request itself.
func TestARegistryThatAsksForBasicIsSignedInTo(t *testing.T) {
	host, _, auth := fakeRegistryWithAuth(t, []string{"arm64"}, &registryAuth{
		mode: "basic", user: "alice", password: "hunter2",
	})
	c := newRegistryClient("", nil, creds(RegistryCredential{Username: "alice", Secret: "hunter2"}))
	ref, err := parseImageRef(host + "/acme/base:v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.resolve(context.Background(), ref, "arm64"); err != nil {
		t.Fatalf("a Basic registry did not pull: %v", err)
	}
	if auth.tokenRequests != 0 {
		t.Errorf("a Basic registry has no token endpoint, and %d tokens were fetched",
			auth.tokenRequests)
	}

	// And an identity token is not a password: a registry asking for one
	// cannot read the other, and saying so beats a 401 nobody can explain.
	c = newRegistryClient("", nil, creds(RegistryCredential{Secret: "eyJ", Identity: true}))
	_, err = c.resolve(context.Background(), ref, "arm64")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v", err)
	}
}

// `john registry login` proves the credential before it keeps it, against the
// registry's own root and pulling nothing. A password wrong by one character
// is otherwise found by a build, days later, by somebody else.
func TestALoginIsCheckedBeforeItIsKept(t *testing.T) {
	host, _, _ := fakeRegistryWithAuth(t, []string{"arm64"}, &registryAuth{
		mode: "password", user: "bob", password: "s3cret",
	})
	ctx := context.Background()
	if err := CheckRegistryCredential(ctx, host,
		RegistryCredential{Username: "bob", Secret: "s3cret"}); err != nil {
		t.Fatalf("the right credential was refused: %v", err)
	}
	err := CheckRegistryCredential(ctx, host, RegistryCredential{Username: "bob", Secret: "wrong"})
	if err == nil {
		t.Fatal("a wrong password was accepted at the prompt")
	}
	// And it is told in the words of the thing being done. The build's
	// message — "the credential john holds", "run john registry login" — is
	// nonsense to somebody who typed a password one second ago.
	if strings.Contains(err.Error(), "registry login") ||
		strings.Contains(err.Error(), "credential john holds") {
		t.Errorf("a login was answered in a build's words: %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatalf("the secret is in the error message: %v", err)
	}
}

// The credential is the host's and stays there.
//
// The base image is pulled on the host because that is where the proxy is, so
// the password is read, spent on an Authorization header, and never handed to
// the machine — what crosses is layers. This asserts it at the seam: the
// environ call is everything the machine is told, and it is these three things
// and no others. A fourth would be a credential leaving the developer's side
// of the boundary.
func TestTheMachineIsToldTheRouteOutAndNothingAboutTheCredential(t *testing.T) {
	v := &VM{opts: VMOptions{
		Env:        []string{"TERM=dumb"},
		ProxyCAPEM: []byte("-----BEGIN CERTIFICATE-----"),
		RegistryCredentials: creds(RegistryCredential{
			Username: "bob", Secret: "SECRET-THAT-MUST-NOT-CROSS",
		}),
	}}

	payload := v.environPayload("vsock:1024")
	want := map[string]bool{"proxy": true, "env": true, "ca_pem": true}
	for k := range payload {
		if !want[k] {
			t.Errorf("the machine is told %q, which nothing has decided it should be", k)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Errorf("no longer told: %v", want)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SECRET-THAT-MUST-NOT-CROSS") ||
		strings.Contains(string(encoded), "bob") {
		t.Fatalf("the registry credential crossed into the machine: %s", encoded)
	}
}
