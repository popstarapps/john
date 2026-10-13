package sandbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Pulling the base image, from the host.
//
// The host does this rather than the guest, for two reasons. It is where the
// proxy is, so the build's traffic obeys the same policy.Build setting without
// a second route or a second set of credentials; and it is plain Go over HTTP,
// so it is testable against a registry served in-process rather than only
// inside a booted kernel.
//
// What the guest does is the part that has to be Linux: unpacking layers onto
// a real filesystem with real ownership, and running the build's commands.
//
// This speaks the registry API and nothing more. It is not a container engine
// and holds no local image store: the environment is rebuilt when its
// definition changes and cached on the sandbox's own disk the rest of the
// time, which is the only caching a sandbox needs.

const (
	mediaOCIIndex    = "application/vnd.oci.image.index.v1+json"
	mediaOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerList  = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerMfst  = "application/vnd.docker.distribution.manifest.v2+json"
)

// imageRef is a base image, resolved into the parts a registry request needs.
type imageRef struct {
	Registry string // host[:port]
	Repo     string // library/debian
	Tag      string // stable-slim, when not pinned by digest
	Digest   string // sha256:… when the reference pinned one
}

// parseImageRef reads what a FROM line says. The defaults are Docker Hub's,
// because that is what an unqualified name has always meant and a developer
// writing `FROM debian:stable-slim` means the same thing here as everywhere.
func parseImageRef(s string) (imageRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return imageRef{}, fmt.Errorf("sandbox: empty image reference")
	}
	ref := imageRef{Registry: "registry-1.docker.io", Tag: "latest"}

	name := s
	if at, dig, ok := strings.Cut(s, "@"); ok {
		name, ref.Digest = at, dig
	}
	// A first path segment is a registry when it looks like a host: it has a
	// dot or a colon, or it is localhost. Otherwise it is part of the repo,
	// which is what makes `debian` and `myorg/thing` both Docker Hub.
	if head, rest, ok := strings.Cut(name, "/"); ok &&
		(strings.ContainsAny(head, ".:") || head == "localhost") {
		ref.Registry, name = head, rest
	}
	if colon := strings.LastIndex(name, ":"); colon >= 0 && !strings.Contains(name[colon:], "/") {
		ref.Tag, name = name[colon+1:], name[:colon]
	}
	if ref.Digest != "" {
		ref.Tag = ""
	}
	// Docker Hub's official images live under library/.
	if ref.Registry == "registry-1.docker.io" && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	ref.Repo = name
	if ref.Repo == "" {
		return imageRef{}, fmt.Errorf("sandbox: %q names no image", s)
	}
	return ref, nil
}

// scheme is https everywhere except a registry on this machine. A local
// registry with no certificate is what a developer runs while they are setting
// one up, and it is what a test serves; treating localhost as plain HTTP is
// what every other tool does with it.
func (r imageRef) scheme() string {
	host, _, _ := strings.Cut(r.Registry, ":")
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return "http"
	}
	return "https"
}

// reference is the tag or digest to ask for.
func (r imageRef) reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

func (r imageRef) String() string {
	if r.Repo == "" {
		// A reference with no repository is the registry itself, which is
		// what the sign-in check asks about.
		return r.Registry
	}
	if r.Digest != "" {
		return r.Registry + "/" + r.Repo + "@" + r.Digest
	}
	return r.Registry + "/" + r.Repo + ":" + r.Tag
}

// descriptor is one blob in a manifest.
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

// resolvedImage is a base image reduced to what building needs: the layers to
// unpack, and the environment the image itself declares — which matters
// because PATH usually comes from there and a build's RUN steps need it.
type resolvedImage struct {
	Ref     imageRef
	Layers  []descriptor
	Env     []string
	Workdir string
}

// RegistryCredential is what the host signs in to a registry with when it
// pulls a base image. The sandbox keeps its own shape for it so that this
// package depends on nothing to have one: the client reads the developer's
// credential — from `john registry login`, from ~/.docker/config.json, or
// from a credential helper — and hands it in through VMOptions.
//
// It is spent here, on the host, and goes no further. The machine is handed
// layers; it never sees the password that fetched them.
type RegistryCredential struct {
	Username string
	Secret   string
	// Identity says the secret is a token the registry issued rather than a
	// password, to be exchanged at its token endpoint instead of sent as one.
	Identity bool
}

