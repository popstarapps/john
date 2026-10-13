package backend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oliverkofoed/gokit/agentkit/llm"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"
)

// The console tries a model before saving it: one short prompt, through the
// same client a session uses, with the key from the dialog — or, on an edit
// that leaves the key blank, the stored one.
func TestTestingAModelSendsOnePromptWithTheRightKey(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()
	browser := browserClient(t)
	resp, err := browser.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin := &apiClient{t: t, base: base, http: browser}

	fake := llmtest.New(llmtest.Text("OK"), llmtest.Error(errors.New("http 404: model not found")))
	srv.cfg.Streamer = fake

	type result struct {
		OK           bool
		Reply, Error string
	}
	draft := map[string]any{
		"name": "Claude", "provider": "anthropic", "hosting": "external",
		"endpoint": "https://api.anthropic.com/v1/messages", "modelId": "claude-test",
		"apiKey": "sk-new",
	}
	var got result
	admin.post("POST", "/api/v1/models/test", draft, &got)
	if !got.OK || got.Reply != "OK" {
		t.Fatalf("test of a working model = %+v", got)
	}
	req := fake.Requests()[0]
	if req.APIKey != "sk-new" || req.Model.BaseURL != "https://api.anthropic.com" || req.Model.ID != "claude-test" {
		t.Fatalf("request: key %q, base %q, model %q", req.APIKey, req.Model.BaseURL, req.Model.ID)
	}
	// What a session sends: the model's own output limit, not one of the test's.
	if req.MaxTokens != 0 || req.Model.MaxOutput == 0 {
		t.Fatalf("request: max tokens %d, model limit %d", req.MaxTokens, req.Model.MaxOutput)
	}

	// An edit with the key left blank is tried with the stored key, and a
	// provider's refusal comes back as the answer rather than as a failure.
	var saved struct{ ID string }
	draft["apiKey"] = "sk-stored"
	admin.post("POST", "/api/v1/models", draft, &saved)
	draft["apiKey"], draft["id"] = "", saved.ID
	admin.post("POST", "/api/v1/models/test", draft, &got)
	if got.OK || got.Error == "" {
		t.Fatalf("test of a failing model = %+v", got)
	}
	if key := fake.Requests()[1].APIKey; key != "sk-stored" {
		t.Fatalf("edit was tried with key %q, want the stored one", key)
	}
}

// A model with no stored key goes out with no key. The client library would
// otherwise take ANTHROPIC_API_KEY from the backend's own environment.
func TestTheBackendNeverSpendsAKeyFromItsEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-the-environment")
	sent := make(chan string, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent <- r.Header.Get("x-api-key")
		http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"no key"}}`, http.StatusUnauthorized)
	}))
	defer provider.Close()

	srv := &Server{}
	_, err := srv.streamer().Stream(t.Context(), llm.Request{
		Model:    llm.Model{ID: "claude-test", API: llm.AnthropicMessages, Provider: "anthropic", BaseURL: provider.URL},
		Messages: []llm.Message{llm.UserText("hi")},
	}).Message()
	if err == nil {
		t.Fatal("a request without a key succeeded")
	}
	if key := <-sent; key != "" {
		t.Fatalf("sent key %q", key)
	}
}

// Standalone signs in whoever asks, so it answers only this machine. A page
// that points its own domain at 127.0.0.1 arrives with that domain as the
// Host, and is refused; so is anything that is not from loopback at all.
func TestALocalOnlyBackendAnswersOnlyThisMachine(t *testing.T) {
	h := localOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, c := range []struct {
		from, host string
		want       int
	}{
		{"127.0.0.1:50000", "127.0.0.1:7171", http.StatusOK},
		{"127.0.0.1:50000", "localhost:7171", http.StatusOK},
		{"[::1]:50000", "[::1]:7171", http.StatusOK},
		{"127.0.0.1:50000", "evil.example:7171", http.StatusForbidden}, // DNS rebinding
		{"127.0.0.1:50000", "", http.StatusForbidden},
		{"192.0.2.7:50000", "127.0.0.1:7171", http.StatusForbidden}, // another machine
	} {
		r := httptest.NewRequest("GET", "/api/v1/viewer", nil)
		r.RemoteAddr, r.Host = c.from, c.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("from %s to %q: %d, want %d", c.from, c.host, w.Code, c.want)
		}
	}
}

// The console is told it is looking at standalone, and standalone's identity
// settings cannot be changed out from under its own sign-in.
func TestStandaloneTellsTheConsoleAndFixesItsIdentity(t *testing.T) {
	srv := testServerWithIdP(t)
	srv.cfg.Standalone = true
	base := "http://" + srv.Addr()
	browser := browserClient(t)
	resp, err := browser.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin := &apiClient{t: t, base: base, http: browser}

	var org, public struct{ Standalone bool }
	admin.get("/api/v1/org", &org)
	(&apiClient{t: t, base: base}).get("/api/v1/org/public", &public)
	if !org.Standalone || !public.Standalone {
		t.Fatalf("standalone: org %v, public %v", org.Standalone, public.Standalone)
	}

	code, body := admin.do("PATCH", "/api/v1/org", map[string]any{"sso": map[string]any{"issuer": "https://idp.example"}})
	if code != http.StatusConflict {
		t.Fatalf("changing standalone's identity provider = %d: %s", code, body)
	}
	admin.post("PATCH", "/api/v1/org", map[string]any{"name": "Demo"}, nil) // the rest still saves
}

// The console lists known models from agentkit's catalog — before sign-in
// too, for first-run setup — and adding one takes a provider, a model and a
// key; the catalog fills in the rest.
func TestAModelFromTheCatalogNeedsOnlyAProviderAModelAndAKey(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	var catalog []struct{ Provider, ID, Name string }
	(&apiClient{t: t, base: base}).get("/api/v1/models/catalog", &catalog)
	found := false
	for _, c := range catalog {
		if c.Provider == "anthropic" && c.ID == llm.ClaudeOpus5.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("catalog has no %s: %+v", llm.ClaudeOpus5.ID, catalog)
	}

	browser := browserClient(t)
	resp, err := browser.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin := &apiClient{t: t, base: base, http: browser}
	var saved struct{ ID, Name string }
	admin.post("POST", "/api/v1/models", map[string]any{
		"provider": "anthropic", "modelId": llm.ClaudeOpus5.ID, "apiKey": "sk-test",
	}, &saved)
	m, err := srv.Store().ModelByID(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != llm.ClaudeOpus5.Name || m.CostOut != llm.ClaudeOpus5.Cost.Output {
		t.Fatalf("saved = %+v", m)
	}
}
