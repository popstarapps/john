package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Org is the singleton organization row. Everything the backend is configured
// with lives here; there is no configuration file.
type Org struct {
	Name             string
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	SSOProvider      string
	SSOEnforced      bool
	AdminEmail       string

	// AllowedDomains are the email domains sign-in admits, lowercase, with
	// no @. Empty admits any domain the identity provider vouches for.
	AllowedDomains []string

	TraceKey   string // age public recipient
	TracePath  string
	SigningKey string // Ed25519 private key, base64 — signs policy snapshots

	// MachineSessionMaxDays caps how long a machine may keep refreshing its
	// credentials before the identity provider has to confirm the person
	// again. Zero turns the cap off, which is what standalone wants: there is
	// no identity provider to send anyone back to.
	MachineSessionMaxDays int

	// BuildNetwork is where an image build's network traffic goes, spelled as
	// policy/build.go spells it: direct or proxied. It rides in the
	// signed snapshot with the rest of the policy, because it is a claim about
	// what a machine may do and the machine must not be able to argue with it.
	// The empty string means direct, which is what the sandbox does with an
	// unset value and therefore the one answer that cannot surprise anyone.
	BuildNetwork string

	// Attachments is what a prompt may carry besides text, spelled as
	// policy/attachments.go spells it: guarded, on or off. It rides in the
	// signed snapshot for the same reason BuildNetwork does — it is a claim
	// about what a machine may send, and the machine must not be able to
	// argue with it. The empty string means guarded.
	Attachments string

	SetupComplete bool
	CreatedAt     time.Time
}

// DefaultMachineSessionMaxDays is the cap a new organization starts with: a
// quarter of refreshing, then the identity provider gets asked again.
const DefaultMachineSessionMaxDays = 90

// ErrNoOrg means setup has never completed: there is no organization yet.
var ErrNoOrg = errors.New("store: no organization configured")

// Org returns the singleton organization, or ErrNoOrg before setup.
func (s *Store) Org() (*Org, error) {
	var o Org
	var created string
	var ssoEnforced, setupComplete int
	var domains string
	err := s.db.QueryRow(`SELECT name, oidc_issuer, oidc_client_id, oidc_client_secret,
		sso_provider, sso_enforced, admin_email, trace_key, trace_path,
		snapshot_signing_key, machine_session_max_days, build_network, attachments,
		sso_allowed_domains, setup_complete, created_at
		FROM org WHERE id = 1`).Scan(
		&o.Name, &o.OIDCIssuer, &o.OIDCClientID, &o.OIDCClientSecret,
		&o.SSOProvider, &ssoEnforced, &o.AdminEmail, &o.TraceKey, &o.TracePath,
		&o.SigningKey, &o.MachineSessionMaxDays, &o.BuildNetwork, &o.Attachments,
		&domains, &setupComplete, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoOrg
	}
	if err != nil {
		return nil, err
	}
	o.SSOEnforced = ssoEnforced != 0
	if domains != "" {
		o.AllowedDomains = strings.Split(domains, ",")
	}
	o.SetupComplete = setupComplete != 0
	o.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &o, nil
}

// SaveOrg writes the singleton organization row, creating or replacing it.
func (s *Store) SaveOrg(o *Org) error {
	created := o.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO org (id, name, oidc_issuer, oidc_client_id, oidc_client_secret,
		sso_provider, sso_enforced, admin_email, trace_key, trace_path,
		snapshot_signing_key, machine_session_max_days, build_network, attachments,
		sso_allowed_domains, setup_complete, created_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name,
			oidc_issuer = excluded.oidc_issuer,
			oidc_client_id = excluded.oidc_client_id,
			oidc_client_secret = excluded.oidc_client_secret,
			sso_provider = excluded.sso_provider,
			sso_enforced = excluded.sso_enforced,
			admin_email = excluded.admin_email,
			trace_key = excluded.trace_key,
			trace_path = excluded.trace_path,
			snapshot_signing_key = excluded.snapshot_signing_key,
			machine_session_max_days = excluded.machine_session_max_days,
			build_network = excluded.build_network,
			attachments = excluded.attachments,
			sso_allowed_domains = excluded.sso_allowed_domains,
			setup_complete = excluded.setup_complete`,
		o.Name, o.OIDCIssuer, o.OIDCClientID, o.OIDCClientSecret,
		o.SSOProvider, boolInt(o.SSOEnforced), o.AdminEmail, o.TraceKey, o.TracePath,
		o.SigningKey, o.MachineSessionMaxDays, o.BuildNetwork, o.Attachments,
		strings.Join(o.AllowedDomains, ","), boolInt(o.SetupComplete),
		created.Format(time.RFC3339))
	return err
}

// CreateSetupToken records the hash of a one-time setup token.
func (s *Store) CreateSetupToken(tokenHash string) error {
	_, err := s.db.Exec(`INSERT INTO setup_tokens (token_hash, created_at) VALUES (?, ?)`,
		tokenHash, time.Now().UTC().Format(time.RFC3339))
	return err
}

// SetupTokenUsable reports whether a setup token exists and is unspent,
// without spending it.
func (s *Store) SetupTokenUsable(tokenHash string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM setup_tokens WHERE token_hash = ? AND used_at IS NULL`,
		tokenHash).Scan(&n)
	return n == 1, err
}

// ReleaseSetupToken makes a spent setup token usable again, for a setup that
// spent it and then failed to write.
func (s *Store) ReleaseSetupToken(tokenHash string) error {
	_, err := s.db.Exec(`UPDATE setup_tokens SET used_at = NULL WHERE token_hash = ?`, tokenHash)
	return err
}

// SpendSetupToken marks an unused setup token as used. It returns false when
// the token does not exist or was already spent — the two cases are
// indistinguishable on purpose.
func (s *Store) SpendSetupToken(tokenHash string) (bool, error) {
	res, err := s.db.Exec(`UPDATE setup_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), tokenHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
