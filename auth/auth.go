// Package auth is the backend's identity layer: the OIDC relying party that
// signs people in through the organization's identity provider, the browser
// sessions and machine tokens that carry that identity afterwards, and the
// CLI login flow that gets a token onto a developer machine with one
// registered redirect URI.
//
// The backend is the OIDC client; browsers and CLIs only ever talk to the
// backend. Nothing here ever sees a model provider credential — identity and
// model access are different doors on purpose.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/popstarapps/john/store"
)

// SessionCookie is the browser session cookie name.
const SessionCookie = "john_session"

const (
	webSessionTTL = 7 * 24 * time.Hour
	flowTTL       = 10 * time.Minute

	// A machine holds two credentials. The access token goes out on every
	// request and lives hours, so the thing most exposed is worth least; the
	// refresh token is presented to one endpoint and rotates each time, so
	// the thing worth most is seen rarely enough that its misuse stands out.
	machineAccessTTL  = 12 * time.Hour
	machineRefreshTTL = 30 * 24 * time.Hour

	// How long a refresh token that has already rotated keeps working. It
	// covers exactly one thing: a client that sent a refresh, was answered,
	// and lost the answer. Anything longer widens the window in which a
	// stolen copy passes unnoticed.
	machineRefreshGrace = time.Minute
)

// Service holds the login flows and answers "who is this request".
type Service struct {
	st *store.Store

	mu       sync.Mutex
	provider *oidc.Provider
	issuer   string              // issuer the cached provider was built for
	web      map[string]*webFlow // by state
	cli      map[string]*cliFlow // by CLI code
}

type webFlow struct {
	verifier string
	nonce    string
	next     string // where the browser goes after
	cliCode  string // non-empty when this sign-in belongs to a CLI login
	expires  time.Time
}

type cliFlow struct {
	userID  string // empty until the browser flow binds a user
	expires time.Time
}

// New creates the service.
func New(st *store.Store) *Service {
	return &Service{st: st, web: map[string]*webFlow{}, cli: map[string]*cliFlow{}}
}

// --- request identity ---

// UserFor answers who a request is: a browser with a session cookie, or a
// client with a Bearer machine token. Nil when nobody.
func (s *Service) UserFor(r *http.Request) *store.User {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		u, err := s.st.UserForMachineToken(strings.TrimPrefix(h, "Bearer "))
		if err == nil && u.Status == "active" {
			return u
		}
		return nil
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		u, err := s.st.UserForWebSession(c.Value)
		if err == nil && u.Status == "active" {
			return u
		}
	}
	return nil
}

// --- the OIDC relying party ---

// oidcClient builds (and caches) the provider client from the org row, so a
// changed IdP configuration takes effect without a restart.
func (s *Service) oidcClient(ctx context.Context, baseURL string) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	org, err := s.st.Org()
	if err != nil {
		return nil, nil, err
	}
	if org.OIDCIssuer == "" {
		return nil, nil, errors.New("auth: no identity provider configured")
	}

	s.mu.Lock()
	provider := s.provider
	if provider == nil || s.issuer != org.OIDCIssuer {
		s.mu.Unlock()
		p, err := oidc.NewProvider(ctx, org.OIDCIssuer)
		if err != nil {
			return nil, nil, fmt.Errorf("auth: discover %s: %w", org.OIDCIssuer, err)
		}
		s.mu.Lock()
		s.provider, s.issuer = p, org.OIDCIssuer
		provider = p
	}
	s.mu.Unlock()

	cfg := &oauth2.Config{
		ClientID:     org.OIDCClientID,
		ClientSecret: org.OIDCClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  baseURL + "/auth/callback",
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	return cfg, provider.Verifier(&oidc.Config{ClientID: org.OIDCClientID}), nil
}

// BaseURL derives the externally visible base URL from the request, honouring
// a reverse proxy's forwarded headers.
func BaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	return scheme + "://" + host
}

// --- browser flow ---

// HandleLogin starts the sign-in: it parks a flow record and redirects to the
// identity provider with state, nonce, and PKCE.
//
// Query: next (path to land on after), cli (a CLI login code this sign-in
// should complete instead of setting a cookie).
func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	cfg, _, err := s.oidcClient(r.Context(), BaseURL(r))
	if err != nil {
		http.Error(w, "sign-in unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	state := randURLSafe()
	nonce := randURLSafe()
	verifier := randURLSafe()
	next := localPath(r.URL.Query().Get("next"))

	s.mu.Lock()
	s.gcLocked()
	s.web[state] = &webFlow{
		verifier: verifier,
		nonce:    nonce,
		next:     next,
		cliCode:  r.URL.Query().Get("cli"),
		expires:  time.Now().Add(flowTTL),
	}
	s.mu.Unlock()

	sum := sha256.Sum256([]byte(verifier))
	url := cfg.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:])),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
	http.Redirect(w, r, url, http.StatusFound)
}

// localPath keeps the redirect after sign-in on this site. It must be a path,
// and not one a browser reads as another host: "//host" is protocol-relative,
// browsers take "\" for "/", and they drop tabs and newlines before parsing,
// so "/\t/host" is "//host" by the time it is followed. Anything else is "/".
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsRune(next, '\\') {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	return next
}

