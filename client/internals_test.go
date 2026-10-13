package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/backend"
	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/proxy"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/terminalui"
)

// internalsBackend boots a real backend with a configured org (including a
// snapshot-signing key) and returns its URL plus a machine token, for
// exercising the client-side pieces that talk to it.
func internalsBackend(t *testing.T) (backendURL, token string, st *store.Store) {
	t.Helper()
	srv, err := backend.New(backend.Config{
		DBPath:  filepath.Join(t.TempDir(), "john.db"),
		Listen:  "127.0.0.1:0",
		Version: "test",
		DevIdP:  &devidp.Identity{Subject: "dev-1", Email: "dev@example.com", Name: "Dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })

	st = srv.Store()
	_, priv, _ := ed25519.GenerateKey(nil)
	if err := st.SaveOrg(&store.Org{
		Name: "Test", OIDCIssuer: "http://" + srv.Addr() + "/devidp", OIDCClientID: "john",
		AdminEmail: "dev@example.com", SetupComplete: true,
		SigningKey: base64.StdEncoding.EncodeToString(priv),
	}); err != nil {
		t.Fatal(err)
	}
	u, err := st.UpsertSSOUser("dev-1", "dev@example.com", "Dev", "dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	token, err = st.CreateMachineToken(u.ID, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return "http://" + srv.Addr(), token, st
}

// TestPolicyStorePinsAndVerifies covers the client's snapshot supply line: it
// pins the backend's signing key, pulls a signed snapshot, verifies it, and the
// proxy can then evaluate a request against it.
func TestPolicyStorePinsAndVerifies(t *testing.T) {
	backendURL, token, st := internalsBackend(t)

	// A baseline network policy, so it lands in every user's snapshot.
	u, _ := st.UserByEmail("dev@example.com")
	g, _ := st.CreatePolicyGroup("net", "", true, u.ID)
	spec := []byte(`{"kind":"network","methods":["GET"],"scheme":"https","host":"proxy.golang.org","path":"/*","headerMatches":[],"queryMatches":[],"decision":"always-allow","maxBodyBytes":null}`)
	st.CreatePolicy(g.ID, "allow goproxy", "", spec, u.ID)

	ps := newPolicyStore(backendURL, token)
	if ps.Current() != nil {
		t.Fatal("no snapshot should be held before the first refresh")
	}
	if err := ps.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	snap := ps.Current()
	if snap == nil {
		t.Fatal("a verified snapshot should be held after refresh")
	}
	if len(snap.Network) != 1 || snap.Network[0].Spec.Host != "proxy.golang.org" {
		t.Fatalf("snapshot network = %+v", snap.Network)
	}
	// The compiled snapshot evaluates as the proxy would.
	allow, _ := url.Parse("https://proxy.golang.org/x/y/@v/list")
	if snap.Evaluate(policy.Request{Method: "GET", URL: allow, Headers: map[string][]string{}}).Verdict != policy.VerdictAllow {
		t.Fatal("the pulled snapshot should allow the policy's host")
	}
	other, _ := url.Parse("https://evil.example/")
	if snap.Evaluate(policy.Request{Method: "GET", URL: other, Headers: map[string][]string{}}).Verdict != policy.VerdictUnknown {
		t.Fatal("an uncovered host should be unknown")
	}
}

// TestFileAccessRequest covers the unknown-request remedy: the client files a
// network access request from a blocked outcome, and it lands on the backend
// with the host-wide spec, and a second block on the same host joins it.
func TestFileAccessRequest(t *testing.T) {
	backendURL, token, st := internalsBackend(t)

	a := newApp(terminalui.New(terminalui.Options{Mode: terminalui.ModePlain}),
		sandbox.NewFake(t.TempDir()), Options{BackendURL: backendURL, Token: token, Email: "dev@example.com"})

	ref, merged, err := a.fileAccessRequest(proxy.Outcome{
		Method: "GET", URL: "https://registry.npmjs.org/left-pad", Verdict: policy.VerdictUnknown,
	}, false, false)
	if err != nil || ref == "" || merged {
		t.Fatalf("fileAccessRequest = %q merged=%v, %v", ref, merged, err)
	}
	// The next file from the same host joins the same request.
	again, merged, err := a.fileAccessRequest(proxy.Outcome{
		Method: "GET", URL: "https://registry.npmjs.org/is-odd", Verdict: policy.VerdictUnknown,
	}, false, true)
	if err != nil || again != ref || !merged {
		t.Fatalf("second block on the host = %q merged=%v, %v; want added to %s", again, merged, err, ref)
	}
	// A merge-only filing for a host with nothing open files nothing.
	if _, _, err := a.fileAccessRequest(proxy.Outcome{
		Method: "GET", URL: "https://example.com/", Verdict: policy.VerdictUnknown,
	}, false, true); !errors.Is(err, errNothingOpen) {
		t.Fatalf("merge-only for another host = %v, want errNothingOpen", err)
	}
	// Only a path, when that is what the developer chose: two different paths
	// on one host are two requests.
	one, _, err := a.fileAccessRequest(proxy.Outcome{
		Method: "GET", URL: "https://api.example.com/v1/status", Verdict: policy.VerdictUnknown,
	}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	two, merged, err := a.fileAccessRequest(proxy.Outcome{
		Method: "GET", URL: "https://api.example.com/v1/users", Verdict: policy.VerdictUnknown,
	}, true, false)
	if err != nil || two == one || merged {
		t.Fatalf("a second exact path = %q merged=%v (first %q), %v; want a request of its own", two, merged, one, err)
	}
	if r, err := st.AccessRequestByIDOrRef(one); err != nil || !strings.Contains(string(r.Spec), `"path":"/v1/status"`) {
		t.Fatalf("an exact-path request = %s, %v", r.Spec, err)
	}

	reqs, err := st.AccessRequests("", "", "", "registry.npmjs.org")
	if err != nil || len(reqs) != 1 {
		t.Fatalf("access requests = %d, %v", len(reqs), err)
	}
	r := reqs[0]
	if r.Status != "pending" {
		t.Fatalf("status = %s", r.Status)
	}
	var spec struct {
		Kind    string   `json:"kind"`
		Host    string   `json:"host"`
		Path    string   `json:"path"`
		Methods []string `json:"methods"`
	}
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	// The whole host, not the one file asked for first: approving it has to
	// unblock the tool, not just its first request.
	if spec.Kind != "network" || spec.Host != "registry.npmjs.org" || spec.Path != "/*" ||
		len(spec.Methods) != 1 || spec.Methods[0] != "GET" {
		t.Fatalf("proposed spec = %+v", spec)
	}
	for _, u := range []string{"GET https://registry.npmjs.org/left-pad", "GET https://registry.npmjs.org/is-odd"} {
		if !strings.Contains(r.Justification, u) {
			t.Fatalf("the request does not list %q:\n%s", u, r.Justification)
		}
	}
}

// A provider key in the environment is not john's to take: other tools read
// the same variables, and a key copied into john's database outlives the
// variable. Models are added in the console, and nowhere else.
func TestStandaloneIgnoresProviderKeysInTheEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("OPENAI_API_KEY", "sk-test-key")
	t.Setenv("GEMINI_API_KEY", "sk-test-key")

	srv, _, _, err := startStandalone("127.0.0.1:0", t.TempDir())
	if err != nil {
		t.Fatalf("startStandalone: %v", err)
	}
	t.Cleanup(srv.Close)
	if models, _ := srv.Store().Models(); len(models) != 0 {
		t.Fatalf("models appeared from the environment: %d", len(models))
	}
}

// The first run waits for the model the developer is adding in the console,
// and the session starts on it; Ctrl-C ends the wait instead.
func TestWaitForModelReturnsTheModelOnceItIsAdded(t *testing.T) {
	srv, _, _, err := startStandalone("127.0.0.1:0", t.TempDir())
	if err != nil {
		t.Fatalf("startStandalone: %v", err)
	}
	t.Cleanup(srv.Close)
	st := srv.Store()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitForModel(ctx, st, 10*time.Millisecond); err == nil {
		t.Fatal("an interrupted wait reported a model")
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		st.CreateModel(&store.Model{ID: "m_test", Name: "Test Model", Provider: "anthropic",
			Hosting: "external", ModelID: "test", APIKey: "sk-test"})
	}()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := waitForModel(ctx, st, 10*time.Millisecond)
	if err != nil || m == nil || m.Name != "Test Model" {
		t.Fatalf("waitForModel = %+v, %v", m, err)
	}
}

// TestStandaloneBootstrapMintsASigningKey covers the key an org deployment
// gets from `john backend --setup` and standalone has nobody to run. Without
// it the org row keeps the migration's empty default, no snapshot can be
// signed, and the client — which pins the public half either way — holds
// nothing, which the proxy reads as deny-all.
func TestStandaloneBootstrapMintsASigningKey(t *testing.T) {
	dataDir := t.TempDir()
	srv, err := backend.New(backend.Config{
		DBPath:  filepath.Join(dataDir, "john.db"),
		Listen:  "127.0.0.1:0",
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })

	id := devidp.Identity{Subject: "standalone-dev", Email: "dev@localhost", Name: "Dev"}
	if _, _, err := bootstrapStandalone(srv, dataDir, id); err != nil {
		t.Fatalf("bootstrapStandalone: %v", err)
	}

	org, err := srv.Store().Org()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(org.SigningKey)
	if err != nil {
		t.Fatalf("signing key is not base64: %v", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		t.Fatalf("standalone left a %d-byte signing key, want %d", len(raw), ed25519.PrivateKeySize)
	}

	// Restarting must not rotate it: a client that pinned the public half on a
	// previous run would reject every snapshot signed with a new key.
	first := org.SigningKey
	if _, _, err := bootstrapStandalone(srv, dataDir, id); err != nil {
		t.Fatalf("second bootstrapStandalone: %v", err)
	}
	again, err := srv.Store().Org()
	if err != nil {
		t.Fatal(err)
	}
	if again.SigningKey != first {
		t.Fatal("restarting standalone rotated the snapshot signing key")
	}
}

// A solo developer gets workspace protections without asking for them, and
// keeps whatever they decide about them afterwards.
//
// Nothing else in john assumes a policy: a fresh organization protects nothing
// until an administrator says so, which is the right answer when there is an
// administrator. Standalone has none — the developer is the whole
// organization — so "no protections at all" would be a setting nobody chose.
func TestStandaloneStartsWithABaselineAndDoesNotReimposeIt(t *testing.T) {
	dataDir := t.TempDir()
	srv, err := backend.New(backend.Config{
		DBPath:  filepath.Join(dataDir, "john.db"),
		Listen:  "127.0.0.1:0",
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })

	id := devidp.Identity{Subject: "standalone-dev", Email: "dev@localhost", Name: "Dev"}
	if _, _, err := bootstrapStandalone(srv, dataDir, id); err != nil {
		t.Fatalf("bootstrapStandalone: %v", err)
	}

	st := srv.Store()
	policies, err := st.Policies("", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, p := range policies {
		var spec policy.WorkspaceSpec
		if err := json.Unmarshal(p.Spec, &spec); err != nil {
			t.Fatal(err)
		}
		for _, path := range spec.Paths {
			modes[path] = spec.Mode
		}
	}
	if modes[".env"] != policy.ModeExclude {
		t.Fatalf("a dotenv file is %q in a fresh standalone, want %q", modes[".env"], policy.ModeExclude)
	}
	if modes[".git"] != policy.ModeReadOnly {
		t.Fatalf(".git is %q in a fresh standalone, want %q", modes[".git"], policy.ModeReadOnly)
	}

	// It reaches the sandbox as protections rather than sitting in a table:
	// the same resolution a real backend does for a real developer.
	user, err := st.UserByEmail("dev@localhost")
	if err != nil {
		t.Fatal(err)
	}
	groups, err := st.GroupsForUser(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseline := false
	for _, g := range groups {
		if g.Baseline {
			baseline = true
		}
	}
	if !baseline {
		t.Fatal("the baseline group does not apply to the only person on the machine")
	}

	// And a developer who turns one off has turned it off. A bootstrap that
	// wrote the baseline back on every start would be overruling a decision
	// rather than making one nobody had made.
	for _, p := range policies {
		if _, err := st.SetPolicyEnabled(p.ID, false, "dev@localhost"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := bootstrapStandalone(srv, dataDir, id); err != nil {
		t.Fatalf("second bootstrapStandalone: %v", err)
	}
	again, err := st.Policies("", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(policies) {
		t.Fatalf("restarting standalone left %d workspace policies, want the %d it started with",
			len(again), len(policies))
	}
	for _, p := range again {
		if p.Enabled {
			t.Fatalf("restarting standalone turned %q back on", p.Name)
		}
	}
}

// TestPromptBeforeTheSessionOpensIsRefused pins the answer to a prompt typed
// while john is still starting: the wire has no Conn yet, so the message
// cannot go anywhere. It must be refused out loud rather than dropped — a
// dropped prompt leaves the UI on "thinking" with nothing ever coming back,
// which is what it did before.
func TestPromptBeforeTheSessionOpensIsRefused(t *testing.T) {
	var out bytes.Buffer
	ui := terminalui.New(terminalui.Options{
		Mode: terminalui.ModePlain, Output: &out, Input: strings.NewReader(""),
	})
	a := newApp(ui, sandbox.NewFake(t.TempDir()),
		Options{BackendURL: "http://127.0.0.1:1", Token: "t", Email: "dev@example.com"})

	done := make(chan struct{})
	go func() { ui.Run(context.Background()); close(done) }()

	if a.wire.connected() {
		t.Fatal("a wire that has never connected reports a Conn")
	}
	a.prompt("say hello")

	ui.Quit()
	<-done

	if a.turn != "" || a.turnSeq != 0 {
		t.Fatalf("a refused prompt opened turn %q (seq %d)", a.turn, a.turnSeq)
	}
	if !strings.Contains(out.String(), "not connected yet") {
		t.Fatalf("the developer was not told why nothing happened:\n%s", out.String())
	}
}

// `john console` on a standalone machine finds a running standalone backend
// by its identity provider, and a second backend on the same port says what
// is in the way rather than only that the address is taken.
func TestASecondStandaloneSaysTheFirstIsInTheWay(t *testing.T) {
	srv, token, _, err := startStandalone("127.0.0.1:0", t.TempDir())
	if err != nil {
		t.Fatalf("startStandalone: %v", err)
	}
	t.Cleanup(srv.Close)
	if token == "" {
		t.Fatal("no token for the local user")
	}
	if !standaloneRunning(srv.Addr()) {
		t.Fatal("a running standalone backend was not recognised")
	}

	_, _, _, err = startStandalone(srv.Addr(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second standalone on %s: %v", srv.Addr(), err)
	}

	// Something else on a port is not a standalone john.
	other := httptest.NewServer(http.NotFoundHandler())
	defer other.Close()
	if standaloneRunning(strings.TrimPrefix(other.URL, "http://")) {
		t.Fatal("a server without the identity provider was taken for standalone john")
	}
}

// The agent reads absolute paths off the sandbox — `pwd`, compiler output —
// and they name the workspace by the host's own path, mirrored. Those come
// back as the relative path the sandbox takes; anything outside the workspace
// is left for the sandbox to refuse.
func TestAnAbsolutePathInTheWorkspaceIsTakenAsRelative(t *testing.T) {
	dir := t.TempDir()
	a := &app{workdir: dir}
	mount := sandbox.GuestMount(dir)
	cases := map[string]string{
		mount + "/src/App.js": "src/App.js",
		mount + "/a.txt":      "a.txt",
		"src/App.js":          "src/App.js",
		"/tmp/x.html":         "/tmp/x.html",
		mount + "-other/a":    mount + "-other/a",
		mount:                 mount,
	}
	for in, want := range cases {
		if got := a.inWorkspace(in); got != want {
			t.Errorf("inWorkspace(%q) = %q, want %q", in, got, want)
		}
	}
}

// What goes under a failed command is the reason, wherever the tool put it.
// These are what npm, apt and Python actually printed.
func TestFailureLinesAreTheReason(t *testing.T) {
	for name, tc := range map[string]struct {
		stderr string
		want   []string
	}{
		"npm, whose reason is under its error code and above its advice and log": {
			"npm ERR! code E403\nnpm ERR! 403 403 Forbidden - GET https://registry.npmjs.org/@vitejs%2fplugin-react\n" +
				"npm ERR! 403 In most cases, you or one of your dependencies are requesting\n" +
				"npm ERR! 403 a package version that is forbidden by your security policy, or\n" +
				"npm ERR! 403 on a server you do not have access to.\n\n" +
				"npm ERR! A complete log of this run can be found in:\n" +
				"npm ERR!     /root/.npm/_logs/2026-10-08T10_15_33_067Z-debug-0.log\n",
			[]string{"npm ERR! code E403",
				"npm ERR! 403 403 Forbidden - GET https://registry.npmjs.org/@vitejs%2fplugin-react",
				"npm ERR! 403 In most cases, you or one of your dependencies are requesting"},
		},
		"apt, which leads with what failed": {
			"E: The repository 'http://deb.debian.org/debian stable InRelease' is not signed.\n" +
				"E: Failed to fetch http://deb.debian.org/debian/dists/stable/InRelease  403  Forbidden\n" +
				"E: Failed to fetch http://deb.debian.org/debian/dists/stable-updates/InRelease  403  Forbidden\n" +
				"E: The repository 'http://deb.debian.org/debian stable-updates InRelease' is not signed.\n",
			[]string{"E: The repository 'http://deb.debian.org/debian stable InRelease' is not signed.",
				"E: Failed to fetch http://deb.debian.org/debian/dists/stable/InRelease  403  Forbidden",
				"E: Failed to fetch http://deb.debian.org/debian/dists/stable-updates/InRelease  403  Forbidden"},
		},
		"Python, whose exception comes last": {
			"Traceback (most recent call last):\n  File \"/w/build.py\", line 3, in <module>\n    main()\n" +
				"  File \"/w/build.py\", line 2, in main\n    open('missing.json')\n" +
				"FileNotFoundError: [Errno 2] No such file or directory: 'missing.json'\n",
			[]string{"main()", "open('missing.json')",
				"FileNotFoundError: [Errno 2] No such file or directory: 'missing.json'"},
		},
	} {
		got := failureLines([]byte(tc.stderr), nil, 3)
		if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s:\ngot  %q\nwant %q", name, got, tc.want)
		}
	}
}

// A policy refresh is announced as what changed. Every refresh used to say
// "policies updated", including the one for a model an administrator added.
func TestAPolicyChangeIsCalledWhatItIs(t *testing.T) {
	for _, c := range []struct {
		what []string
		want string
	}{
		{nil, "policies updated"},
		{[]string{"models"}, "models updated"},
		{[]string{"routing"}, "model routing updated"},
		{[]string{"builds", "prompts"}, "build settings and prompt settings updated"},
		{[]string{"network", "workspace", "models"}, "network policies, workspace protections and models updated"},
		{[]string{"groups"}, "policies updated"},
		{[]string{"something-newer", "groups"}, "policies updated"},
	} {
		if got := policyChangeNotice(c.what); got != c.want {
			t.Errorf("policyChangeNotice(%q) = %q, want %q", c.what, got, c.want)
		}
	}
}

// TestASnapshotThatExpiredInSleepIsRenewedWhenTheProxyAsks is a laptop waking
// from an hour asleep: the snapshot it holds expired by the wall clock while
// the timer that would have renewed it stood still. The proxy's next decision
// fetches a fresh one rather than refusing everything as "no policy".
func TestASnapshotThatExpiredInSleepIsRenewedWhenTheProxyAsks(t *testing.T) {
	backendURL, token, _ := internalsBackend(t)
	ps := newPolicyStore(backendURL, token)
	if err := ps.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	// An hour of sleep, as the wall clock sees it.
	ps.mu.Lock()
	stale := *ps.snap
	stale.IssuedAt = time.Now().Add(-65 * time.Minute)
	stale.ExpiresAt = time.Now().Add(-60 * time.Minute)
	ps.snap = &stale
	ps.mu.Unlock()
	if ps.Current() != nil {
		t.Fatal("an expired snapshot is still being handed out")
	}
	if !ps.due(time.Now()) {
		t.Fatal("an expired snapshot is not due for renewal")
	}

	if snap := ps.Usable(); snap == nil || snap.Expired(time.Now()) {
		t.Fatalf("the proxy got %+v, want a fresh snapshot", snap)
	}
	if ps.due(time.Now()) {
		t.Fatal("a snapshot just fetched is already due again")
	}
}

// With the backend gone, Usable still fails closed, and says so promptly.
func TestAnExpiredSnapshotWithNoBackendStillFailsClosed(t *testing.T) {
	ps := newPolicyStore("http://127.0.0.1:1", "t")
	start := time.Now()
	if snap := ps.Usable(); snap != nil {
		t.Fatalf("no backend, and a snapshot came back: %+v", snap)
	}
	if waited := time.Since(start); waited > 6*time.Second {
		t.Fatalf("the proxy was held %s before failing closed", waited)
	}
}