// Empty means anonymous, which is the ordinary case: most base images are
// public and most organizations never configure anything here.
func (c RegistryCredential) Empty() bool { return c.Username == "" && c.Secret == "" }

// registryClient fetches from a registry, through the proxy or not.
type registryClient struct {
	http  *http.Client
	creds func(host string) (RegistryCredential, error)

	// tokens are bearer tokens, keyed by registry and repository because a
	// token is issued for a scope and one build can touch more than one.
	tokens map[string]string
	// basic are the registries that answered with a Basic challenge, where
	// there is no token to fetch and the credential goes on every request.
	basic map[string]RegistryCredential
	// looked memoises the credential lookup: a helper is a program, and one
	// build asks about the same registry a dozen times.
	looked map[string]RegistryCredential
}

// newRegistryClient builds the client for one environment build. creds may be
// nil, which means anonymous and is what the tests that need no sign-in pass.
//
// proxySocket empty means the build goes straight out, which is what
// policy.BuildDirect asks for and the default: a build is how a project gets
// its compilers, and it is what makes the sandbox useful afterwards under a
// policy that allows no egress at all. When a socket is given, every request
// goes through the john proxy over it and is decided by the network policies,
// which is what policy.BuildProxied asks for.
func newRegistryClient(proxySocket string, caPEM []byte,
	creds func(host string) (RegistryCredential, error)) *registryClient {
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        8,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 30 * time.Second,
	}
	if proxySocket != "" {
		// The proxy listens on a Unix socket, which no proxy URL can name.
		// So the request is addressed to a proxy that does not resolve and
		// the dialer answers with the socket: net/http then speaks CONNECT
		// over it exactly as it would to a proxy on a port.
		tr.Proxy = func(*http.Request) (*url.URL, error) {
			return &url.URL{Scheme: "http", Host: "john-proxy.invalid:8080"}, nil
		}
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", proxySocket)
		}
		if len(caPEM) > 0 {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(caPEM)
			tr.TLSClientConfig = &tls.Config{RootCAs: pool}
		}
	}
	return &registryClient{
		http:   &http.Client{Transport: tr, Timeout: 10 * time.Minute},
		creds:  creds,
		tokens: map[string]string{},
		basic:  map[string]RegistryCredential{},
		looked: map[string]RegistryCredential{},
	}
}

// credential asks the client's owner what this registry is signed in to with,
// once per registry per build.
func (c *registryClient) credential(host string) (RegistryCredential, error) {
	if cred, ok := c.looked[host]; ok {
		return cred, nil
	}
	if c.creds == nil {
		return RegistryCredential{}, nil
	}
	cred, err := c.creds(host)
	if err != nil {
		return RegistryCredential{}, fmt.Errorf("sandbox: the credential for %s: %w", host, err)
	}
	c.looked[host] = cred
	return cred, nil
}

// resolve picks the manifest for one architecture and reads what it needs.
func (c *registryClient) resolve(ctx context.Context, ref imageRef, goarch string) (*resolvedImage, error) {
	body, mediaType, err := c.manifest(ctx, ref, ref.reference())
	if err != nil {
		return nil, err
	}

	// A multi-architecture image is an index of manifests; pick ours. The
	// guest's architecture is the host CPU's, because the machines virtualize
	// rather than emulate — so an image with no build for it cannot be run
	// here at all, and saying which one is missing is the useful answer.
	if mediaType == mediaOCIIndex || mediaType == mediaDockerList {
		var index struct {
			Manifests []descriptor `json:"manifests"`
		}
		if err := json.Unmarshal(body, &index); err != nil {
			return nil, err
		}
		var pick string
		var have []string
		for _, m := range index.Manifests {
			if m.Platform == nil || m.Platform.OS != "linux" {
				continue
			}
			have = append(have, m.Platform.Architecture)
			if m.Platform.Architecture == goarch {
				pick = m.Digest
			}
		}
		if pick == "" {
			return nil, fmt.Errorf("sandbox: %s has no linux/%s build (it has %s), and the "+
				"sandbox runs the host's own processor",
				ref, goarch, strings.Join(have, ", "))
		}
		if body, _, err = c.manifest(ctx, ref, pick); err != nil {
			return nil, err
		}
	}

	var manifest struct {
		Config descriptor   `json:"config"`
		Layers []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, err
	}
	if len(manifest.Layers) == 0 {
		return nil, fmt.Errorf("sandbox: %s has no layers", ref)
	}

	img := &resolvedImage{Ref: ref, Layers: manifest.Layers}

	// The image's own environment. PATH almost always comes from here, and a
	// RUN step that cannot find /usr/bin fails in a way that looks like the
	// command is missing rather than the environment being empty.
	if manifest.Config.Digest != "" {
		blob, err := c.blob(ctx, ref, manifest.Config.Digest)
		if err != nil {
			return nil, err
		}
		var cfg struct {
			Config struct {
				Env        []string `json:"Env"`
				WorkingDir string   `json:"WorkingDir"`
			} `json:"config"`
		}
		err = json.NewDecoder(io.LimitReader(blob, 4<<20)).Decode(&cfg)
		blob.Close()
		if err != nil {
			return nil, fmt.Errorf("sandbox: %s image config: %w", ref, err)
		}
		img.Env, img.Workdir = cfg.Config.Env, cfg.Config.WorkingDir
	}
	return img, nil
}

