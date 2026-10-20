package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/popstarapps/john/client/credstore"
)

// The access token this machine carries lives hours, which is short enough
// that nothing here can be left to a sign-in months ago. Two things keep it
// current: a check before the session starts, and a ticker while it runs.
//
// Nothing about this is visible in normal use. It becomes visible in exactly
// one case — the sign-in itself is over, because it was revoked or the
// organization's cap ran out — and then it says so plainly, because the fix
// is a thing only the developer can do.

const (
	// refreshLead is how long before expiry a token is renewed. It is a large
	// fraction of the token's life on purpose: renewing early means a laptop
	// that sleeps through the window still wakes up with a working token,
	// and a backend that is briefly unreachable gets many more tries.
	refreshLead = 4 * time.Hour
)

// refreshEvery is how often a running session re-checks. Sessions outlive a
// token's life, so this cannot be left to start-up alone. A variable so tests
// can drive the loop without waiting half an hour.
var refreshEvery = 30 * time.Minute

// ErrSignInRequired means refreshing is over: the sign-in was revoked, hit
// the organization's maximum machine session lifetime, or the refresh token
// expired unused. Only `john login` fixes it.
var ErrSignInRequired = errors.New("your sign-in has expired — run `john login`")

// BackendUnavailableError means the backend did not answer the refresh at
// all: it is down, restarting, or behind a proxy with nothing to send to. It is
// not a sign-in problem, and saying so is most of the help — the instinct on
// seeing "sign-in" and "expired" together is to run `john login`, which would
// fail the same way.
type BackendUnavailableError struct {
	URL    string
	Status int   // the 5xx it answered with; 0 when nothing answered
	Err    error // the transport error, when there was one
}

func (e *BackendUnavailableError) Error() string {
	return fmt.Sprintf("can't reach the john backend at %s (%s).\n"+
		"      Your sign-in is still valid — this isn't a login problem.\n"+
		"      The backend may be restarting or down. Try again in a minute.", e.URL, e.Reason())
}

func (e *BackendUnavailableError) Unwrap() error { return e.Err }

// Reason is the short why: the status it answered with, or what became of
// the connection when nothing did.
func (e *BackendUnavailableError) Reason() string {
	if e.Status != 0 {
		return fmt.Sprintf("%d %s", e.Status, http.StatusText(e.Status))
	}
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.Is(e.Err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(e.Err, &dns):
		return "host not found"
	case errors.As(e.Err, &ne) && ne.Timeout():
		return "timed out"
	}
	return "no answer"
}

// refreshClient bounds getting a connection, and only that. A backend that
// has the request may already have spent the refresh token, and giving up on
// its answer would mean presenting the spent one next time — which revokes
// the whole sign-in. Waiting on a slow answer is the lesser problem.
var refreshClient = &http.Client{Transport: &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	TLSHandshakeTimeout: 10 * time.Second,
}}

// refreshNow trades the stored refresh token for a new pair and saves it.
func refreshNow(c *credstore.Credentials) (*credstore.Credentials, error) {
	if c.Refresh == "" {
		// Signed in before this machine knew about refresh tokens. The access
		// token it holds still works until it expires; there is nothing to
		// renew it with.
		return c, nil
	}
	host, _ := os.Hostname()

	body, err := json.Marshal(map[string]string{"refreshToken": c.Refresh, "host": host})
	if err != nil {
		return nil, err
	}
	resp, err := refreshClient.Post(c.BackendURL+"/api/v1/auth/refresh", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, &BackendUnavailableError{URL: c.BackendURL, Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrSignInRequired
	}
	if resp.StatusCode >= 500 {
		return nil, &BackendUnavailableError{URL: c.BackendURL, Status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refreshing this machine's sign-in: status %d", resp.StatusCode)
	}

	var out struct {
		Token            string    `json:"token"`
		ExpiresAt        time.Time `json:"expiresAt"`
		RefreshToken     string    `json:"refreshToken"`
		RefreshExpiresAt time.Time `json:"refreshExpiresAt"`
		User             struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Token == "" || out.RefreshToken == "" {
		return nil, errors.New("the backend answered a refresh without a credential")
	}

	next := *c
	next.Token, next.ExpiresAt = out.Token, out.ExpiresAt
	next.Refresh, next.RefreshExpiresAt = out.RefreshToken, out.RefreshExpiresAt
	if out.User.Email != "" {
		next.Email = out.User.Email
	}

	// Saved before returning, and this is the one write that must not be
	// lost: the old refresh token is spent the moment the backend answered,
	// so a pair that reaches this process and not the disk is a pair nobody
	// holds. A failure here is reported rather than swallowed.
	if err := credstore.Save(&next); err != nil {
		return nil, fmt.Errorf("storing refreshed credentials: %w", err)
	}
	return &next, nil
}

// needsRefresh reports whether the access token is close enough to expiry to
// renew. Credentials from before this existed carry no expiry and are left
// alone: there is nothing to renew them with.
func needsRefresh(c *credstore.Credentials) bool {
	if c.Refresh == "" {
		return false
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return time.Now().Add(refreshLead).After(c.ExpiresAt)
}

// ensureFresh renews the credentials if they are near expiry. A backend that
// cannot be reached is not fatal — the token in hand may still have hours
// left, and the session should start — but an expired sign-in is. So is an
// unreachable backend once the token in hand has run out; that comes back as
// a *BackendUnavailableError, which waitForBackend knows how to sit out.
func ensureFresh(c *credstore.Credentials) (*credstore.Credentials, error) {
	if !needsRefresh(c) {
		return c, nil
	}
	next, err := refreshNow(c)
	if errors.Is(err, ErrSignInRequired) {
		return nil, err
	}
	if err != nil {
		if time.Now().After(c.ExpiresAt) && !c.ExpiresAt.IsZero() {
			var down *BackendUnavailableError
			if errors.As(err, &down) {
				return nil, err // says what to do, which a suffix would muddy
			}
			return nil, fmt.Errorf("%w (and this machine's token has expired)", err)
		}
		return c, nil // keep going on what we have
	}
	return next, nil
}

// tokenHolder is whatever needs to be told when the token changes underneath
// it. The wire is the only implementation; it dials with the token it is
// holding at that moment, so a reconnect after a refresh has to use the new
// one.
type tokenHolder interface {
	setToken(string)
}

// refresher keeps a running session's credentials current.
type refresher struct {
	mu     sync.Mutex
	creds  *credstore.Credentials
	holder tokenHolder

	// onExpired is called once, when refreshing has permanently failed. The
	// session cannot outlive its credentials, and pretending otherwise would
	// leave a developer watching reconnects fail with no explanation.
	onExpired func(error)
}

// run renews on a ticker until the context ends.
func (r *refresher) run(done <-chan struct{}) {
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			r.tick()
		}
	}
}

func (r *refresher) tick() {
	r.mu.Lock()
	c := r.creds
	r.mu.Unlock()
	if c == nil || !needsRefresh(c) {
		return
	}

	next, err := refreshNow(c)
	if errors.Is(err, ErrSignInRequired) {
		if r.onExpired != nil {
			r.onExpired(err)
			r.onExpired = nil
		}
		return
	}
	if err != nil {
		return // transient; the ticker comes back
	}

	r.mu.Lock()
	r.creds = next
	r.mu.Unlock()
	if r.holder != nil {
		r.holder.setToken(next.Token)
	}
}