// HandleCallback finishes the sign-in: code exchange, ID token verification,
// user upsert, then either a session cookie or a CLI binding.
func (s *Service) HandleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")

	s.mu.Lock()
	flow := s.web[state]
	delete(s.web, state)
	s.mu.Unlock()
	if flow == nil || time.Now().After(flow.expires) {
		http.Error(w, "sign-in expired — start again", http.StatusBadRequest)
		return
	}
	if errCode := q.Get("error"); errCode != "" {
		http.Error(w, "identity provider refused: "+errCode, http.StatusBadRequest)
		return
	}

	cfg, verifier, err := s.oidcClient(r.Context(), BaseURL(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	token, err := cfg.Exchange(r.Context(), q.Get("code"),
		oauth2.SetAuthURLParam("code_verifier", flow.verifier))
	if err != nil {
		http.Error(w, "code exchange failed", http.StatusBadGateway)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "identity provider returned no id_token", http.StatusBadGateway)
		return
	}
	idToken, err := verifier.Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "id_token verification failed", http.StatusBadGateway)
		return
	}
	if idToken.Nonce != flow.nonce {
		http.Error(w, "nonce mismatch", http.StatusBadRequest)
		return
	}
	var claims idClaims
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "unreadable claims", http.StatusBadGateway)
		return
	}

	org, err := s.st.Org()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// Before the user is created or matched by email: an address the
	// provider has not verified must not land on someone else's account.
	if err := admit(org.OIDCIssuer, org.AllowedDomains, claims); err != nil {
		http.Error(w, "sign-in refused: "+err.Error(), http.StatusForbidden)
		return
	}
	user, err := s.st.UpsertSSOUser(idToken.Subject, claims.Email, claims.Name, org.AdminEmail)
	if err != nil {
		http.Error(w, "sign-in failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if user.Status != "active" {
		http.Error(w, "this account is suspended", http.StatusForbidden)
		return
	}

	if flow.cliCode != "" {
		s.mu.Lock()
		if cf := s.cli[flow.cliCode]; cf != nil && time.Now().Before(cf.expires) {
			cf.userID = user.ID
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>john</title>"+
			"<body style=\"font-family:system-ui;margin:4rem\"><h2>Signed in as %s</h2>"+
			"<p>You can close this window and return to the terminal.</p>", htmlEscape(user.Email))
		return
	}

	secret, err := s.st.CreateWebSession(user.ID, webSessionTTL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    secret,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(webSessionTTL / time.Second),
	})
	http.Redirect(w, r, flow.next, http.StatusFound)
}

// HandleLogout drops the browser session.
func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		s.st.DeleteWebSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/signin", http.StatusFound)
}

// --- CLI flow ---

// StartCLI begins a CLI login and returns the code the CLI polls with and the
// URL it opens in a browser.
func (s *Service) StartCLI(baseURL string) (code, url string) {
	code = randURLSafe()
	s.mu.Lock()
	s.gcLocked()
	s.cli[code] = &cliFlow{expires: time.Now().Add(flowTTL)}
	s.mu.Unlock()
	return code, baseURL + "/auth/login?cli=" + code
}

// ErrPending means the browser half of a CLI login has not finished.
var ErrPending = errors.New("auth: sign-in not completed yet")

// PollCLI redeems a CLI code once its browser flow has bound a user: it mints
// the machine's credential pair and forgets the code. Before that, ErrPending.
func (s *Service) PollCLI(code, host string) (creds *store.MachineCredentials, user *store.User, err error) {
	s.mu.Lock()
	cf := s.cli[code]
	if cf == nil || time.Now().After(cf.expires) {
		delete(s.cli, code)
		s.mu.Unlock()
		return nil, nil, errors.New("auth: unknown or expired login code")
	}
	if cf.userID == "" {
		s.mu.Unlock()
		return nil, nil, ErrPending
	}
	delete(s.cli, code) // single redemption
	userID := cf.userID
	s.mu.Unlock()

	user, err = s.st.UserByID(userID)
	if err != nil {
		return nil, nil, err
	}
	creds, err = s.st.StartMachineSession(user.ID, host, s.machineTTL())
	if err != nil {
		return nil, nil, err
	}
	return creds, user, nil
}

// Refresh trades a refresh token for a new pair. It is the only place a
// refresh token is ever presented, and every outcome other than success ends
// the same way for the client: sign in again through the identity provider.
func (s *Service) Refresh(refreshToken, host string) (*store.MachineCredentials, *store.User, error) {
	return s.st.RotateMachineSession(refreshToken, host, s.machineTTL())
}

// machineTTL reads the organization's cap on how long a machine may keep
// refreshing. A backend with no organization row yet cannot mint anything, so
// the zero cap there is never reached; standalone sets the cap to zero on
// purpose, having no identity provider to send anyone back to.
func (s *Service) machineTTL() store.MachineTTL {
	ttl := store.MachineTTL{
		Access:  machineAccessTTL,
		Refresh: machineRefreshTTL,
		Grace:   machineRefreshGrace,
	}
	if org, err := s.st.Org(); err == nil && org.MachineSessionMaxDays > 0 {
		ttl.MaxLife = time.Duration(org.MachineSessionMaxDays) * 24 * time.Hour
	}
	return ttl
}

// gcLocked drops expired flows. Called with mu held from flow starts.
func (s *Service) gcLocked() {
	now := time.Now()
	for k, f := range s.web {
		if now.After(f.expires) {
			delete(s.web, k)
		}
	}
	for k, f := range s.cli {
		if now.After(f.expires) {
			delete(s.cli, k)
		}
	}
}

func randURLSafe() string {
	var b [24]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