func (c *registryClient) manifest(ctx context.Context, ref imageRef, reference string) ([]byte, string, error) {
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", ref.scheme(), ref.Registry, ref.Repo, reference)
	resp, err := c.get(ctx, ref, u, strings.Join([]string{
		mediaOCIIndex, mediaDockerList, mediaOCIManifest, mediaDockerMfst,
	}, ", "))
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// blob opens one blob for streaming. The caller closes it.
func (c *registryClient) blob(ctx context.Context, ref imageRef, digest string) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s://%s/v2/%s/blobs/%s", ref.scheme(), ref.Registry, ref.Repo, digest)
	resp, err := c.get(ctx, ref, u, "*/*")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// get performs a registry request, signing in when the registry asks it to.
//
// The sign-in is one retry and no more: the challenge says what the registry
// wants, john either has something to answer it with or does not, and a
// refusal after that is a refusal. What it must never do is loop, because the
// interesting failure — a credential that is real and not allowed to pull this
// repository — answers 401 every time.
func (c *registryClient) get(ctx context.Context, ref imageRef, url, accept string) (*http.Response, error) {
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		if tok := c.tokens[c.tokenKey(ref)]; tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		} else if cred, ok := c.basic[ref.Registry]; ok {
			req.SetBasicAuth(cred.Username, cred.Secret)
		}
		return c.http.Do(req)
	}

	resp, err := do()
	if err != nil {
		return nil, fmt.Errorf("sandbox: reaching %s: %w", ref.Registry, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()
		if err := c.authorize(ctx, ref, challenge); err != nil {
			return nil, err
		}
		if resp, err = do(); err != nil {
			return nil, fmt.Errorf("sandbox: reaching %s: %w", ref.Registry, err)
		}
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			cred, _ := c.credential(ref.Registry)
			return nil, c.refused(ref, cred)
		}
		return nil, fmt.Errorf("sandbox: %s: %s: %s", ref, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// tokenKey scopes a bearer token to what it was issued for.
func (c *registryClient) tokenKey(ref imageRef) string { return ref.Registry + "|" + ref.Repo }

// refusal is a registry saying no to what it was given. It is a type rather
// than a string because the words below are written for a BUILD, which is
// where this usually surfaces — someone checking a credential they have just
// typed is owed a different sentence, and matches on the type to say it.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

// refused is what a developer is told when a registry will not serve them, and
// the two halves of it are different problems. With no credential the answer
// is how to give john one. With a credential the answer is that the one this
// machine holds is not enough, which is a permissions question at the registry
// and not something john can fix by asking again.
func (c *registryClient) refused(ref imageRef, cred RegistryCredential) error {
	if cred.Empty() {
		return &refusal{fmt.Sprintf("sandbox: %s requires a sign-in for %s. Run "+
			"`john registry login %s` — or, if you already pull this image with docker, john "+
			"reads ~/.docker/config.json and its credential helpers and needs nothing",
			ref.Registry, ref, ref.Registry)}
	}
	return &refusal{fmt.Sprintf("sandbox: %s refused the credential john holds for it%s. It is "+
		"signed in, so this is a permission on %s rather than a missing password",
		ref.Registry, asUser(cred), ref)}
}

// asUser names the account without ever naming the secret.
func asUser(cred RegistryCredential) string {
	switch {
	case cred.Identity:
		return " (a token)"
	case cred.Username != "":
		return " (as " + cred.Username + ")"
	}
	return ""
}

// authorize answers a challenge, with the credential this machine holds for
// the registry if it holds one.
//
// Two schemes, because registries are split between them. Docker Hub, GHCR,
// ECR and Artifact Registry answer Bearer: there is a token endpoint, and the
// credential is spent there rather than on the registry. Harbor, Artifactory
// and most on-premises registries answer Basic: there is no endpoint, and the
// credential goes on every request. john does both, because refusing the
// second one meant refusing the registries most organizations actually run.
func (c *registryClient) authorize(ctx context.Context, ref imageRef, challenge string) error {
	scheme, params, _ := strings.Cut(challenge, " ")
	cred, err := c.credential(ref.Registry)
	if err != nil {
		return err
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		return c.bearer(ctx, ref, challengeFields(params), cred)
	case strings.EqualFold(scheme, "Basic"):
		if cred.Empty() {
			return c.refused(ref, cred)
		}
		if cred.Identity {
			return fmt.Errorf("sandbox: %s asks for a password and what john holds for it is a "+
				"token, which only a registry that issued it can read", ref.Registry)
		}
		c.basic[ref.Registry] = cred
		return nil
	default:
		return fmt.Errorf("sandbox: %s asked for %q authentication, which john does not do",
			ref.Registry, scheme)
	}
}

// challengeFields reads the parameters of a Www-Authenticate header.
func challengeFields(params string) map[string]string {
	fields := map[string]string{}
	for _, part := range splitUnquoted(params) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return fields
}

// bearer fetches a token from the realm the challenge names and remembers it.
//
// An identity token is exchanged rather than sent: it is what a registry hands
// back when a person signs in interactively, and the token endpoint takes it
// as an OAuth2 refresh grant. A password goes as HTTP Basic on the token
// request, which is what every registry client does and what makes the same
// credential work here as in docker.
func (c *registryClient) bearer(ctx context.Context, ref imageRef,
	fields map[string]string, cred RegistryCredential) error {
	realm := fields["realm"]
	if realm == "" {
		return fmt.Errorf("sandbox: %s asked for a token but named no realm", ref.Registry)
	}
	service, scope := fields["service"], fields["scope"]
	if scope == "" && ref.Repo != "" {
		scope = "repository:" + ref.Repo + ":pull"
	}

	var req *http.Request
	var err error
	if cred.Identity {
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", cred.Secret)
		form.Set("client_id", "john")
		if service != "" {
			form.Set("service", service)
		}
		if scope != "" {
			form.Set("scope", scope)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, realm,
			strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		q := url.Values{}
		if service != "" {
			q.Set("service", service)
		}
		if scope != "" {
			q.Set("scope", scope)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
		if err == nil && !cred.Empty() {
			req.SetBasicAuth(cred.Username, cred.Secret)
		}
	}
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sandbox: %s token: %w", ref.Registry, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return c.refused(ref, cred)
		}
		return fmt.Errorf("sandbox: %s refused a pull token: %s", ref.Registry, resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return err
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	if tok.Token == "" {
		return fmt.Errorf("sandbox: %s issued an empty token", ref.Registry)
	}
	c.tokens[c.tokenKey(ref)] = tok.Token
	return nil
}

// CheckRegistryCredential signs in to a registry the way a build would, and is
// how `john registry login` refuses a typo at the prompt instead of three days
// later inside somebody's first build. It asks for nothing but the registry's
// own root, so it needs no repository and pulls nothing.
func CheckRegistryCredential(ctx context.Context, host string, cred RegistryCredential) error {
	c := newRegistryClient("", nil, func(string) (RegistryCredential, error) { return cred, nil })
	ref := imageRef{Registry: host}
	resp, err := c.get(ctx, ref, ref.scheme()+"://"+host+"/v2/", "*/*")
	if err != nil {
		// The pull's words are wrong here: nothing is being pulled, and the
		// person reading this typed the credential a second ago.
		var no *refusal
		if errors.As(err, &no) {
			return fmt.Errorf("%s did not accept that sign-in — check the account and the "+
				"password, and that this account may read the registry", host)
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// splitUnquoted splits a challenge's comma-separated parameters, leaving
// commas inside quoted values alone — scopes contain them.
func splitUnquoted(s string) []string {
	var out []string
	var buf strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			buf.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, buf.String())
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}
