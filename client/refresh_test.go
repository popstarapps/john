package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/backend"
	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/terminalui"
)

// signedInAgainstARealBackend runs the whole sign-in — CLI start, a headless
// browser through the embedded identity provider, poll — and returns the
// credentials the backend minted, stored where credstore will find them.
func signedInAgainstARealBackend(t *testing.T) (*backend.Server, *credstore.Credentials) {
	t.Helper()
	t.Setenv("JOHN_HOME", t.TempDir())

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
	t.Cleanup(func() { cancel(); <-done; srv.Close() })

	if err := srv.Store().SaveOrg(&store.Org{
		Name:          "Test Org",
		OIDCIssuer:    "http://" + srv.Addr() + "/devidp",
		OIDCClientID:  "john",
		AdminEmail:    "oliver@example.com",
		SetupComplete: true,
	}); err != nil {
		t.Fatal(err)
	}

	base := "http://" + srv.Addr()
	var start struct{ Code, URL string }
	if err := postJSON(base+"/api/v1/auth/cli/start", map[string]string{}, &start, nil); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	resp, err := browser.Get(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var done2 struct {
		Token            string    `json:"token"`
		ExpiresAt        time.Time `json:"expiresAt"`
		RefreshToken     string    `json:"refreshToken"`
		RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
	}
	status := 0
	if err := postJSON(base+"/api/v1/auth/cli/poll",
		map[string]string{"code": start.Code, "host": "test"}, &done2, &status); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || done2.RefreshToken == "" {
		t.Fatalf("poll = %d %+v", status, done2)
	}

	creds := &credstore.Credentials{
		BackendURL:       base,
		Token:            done2.Token,
		ExpiresAt:        done2.ExpiresAt,
		Refresh:          done2.RefreshToken,
		RefreshExpiresAt: done2.RefreshExpiresAt,
		Email:            "oliver@example.com",
	}
	if err := credstore.Save(creds); err != nil {
		t.Fatal(err)
	}
	return srv, creds
}

// A machine that has not run john since yesterday renews on the way in, and
// the renewal reaches the disk — a pair that only ever existed in one process
// is a pair nobody holds, because the old refresh token is already spent.
func TestStartingUpRenewsAnAgeingTokenAndStoresIt(t *testing.T) {
	_, creds := signedInAgainstARealBackend(t)

	// Backdate the access token to just inside the renewal window, which is
	// the state a laptop wakes up in.
	stale := *creds
	stale.ExpiresAt = time.Now().Add(refreshLead / 2)
	if err := credstore.Save(&stale); err != nil {
		t.Fatal(err)
	}

	next, err := ensureFresh(&stale)
	if err != nil {
		t.Fatalf("ensureFresh: %v", err)
	}
	if next.Token == stale.Token {
		t.Fatal("ensureFresh did not renew a token inside the renewal window")
	}
	if next.Refresh == stale.Refresh {
		t.Fatal("the refresh token was not rotated")
	}

	stored, err := credstore.Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Token != next.Token || stored.Refresh != next.Refresh {
		t.Fatal("the renewed pair never reached the disk")
	}
}

// A token with hours left is left alone. Renewing on every start would spend
// a refresh token per invocation for nothing.
func TestAFreshTokenIsLeftAlone(t *testing.T) {
	_, creds := signedInAgainstARealBackend(t)

	next, err := ensureFresh(creds)
	if err != nil {
		t.Fatal(err)
	}
	if next.Token != creds.Token || next.Refresh != creds.Refresh {
		t.Fatal("a token nowhere near expiry was renewed anyway")
	}
}

// The one failure a developer has to be told about: refreshing is over and
// only signing in again fixes it. It must not be reported as a network blip.
func TestAnEndedSignInSaysSoRatherThanRetryingForever(t *testing.T) {
	srv, creds := signedInAgainstARealBackend(t)

	user, err := srv.Store().UserByEmail("oliver@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store().RevokeMachineSessionsFor(user.ID); err != nil {
		t.Fatal(err)
	}

	stale := *creds
	stale.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := ensureFresh(&stale); err != ErrSignInRequired {
		t.Fatalf("ensureFresh after revocation = %v, want ErrSignInRequired", err)
	}
}

// A backend that cannot be reached is not the same as a sign-in that ended.
// The token in hand may still have hours on it, and the session should start.
func TestAnUnreachableBackendDoesNotStopASessionStarting(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	creds := &credstore.Credentials{
		BackendURL: "http://127.0.0.1:1", // nothing listens here
		Token:      "still-valid-for-hours",
		Refresh:    "some-refresh-token",
		ExpiresAt:  time.Now().Add(refreshLead / 2),
	}
	next, err := ensureFresh(creds)
	if err != nil {
		t.Fatalf("an unreachable backend was treated as fatal: %v", err)
	}
	if next.Token != creds.Token {
		t.Fatal("credentials changed despite the backend being unreachable")
	}
}

// The session keeps the wire's token current: a reconnect after a renewal has
// to dial with the new one, or it would be refused with a token the machine
// has already replaced.
func TestARenewalReachesTheWire(t *testing.T) {
	_, creds := signedInAgainstARealBackend(t)

	stale := *creds
	stale.ExpiresAt = time.Now().Add(refreshLead / 2)

	holder := &recordingHolder{}
	r := &refresher{creds: &stale, holder: holder}
	r.tick()

	if holder.current() == "" {
		t.Fatal("the wire was never told about the renewed token")
	}
	if holder.current() == stale.Token {
		t.Fatal("the wire was handed the token that was being replaced")
	}
}

// recordingHolder stands in for the wire. The refresher writes to it from its
// own goroutine, so the reads are guarded.
type recordingHolder struct {
	mu    sync.Mutex
	token string
}

func (h *recordingHolder) setToken(t string) {
	h.mu.Lock()
	h.token = t
	h.mu.Unlock()
}

func (h *recordingHolder) current() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.token
}

