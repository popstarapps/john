package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/popstarapps/john/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "john.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestUserForResolvesBearerAndCookie proves request identity: a Bearer machine
// token and a session cookie both resolve to their user, and anything else is
// nobody.
func TestUserForResolvesBearerAndCookie(t *testing.T) {
	st := testStore(t)
	svc := New(st)
	u, err := st.UpsertSSOUser("sub-1", "dev@example.com", "Dev", "")
	if err != nil {
		t.Fatal(err)
	}

	token, _ := st.CreateMachineToken(u.ID, "laptop", time.Hour)
	cookie, _ := st.CreateWebSession(u.ID, time.Hour)

	// Bearer token.
	r := httptest.NewRequest("GET", "/api/v1/viewer", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if got := svc.UserFor(r); got == nil || got.ID != u.ID {
		t.Fatalf("bearer resolve = %v", got)
	}

	// Cookie session.
	r = httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	if got := svc.UserFor(r); got == nil || got.ID != u.ID {
		t.Fatalf("cookie resolve = %v", got)
	}

	// Nobody.
	if got := svc.UserFor(httptest.NewRequest("GET", "/", nil)); got != nil {
		t.Fatalf("anonymous request resolved to %v", got)
	}
	bad := httptest.NewRequest("GET", "/", nil)
	bad.Header.Set("Authorization", "Bearer garbage")
	if got := svc.UserFor(bad); got != nil {
		t.Fatalf("garbage token resolved to %v", got)
	}

	// A suspended user does not resolve even with a valid token.
	st.DB().Exec(`UPDATE users SET status = 'suspended' WHERE id = ?`, u.ID)
	r = httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if got := svc.UserFor(r); got != nil {
		t.Fatalf("suspended user resolved to %v", got)
	}
}

// TestCLIFlowStartAndPollStates covers the CLI login handshake's own states:
// a code is issued, polling is pending until a browser binds it (that binding
// is covered end-to-end in the backend package), and an unknown code fails.
func TestCLIFlowStartAndPollStates(t *testing.T) {
	svc := New(testStore(t))

	code, url := svc.StartCLI("https://backend.example")
	if code == "" || url == "" {
		t.Fatalf("start = %q %q", code, url)
	}
	if want := "https://backend.example/auth/login?cli=" + code; url != want {
		t.Fatalf("url = %q, want %q", url, want)
	}

	// Pending until the browser half binds a user.
	if _, _, err := svc.PollCLI(code, "laptop"); !errors.Is(err, ErrPending) {
		t.Fatalf("poll before binding = %v, want ErrPending", err)
	}
	// An unknown code is refused (and is not ErrPending).
	if _, _, err := svc.PollCLI("nope", "laptop"); err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("unknown code = %v, want a hard error", err)
	}
}

func TestBaseURLHonoursForwardedHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "http://internal:8080/auth/login", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "john.acme.example")
	if got := BaseURL(r); got != "https://john.acme.example" {
		t.Fatalf("BaseURL = %q", got)
	}
}

// The page to land on after sign-in comes from the query string, so it must
// not be able to send the browser to another site once it is signed in.
func TestLocalPathKeepsTheRedirectOnThisSite(t *testing.T) {
	for next, want := range map[string]string{
		"/policies":            "/policies",
		"/admin/models?add=1":  "/admin/models?add=1",
		"/%2F%2Fevil.example":  "/%2F%2Fevil.example", // encoded: a path here
		"":                     "/",
		"policies":             "/",
		"https://evil.example": "/",
		"//evil.example":       "/",
		"//evil.example/x":     "/",
		"/\\evil.example":      "/",
		"/\t/evil.example":     "/",
		"/\n/evil.example":     "/",
	} {
		if got := localPath(next); got != want {
			t.Errorf("localPath(%q) = %q, want %q", next, got, want)
		}
	}
}
