package backend

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/store"
)

// testServerWithIdP boots a backend with the embedded identity provider and a
// configured organization whose issuer points at it — the standalone shape.
func testServerWithIdP(t *testing.T) *Server {
	t.Helper()
	srv, err := New(Config{
		DBPath:  filepath.Join(t.TempDir(), "john.db"),
		Listen:  "127.0.0.1:0",
		Version: "test",
		DevIdP:  &devidp.Identity{Subject: "dev-1", Email: "oliver@example.com", Name: "Oliver K"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		srv.Close()
	})

	// With a real signing key, as every organization has: setup mints one and
	// so does standalone, and a test org without one exercises the deny-all
	// fallback instead of the policy path it looks like it is testing.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store().SaveOrg(&store.Org{
		Name:             "Test Org",
		OIDCIssuer:       "http://" + srv.Addr() + "/devidp",
		OIDCClientID:     "john",
		OIDCClientSecret: "dev-secret",
		AdminEmail:       "oliver@example.com",
		SigningKey:       base64.StdEncoding.EncodeToString(priv),
		SetupComplete:    true,
	}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	return srv
}

func browserClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func TestBrowserSignInThroughOIDC(t *testing.T) {
	requireConsole(t) // the dance ends on a console route, which must render
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()
	browser := browserClient(t)

	// The full dance: /auth/login → IdP authorize → /auth/callback → cookie →
	// console. The default client follows every redirect.
	resp, err := browser.Get(base + "/auth/login?next=/policies")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status %d", resp.StatusCode)
	}
	if got := resp.Request.URL.Path; got != "/policies" {
		t.Fatalf("landed on %s, want /policies", got)
	}

	var viewer struct {
		User    struct{ Email, Role string }
		IsAdmin bool
	}
	getJSON(t, browser, base+"/api/v1/viewer", &viewer)
	if viewer.User.Email != "oliver@example.com" {
		t.Fatalf("viewer = %+v", viewer)
	}
	if !viewer.IsAdmin {
		t.Fatal("the configured admin email must come out an administrator")
	}
}

func TestSignOutClearsSession(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()
	browser := browserClient(t)

	resp, _ := browser.Get(base + "/auth/login")
	resp.Body.Close()
	if code := status(t, browser, base+"/api/v1/viewer"); code != http.StatusOK {
		t.Fatalf("signed-in viewer = %d", code)
	}
	resp, _ = browser.Get(base + "/auth/logout")
	resp.Body.Close()
	if code := status(t, browser, base+"/api/v1/viewer"); code != http.StatusUnauthorized {
		t.Fatalf("signed-out viewer = %d, want 401", code)
	}
}

func TestCLILoginFlow(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	// The CLI starts a flow…
	var start struct{ Code, URL string }
	postJSON(t, http.DefaultClient, base+"/api/v1/auth/cli/start", map[string]string{}, &start)
	if start.Code == "" || !strings.Contains(start.URL, "cli="+start.Code) {
		t.Fatalf("start = %+v", start)
	}

	// …polling answers pending until the browser half happens…
	var pending struct{ Pending bool }
	code := postJSONStatus(t, base+"/api/v1/auth/cli/poll", map[string]string{"code": start.Code}, &pending)
	if code != http.StatusAccepted || !pending.Pending {
		t.Fatalf("early poll = %d %+v", code, pending)
	}

	// …the developer signs in in a browser…
	browser := browserClient(t)
	resp, err := browser.Get(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// …and the poll now yields a machine token.
	var done struct {
		Token string
		User  struct{ Email string }
	}
	code = postJSONStatus(t, base+"/api/v1/auth/cli/poll", map[string]string{"code": start.Code}, &done)
	if code != http.StatusOK || done.Token == "" || done.User.Email != "oliver@example.com" {
		t.Fatalf("poll = %d %+v", code, done)
	}

	// The token authenticates API calls as a Bearer…
	req, _ := http.NewRequest("GET", base+"/api/v1/viewer", nil)
	req.Header.Set("Authorization", "Bearer "+done.Token)
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("bearer viewer = %d", r2.StatusCode)
	}

	// …and the login code was single-use.
	code = postJSONStatus(t, base+"/api/v1/auth/cli/poll", map[string]string{"code": start.Code}, &struct{}{})
	if code != http.StatusBadRequest {
		t.Fatalf("re-poll = %d, want 400", code)
	}
}

