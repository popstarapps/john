package client

import (
	"bufio"
	"context"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/backend"
	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/store"
)

// TestLoginFlowAgainstRealBackend runs the CLI half of `john login` against a
// real backend with the embedded IdP, with a headless "browser" completing
// the other half — the whole credential path, no mocks.
func TestLoginFlowAgainstRealBackend(t *testing.T) {
	srv, err := backend.New(backend.Config{
		DBPath:  filepath.Join(t.TempDir(), "john.db"),
		Listen:  "127.0.0.1:0",
		Version: "test",
		DevIdP:  &devidp.Identity{Subject: "dev-1", Email: "oliver@example.com", Name: "Oliver K"},
	})
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	defer func() { cancel(); <-done; srv.Close() }()

	if err := srv.Store().SaveOrg(&store.Org{
		Name:          "Test Org",
		OIDCIssuer:    "http://" + srv.Addr() + "/devidp",
		OIDCClientID:  "john",
		AdminEmail:    "oliver@example.com",
		SetupComplete: true,
	}); err != nil {
		t.Fatal(err)
	}

	old := loginPollInterval
	loginPollInterval = 50 * time.Millisecond
	defer func() { loginPollInterval = old }()

	// loginFlow prints the URL it wants a browser to open; capture it from a
	// pipe and open it with a cookie-jar client instead.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pr.Close(); pw.Close() })
	type result struct {
		creds *credstore.Credentials
		err   error
	}
	resc := make(chan result, 1)
	go func() {
		creds, err := loginFlow("http://"+srv.Addr(), pw)
		resc <- result{creds, err}
	}()

	url := waitForURL(t, pr)
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	resp, err := browser.Get(url)
	if err != nil {
		t.Fatalf("browser: %v", err)
	}
	resp.Body.Close()

	select {
	case res := <-resc:
		if res.err != nil {
			t.Fatalf("loginFlow: %v", res.err)
		}
		if res.creds.Email != "oliver@example.com" || res.creds.Token == "" {
			t.Fatalf("loginFlow = %q %q", res.creds.Email, res.creds.Token)
		}
		// Both halves come back, and the access token is the short-lived one:
		// a machine that was handed only a long-lived token would be back to
		// the credential this whole change exists to remove.
		if res.creds.Refresh == "" {
			t.Fatal("loginFlow returned no refresh token")
		}
		if d := time.Until(res.creds.ExpiresAt); d > 24*time.Hour {
			t.Fatalf("access token lives %v — that is not short-lived", d)
		}
		if !res.creds.RefreshExpiresAt.After(res.creds.ExpiresAt) {
			t.Fatal("refresh token does not outlive the access token")
		}
		// The token authenticates.
		req, _ := http.NewRequest("GET", "http://"+srv.Addr()+"/api/v1/viewer", nil)
		req.Header.Set("Authorization", "Bearer "+res.creds.Token)
		r2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r2.Body.Close()
		if r2.StatusCode != http.StatusOK {
			t.Fatalf("viewer with minted token = %d", r2.StatusCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("loginFlow never returned")
	}
}

// waitForURL scans the pipe for the sign-in URL loginFlow prints.
func waitForURL(t *testing.T, r *os.File) string {
	t.Helper()
	urls := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
				urls <- line
				return
			}
		}
	}()
	select {
	case u := <-urls:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("loginFlow never printed a sign-in URL")
		return ""
	}
}

func TestCredstoreRoundTrip(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	if _, err := credstore.Load(); err != credstore.ErrNotLoggedIn {
		t.Fatalf("fresh Load err = %v, want ErrNotLoggedIn", err)
	}
	in := &credstore.Credentials{BackendURL: "http://b", Token: "tok", Email: "a@b.c"}
	if err := credstore.Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := credstore.Load()
	if err != nil || *out != *in {
		t.Fatalf("Load = %+v, %v", out, err)
	}
	if err := credstore.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := credstore.Load(); err != credstore.ErrNotLoggedIn {
		t.Fatalf("after Clear err = %v", err)
	}
}