// The ticker is what keeps a long session alive: a session can easily outlast
// a twelve-hour token, and the start-up check does nothing for it.
func TestARunningSessionKeepsRenewingOnItsOwn(t *testing.T) {
	_, creds := signedInAgainstARealBackend(t)

	old := refreshEvery
	refreshEvery = 10 * time.Millisecond
	t.Cleanup(func() { refreshEvery = old })

	stale := *creds
	stale.ExpiresAt = time.Now().Add(refreshLead / 2)

	holder := &recordingHolder{}
	r := &refresher{creds: &stale, holder: holder}
	done := make(chan struct{})
	go r.run(done)

	deadline := time.After(5 * time.Second)
	for {
		if holder.current() != "" {
			break
		}
		select {
		case <-deadline:
			close(done)
			t.Fatal("the session never renewed on its own")
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(done)

	if got := holder.current(); got == stale.Token {
		t.Fatal("the wire was handed the token that was being replaced")
	}

	// And it stops when the session does, rather than renewing forever.
	settled := holder.current()
	time.Sleep(50 * time.Millisecond)
	if holder.current() != settled {
		t.Fatal("the refresher kept going after the session ended")
	}
}

// Credentials stored before expiry was recorded carry a zero time. That is
// "unknown", not "expired in 1970", and it has to mean renew rather than
// silently never renewing.
func TestCredentialsWithNoRecordedExpiryAreRenewed(t *testing.T) {
	_, creds := signedInAgainstARealBackend(t)

	old := *creds
	old.ExpiresAt = time.Time{}
	if !needsRefresh(&old) {
		t.Fatal("credentials with no known expiry were left alone forever")
	}

	// But a machine with no refresh token at all cannot be renewed, and
	// asking would only produce a failure on every start-up.
	preRefresh := credstore.Credentials{BackendURL: creds.BackendURL, Token: "old-style-token"}
	if needsRefresh(&preRefresh) {
		t.Fatal("a machine with nothing to refresh with was asked to refresh")
	}
}

// A backend that is down while the token has run out is not a sign-in
// problem, and the message must not read like one: the instinct on seeing
// "sign-in" and "expired" together is `john login`, which fails the same way.
func TestADownBackendWithAnExpiredTokenSaysItIsTheBackend(t *testing.T) {
	t.Setenv("JOHN_HOME", t.TempDir())

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer gateway.Close()

	for name, url := range map[string]string{
		"502":     gateway.URL,
		"refused": "http://127.0.0.1:1",
	} {
		t.Run(name, func(t *testing.T) {
			creds := &credstore.Credentials{
				BackendURL: url,
				Token:      "expired",
				Refresh:    "some-refresh-token",
				ExpiresAt:  time.Now().Add(-time.Minute),
			}
			_, err := ensureFresh(creds)
			var down *BackendUnavailableError
			if !errors.As(err, &down) {
				t.Fatalf("ensureFresh = %v, want *BackendUnavailableError", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, url) || !strings.Contains(msg, "isn't a login problem") {
				t.Fatalf("message does not name the backend or rule out sign-in: %q", msg)
			}
			if strings.Contains(msg, "expired") {
				t.Fatalf("message still reads like a sign-in problem: %q", msg)
			}
		})
	}
	creds := &credstore.Credentials{BackendURL: gateway.URL, Refresh: "x", ExpiresAt: time.Now().Add(-time.Minute)}
	_, err := ensureFresh(creds)
	if !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("message does not carry the status: %q", err)
	}
}

func fastBackendWait(t *testing.T, limit time.Duration) {
	t.Helper()
	first, max, lim, frame := backendWaitFirst, backendWaitMax, backendWaitLimit, backendWaitFrame
	backendWaitFirst, backendWaitMax, backendWaitLimit, backendWaitFrame =
		5*time.Millisecond, 10*time.Millisecond, limit, time.Millisecond
	t.Cleanup(func() {
		backendWaitFirst, backendWaitMax, backendWaitLimit, backendWaitFrame = first, max, lim, frame
	})
}

// A deploy is over in seconds; the session starts the moment it is.
func TestWaitingOutABackendThatComesBack(t *testing.T) {
	fastBackendWait(t, time.Minute)

	calls := 0
	want := &credstore.Credentials{Token: "renewed"}
	var out bytes.Buffer
	got, err := waitForBackend(context.Background(), &out, terminalui.DefaultTheme(io.Discard),
		func() (*credstore.Credentials, error) {
			calls++
			if calls < 3 {
				return nil, &BackendUnavailableError{URL: "https://john.example.com", Status: 502}
			}
			return want, nil
		})
	if err != nil || got != want {
		t.Fatalf("waitForBackend = %v, %v; want the renewed credentials", got, err)
	}
	if calls != 3 {
		t.Fatalf("tried %d times, want 3", calls)
	}
	if !strings.Contains(out.String(), "john.example.com is unavailable (502 Bad Gateway)") {
		t.Fatalf("the wait did not say what it was waiting for: %q", out.String())
	}
}

// Past the limit it is an outage, not a blip, and it is reported as one.
func TestWaitingForABackendGivesUp(t *testing.T) {
	fastBackendWait(t, 30*time.Millisecond)

	_, err := waitForBackend(context.Background(), io.Discard, terminalui.DefaultTheme(io.Discard),
		func() (*credstore.Credentials, error) {
			return nil, &BackendUnavailableError{URL: "https://john.example.com", Status: 503}
		})
	var down *BackendUnavailableError
	if !errors.As(err, &down) {
		t.Fatalf("waitForBackend = %v, want the outage reported", err)
	}
}

// Only an unavailable backend is worth waiting for; an ended sign-in is not
// going to come back by itself.
func TestWaitingDoesNotSitOutAnEndedSignIn(t *testing.T) {
	fastBackendWait(t, time.Minute)

	calls := 0
	_, err := waitForBackend(context.Background(), io.Discard, terminalui.DefaultTheme(io.Discard),
		func() (*credstore.Credentials, error) {
			calls++
			return nil, ErrSignInRequired
		})
	if err != ErrSignInRequired || calls != 1 {
		t.Fatalf("waitForBackend = %v after %d tries, want ErrSignInRequired after 1", err, calls)
	}
}

// Ctrl-C ends the wait at once.
func TestWaitingForABackendCanBeInterrupted(t *testing.T) {
	fastBackendWait(t, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waitForBackend(ctx, io.Discard, terminalui.DefaultTheme(io.Discard),
		func() (*credstore.Credentials, error) {
			return nil, &BackendUnavailableError{URL: "https://john.example.com"}
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForBackend = %v, want context.Canceled", err)
	}
}
