// Package devidp is a small, real OIDC provider the backend can host itself:
// discovery, authorize, token, and JWKS endpoints, RS256-signed ID tokens,
// PKCE verification, auto-consent. Standalone mode and the test suite sign in
// through it, which means the backend's production OIDC path — discovery,
// code exchange, signature verification — is exercised on every `john
// standalone` boot and every CI run, not just against a customer's IdP.
//
// It is deliberately not a general IdP: one configured identity, consent is
// automatic, and nothing persists. Trusting it outside a loopback deployment
// would be a mistake, which is why the backend only mounts it when standalone
// or a test asks.
package devidp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Identity is who every authorize call signs in as.
type Identity struct {
	Subject string
	Email   string
	Name    string
}

// Provider is a running dev identity provider. Mount Handler under the path
// that makes Issuer true.
type Provider struct {
	issuer string
	key    *rsa.PrivateKey
	keyID  string

	mu       sync.Mutex
	identity Identity
	codes    map[string]*pendingCode
}

type pendingCode struct {
	clientID    string
	redirectURI string
	nonce       string
	challenge   string // S256 code challenge
	identity    Identity
	expires     time.Time
}

// New creates a provider that will be served at issuer (e.g.
// "http://127.0.0.1:8080/devidp").
func New(issuer string, identity Identity) (*Provider, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(x509.MarshalPKCS1PublicKey(&key.PublicKey))
	return &Provider{
		issuer:   strings.TrimSuffix(issuer, "/"),
		key:      key,
		keyID:    hex.EncodeToString(sum[:8]),
		identity: identity,
		codes:    map[string]*pendingCode{},
	}, nil
}

// Issuer returns the issuer URL the provider was created for.
func (p *Provider) Issuer() string { return p.issuer }

// SetIdentity changes who subsequent authorize calls sign in as.
func (p *Provider) SetIdentity(id Identity) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.identity = id
}

// Handler serves the provider. Routes are relative to the mount point.
func (p *Provider) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.handleDiscovery)
	mux.HandleFunc("GET /authorize", p.handleAuthorize)
	mux.HandleFunc("POST /token", p.handleToken)
	mux.HandleFunc("GET /jwks", p.handleJWKS)
	return mux
}

func (p *Provider) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                p.issuer,
		"authorization_endpoint":                p.issuer + "/authorize",
		"token_endpoint":                        p.issuer + "/token",
		"jwks_uri":                              p.issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
	})
}

// handleAuthorize is the whole consent experience: there is none. The
// configured identity is signed in and sent straight back.
func (p *Provider) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirectURI := q.Get("redirect_uri")
	u, err := url.Parse(redirectURI)
	if err != nil || redirectURI == "" {
		http.Error(w, "devidp: bad redirect_uri", http.StatusBadRequest)
		return
	}
	if q.Get("response_type") != "code" {
		http.Error(w, "devidp: only response_type=code", http.StatusBadRequest)
		return
	}

	code := randToken()
	p.mu.Lock()
	p.codes[code] = &pendingCode{
		clientID:    q.Get("client_id"),
		redirectURI: redirectURI,
		nonce:       q.Get("nonce"),
		challenge:   q.Get("code_challenge"),
		identity:    p.identity,
		expires:     time.Now().Add(5 * time.Minute),
	}
	p.mu.Unlock()

	out := u.Query()
	out.Set("code", code)
	if state := q.Get("state"); state != "" {
		out.Set("state", state)
	}
	u.RawQuery = out.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (p *Provider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, "invalid_request", err.Error())
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, "unsupported_grant_type", "only authorization_code")
		return
	}
	code := r.PostForm.Get("code")

	p.mu.Lock()
	pc := p.codes[code]
	delete(p.codes, code) // single use, success or not
	p.mu.Unlock()

	if pc == nil || time.Now().After(pc.expires) {
		oauthError(w, "invalid_grant", "unknown or expired code")
		return
	}
	if uri := r.PostForm.Get("redirect_uri"); uri != "" && uri != pc.redirectURI {
		oauthError(w, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if pc.challenge != "" {
		verifier := r.PostForm.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != pc.challenge {
			oauthError(w, "invalid_grant", "PKCE verification failed")
			return
		}
	}

	now := time.Now()
	claims := map[string]any{
		"iss":            p.issuer,
		"sub":            pc.identity.Subject,
		"aud":            pc.clientID,
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
		"email":          pc.identity.Email,
		"email_verified": true,
		"name":           pc.identity.Name,
	}
	if pc.nonce != "" {
		claims["nonce"] = pc.nonce
	}
	idToken, err := p.signJWT(claims)
	if err != nil {
		oauthError(w, "server_error", err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"access_token": randToken(),
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func (p *Provider) handleJWKS(w http.ResponseWriter, r *http.Request) {
	pub := p.key.Public().(*rsa.PublicKey)
	writeJSON(w, map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": p.keyID,
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	})
}

func (p *Provider) signJWT(claims map[string]any) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": p.keyID}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func randToken() string {
	var b [24]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `{"error":%q,"error_description":%q}`, code, desc)
}