func TestSetupFlow(t *testing.T) {
	srv, err := New(Config{
		DBPath:  filepath.Join(t.TempDir(), "john.db"),
		Listen:  "127.0.0.1:0",
		Version: "test",
		Setup:   true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	defer func() { cancel(); <-done; srv.Close() }()

	if !strings.Contains(srv.SetupURL, "/setup?token=") {
		t.Fatalf("SetupURL = %q", srv.SetupURL)
	}
	token := srv.SetupURL[strings.Index(srv.SetupURL, "token=")+len("token="):]
	base := "http://" + srv.Addr()

	payload := map[string]any{
		"orgName":    "Acme",
		"issuer":     "https://idp.acme.example",
		"clientId":   "john",
		"adminEmail": "admin@acme.example",
		"traceKey":   testRecipient,
		"tracePath":  filepath.Join(t.TempDir(), "traces"),
		"models": []map[string]any{{
			"name": "Claude Opus", "provider": "anthropic", "hosting": "external",
			"modelId": "claude-opus-5", "contextWindow": 200000,
			"costPerMTokIn": 5.0, "costPerMTokOut": 25.0, "apiKey": "sk-test",
		}},
	}
	body, _ := json.Marshal(payload)

	// Without the token: refused, nothing written.
	resp, err := http.Post(base+"/api/v1/setup", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tokenless setup = %d", resp.StatusCode)
	}

	// With it but a form that is wrong: refused, and the token is not spent,
	// so the operator corrects the form instead of restarting setup.
	bad, _ := json.Marshal(map[string]any{"orgName": "Acme", "traceKey": "AGE-SECRET-KEY-1XYZ"})
	req, _ := http.NewRequest("POST", base+"/api/v1/setup", bytes.NewReader(bad))
	req.Header.Set("X-Setup-Token", token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(msg), "missing OIDC issuer") {
		t.Fatalf("setup with a bad form = %d %s, want 400 saying what is missing", resp.StatusCode, msg)
	}

	// With it: the organization is written, key material generated.
	req, _ = http.NewRequest("POST", base+"/api/v1/setup", bytes.NewReader(body))
	req.Header.Set("X-Setup-Token", token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup = %d", resp.StatusCode)
	}

	org, err := srv.Store().Org()
	if err != nil {
		t.Fatal(err)
	}
	if org.Name != "Acme" || !org.SetupComplete || org.SigningKey == "" || org.AdminEmail != "admin@acme.example" {
		t.Fatalf("org = %+v", org)
	}
	// Nothing was said about domains, so sign-in admits the administrator's
	// and no other, rather than whoever the provider vouches for.
	if len(org.AllowedDomains) != 1 || org.AllowedDomains[0] != "acme.example" {
		t.Fatalf("allowed domains = %q, want the administrator's", org.AllowedDomains)
	}
	models, err := srv.Store().Models()
	if err != nil || len(models) != 1 {
		t.Fatalf("models = %v, %v", models, err)
	}
	if lm, err := models[0].LLM(); err != nil || lm.ID != "claude-opus-5" || lm.API != "anthropic-messages" {
		t.Fatalf("llm model = %+v, %v", lm, err)
	}

	// The token was spent: a replay is refused.
	req, _ = http.NewRequest("POST", base+"/api/v1/setup", bytes.NewReader(body))
	req.Header.Set("X-Setup-Token", token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("replayed setup = %d, want 403", resp.StatusCode)
	}
}

func TestSetupRefusedOnConfiguredBackend(t *testing.T) {
	db := filepath.Join(t.TempDir(), "john.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	st.SaveOrg(&store.Org{Name: "Done", SetupComplete: true})
	st.Close()

	_, err = New(Config{DBPath: db, Listen: "127.0.0.1:0", Setup: true})
	if err == nil || !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("err = %v, want refusal", err)
	}
}

// --- helpers ---

func getJSON(t *testing.T, c *http.Client, url string, out any) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func status(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func postJSON(t *testing.T, c *http.Client, url string, in, out any) {
	t.Helper()
	if code := postJSONStatus(t, url, in, out); code != http.StatusOK {
		t.Fatalf("POST %s = %d", url, code)
	}
}

func postJSONStatus(t *testing.T, url string, in, out any) int {
	t.Helper()
	body, _ := json.Marshal(in)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode
}
