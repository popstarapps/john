package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// NewSecret returns a fresh opaque credential: 32 random bytes, URL-safe
// base64. Only its hash is ever stored.
func NewSecret() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// HashSecret is the stored form of any credential: hex sha256. Constant-time
// comparison is unnecessary because lookups are by hash equality in SQL and
// the input space is 256 bits of entropy.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// MachineToken is what `john login` stores on a developer machine, by hash.
type MachineToken struct {
	ID        string
	UserID    string
	Host      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateMachineToken mints a machine token for a user and returns the secret,
// which is never stored and never seen again.
func (s *Store) CreateMachineToken(userID, host string, ttl time.Duration) (secret string, err error) {
	secret = NewSecret()
	now := time.Now().UTC()
	_, err = s.db.Exec(`INSERT INTO machine_tokens (id, user_id, token_hash, host, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		NewID("mt"), userID, HashSecret(secret), host,
		now.Format(time.RFC3339), now.Add(ttl).Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return secret, nil
}

// UserForMachineToken resolves a presented machine token to its user. Expired
// and revoked tokens do not resolve; a resolving token refreshes its
// last_used_at and the user's activity.
func (s *Store) UserForMachineToken(secret string) (*User, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var userID string
	err := s.db.QueryRow(`SELECT user_id FROM machine_tokens
		WHERE token_hash = ? AND revoked_at IS NULL AND expires_at > ?`,
		HashSecret(secret), now).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.db.Exec(`UPDATE machine_tokens SET last_used_at = ? WHERE token_hash = ?`, now, HashSecret(secret))
	s.TouchUser(userID)
	return s.UserByID(userID)
}

// CreateWebSession mints a browser session and returns its secret for the
// cookie.
func (s *Store) CreateWebSession(userID string, ttl time.Duration) (secret string, err error) {
	secret = NewSecret()
	now := time.Now().UTC()
	_, err = s.db.Exec(`INSERT INTO web_sessions (id, user_id, token_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		NewID("ws"), userID, HashSecret(secret),
		now.Format(time.RFC3339), now.Add(ttl).Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return secret, nil
}

// UserForWebSession resolves a session cookie to its user.
func (s *Store) UserForWebSession(secret string) (*User, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	var userID string
	err := s.db.QueryRow(`SELECT user_id FROM web_sessions WHERE token_hash = ? AND expires_at > ?`,
		HashSecret(secret), now).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.TouchUser(userID)
	return s.UserByID(userID)
}

// DeleteWebSession signs a browser out.
func (s *Store) DeleteWebSession(secret string) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE token_hash = ?`, HashSecret(secret))
	return err
}

// RevokeWebSessionsFor signs a person out of every browser at once, the way
// RevokeMachineSessionsFor does for their machines. Suspension uses both.
func (s *Store) RevokeWebSessionsFor(userID string) error {
	_, err := s.db.Exec(`DELETE FROM web_sessions WHERE user_id = ?`, userID)
	return err
}
