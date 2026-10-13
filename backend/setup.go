package backend

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"github.com/popstarapps/john/auth"
	"github.com/popstarapps/john/store"
)

// setupPayload is what the console's setup wizard submits — its State object,
// field for field.
type setupPayload struct {
	OrgName      string `json:"orgName"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	AdminEmail   string `json:"adminEmail"`
	// AllowedDomains are the email domains sign-in admits. Left out, setup
	// admits the administrator's own domain and no other.
	AllowedDomains []string     `json:"allowedDomains"`
	Models         []modelDraft `json:"models"`
	TraceKey       string       `json:"traceKey"`
	TracePath      string       `json:"tracePath"`
}

// modelDraft mirrors the console's ModelDraft.
type modelDraft struct {
	Name                string            `json:"name"`
	Provider            string            `json:"provider"`
	Hosting             string            `json:"hosting"`
	Endpoint            string            `json:"endpoint"`
	ModelID             string            `json:"modelId"`
	ContextWindow       int               `json:"contextWindow"`
	CostPerMTokIn       float64           `json:"costPerMTokIn"`
	CostPerMTokCachedIn float64           `json:"costPerMTokCachedIn"`
	CostPerMTokOut      float64           `json:"costPerMTokOut"`
	APIKey              string            `json:"apiKey"`
	Extra               map[string]string `json:"extra"`
}

// handleSetup is the whole bootstrap: one authenticated-by-token POST that
// writes the organization into the database. The operator who could read the
// printed URL is trusted to run it, exactly once.
//
// The token is checked first and spent last. Everything the form can get
// wrong is refused before it is spent, so a typo costs a correction rather
// than a restart; and a write that fails after it was spent gives it back.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Setup-Token")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "missing setup token")
		return
	}
	hash := store.HashSecret(token)
	ok, err := s.store.SetupTokenUsable(hash)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusForbidden, "unknown or already-used setup token")
		return
	}

	org, models, msg := s.validateSetup(r)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	// Spending is the lock: of two submissions racing, one gets the token.
	if ok, err = s.store.SpendSetupToken(hash); err != nil || !ok {
		writeErr(w, http.StatusForbidden, "unknown or already-used setup token")
		return
	}
	if err := s.writeSetup(org, models); err != nil {
		s.store.ReleaseSetupToken(hash)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// validateSetup reads the form into the organization and models it would
// write, or says what is wrong with it. It writes nothing.
func (s *Server) validateSetup(r *http.Request) (*store.Org, []*store.Model, string) {
	var p setupPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		return nil, nil, "bad payload: " + err.Error()
	}
	p.OrgName = strings.TrimSpace(p.OrgName)
	p.AdminEmail = strings.TrimSpace(p.AdminEmail)
	p.Issuer = strings.TrimSpace(p.Issuer)
	p.ClientID = strings.TrimSpace(p.ClientID)
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"organization name", p.OrgName}, {"OIDC issuer", p.Issuer},
		{"client ID", p.ClientID}, {"administrator email", p.AdminEmail},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, nil, "missing " + strings.Join(missing, ", ")
	}
	if !strings.Contains(p.AdminEmail, "@") {
		return nil, nil, "the administrator email is not an email address: " + p.AdminEmail
	}

	// An organization starts closed to everyone outside the administrator's
	// domain rather than open to whoever the provider vouches for: the
	// provider may vouch for the whole internet, and nobody has looked yet.
	domains, err := auth.NormalizeDomains(p.AllowedDomains)
	if err != nil {
		return nil, nil, "allowed domains: " + err.Error()
	}
	if len(domains) == 0 {
		if domains, err = auth.NormalizeDomains([]string{auth.DomainOf(p.AdminEmail)}); err != nil {
			return nil, nil, "administrator email: " + err.Error()
		}
	}

	// The trace key is not optional. An organization without one records its
	// sessions in the clear, which is the one thing this system promises not
	// to do — so rather than allowing it, an empty field falls back to the key
	// minted at startup and printed to the operator's terminal.
	//
	// It is validated here because a typo would otherwise be silent: an
	// unparseable recipient means no trace writer, which looks exactly like a
	// backend that is working.
	traceKey := strings.TrimSpace(p.TraceKey)
	if strings.HasPrefix(strings.ToUpper(traceKey), "AGE-SECRET-KEY-") {
		return nil, nil, "that is the private trace key, which must never reach the backend. " +
			"Leave the field empty to use the key printed in the terminal, or paste a public key (age1…)"
	}
	if traceKey == "" {
		traceKey = s.setupTraceKey
	}
	if traceKey == "" {
		return nil, nil, "a trace key is required"
	}
	if _, err := age.ParseX25519Recipient(traceKey); err != nil {
		return nil, nil, "that is not an age public key (age1…): " + err.Error()
	}

	// A key with nowhere to write is the same problem in a different place,
	// and a directory this process cannot write is nowhere.
	tracePath := strings.Trim(strings.TrimSpace(p.TracePath), `"'`)
	if tracePath == "" {
		tracePath = filepath.Join(filepath.Dir(s.cfg.DBPath), "traces")
	}
	if abs, err := filepath.Abs(tracePath); err == nil {
		tracePath = abs
	}
	if err := writableDir(tracePath); err != nil {
		return nil, nil, "traces cannot be written to " + tracePath + ": " + err.Error()
	}

	var models []*store.Model
	for _, d := range p.Models {
		if d.Hosting != "" && d.Hosting != "internal" && d.Hosting != "external" {
			return nil, nil, "model " + d.Name + ": hosting must be internal or external"
		}
		models = append(models, &store.Model{
			Name: d.Name, Provider: d.Provider, Hosting: d.Hosting,
			Endpoint: d.Endpoint, ModelID: d.ModelID, ContextWindow: d.ContextWindow,
			CostIn: d.CostPerMTokIn, CostCachedIn: d.CostPerMTokCachedIn, CostOut: d.CostPerMTokOut,
			APIKey: d.APIKey,
		})
	}

	return &store.Org{
		Name:             p.OrgName,
		OIDCIssuer:       strings.TrimSuffix(p.Issuer, "/"),
		OIDCClientID:     p.ClientID,
		OIDCClientSecret: p.ClientSecret,
		SSOProvider:      providerLabel(p.Issuer),
		SSOEnforced:      true,
		AdminEmail:       p.AdminEmail,
		AllowedDomains:   domains,
		TraceKey:         traceKey,
		TracePath:        tracePath,
		// Set explicitly rather than left to the column default, so the value
		// an organization runs under is one somebody chose.
		MachineSessionMaxDays: store.DefaultMachineSessionMaxDays,
		SetupComplete:         true,
	}, models, ""
}

// writeSetup writes what validateSetup produced.
func (s *Server) writeSetup(org *store.Org, models []*store.Model) error {
	// The snapshot signing key is born here and never leaves the database;
	// clients pin its public half at login.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	org.SigningKey = base64.StdEncoding.EncodeToString(priv)
	for _, m := range models {
		if err := s.store.CreateModel(m); err != nil {
			return fmt.Errorf("model %s: %w", m.Name, err)
		}
	}
	// The organization last: it is what marks setup complete, so a failure
	// before it leaves a backend that can still be set up.
	return s.store.SaveOrg(org)
}

// writableDir creates dir if it is missing and proves a file can be written
// in it.
func writableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".john-setup-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// providerLabel guesses a display name for the console's sign-in card from
// the issuer host; purely cosmetic.
func providerLabel(issuer string) string {
	switch {
	case strings.Contains(issuer, "okta"):
		return "Okta"
	case strings.Contains(issuer, "microsoftonline") || strings.Contains(issuer, "entra"):
		return "Microsoft Entra ID"
	case strings.Contains(issuer, "google"):
		return "Google Workspace"
	case strings.Contains(issuer, "/devidp"):
		return "Developer IdP"
	default:
		return "Single sign-on"
	}
}
