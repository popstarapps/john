package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// User is a person the identity provider has signed in.
type User struct {
	ID           string
	Email        string
	Name         string
	Initials     string
	Title        string
	Role         string // admin | member
	Status       string // active | suspended
	SSOSubject   string
	CreatedAt    time.Time
	LastActiveAt time.Time // zero when never

	// TracePubKey is the age recipient this user's sessions are wrapped for,
	// registered by their client. Empty means their sessions are readable by
	// the organization key alone until an admin grants them.
	TracePubKey string
}

// ErrNotFound is returned by lookups that matched nothing.
var ErrNotFound = errors.New("store: not found")

// NewID returns a random identifier with the given prefix, e.g. "u_9f2ca…".
// NewID makes an opaque identifier.
//
// It is random and carries no time, so a list of ids is not a chronology and
// must never be used as one. Every query here that wants newest-first sorts on
// a timestamp and breaks ties on rowid — insertion order — rather than on id,
// because a tie broken on a random string is a random order.
//
// Ties are not the rare event they look like. The timestamps are written from
// time.Now(), which resolves to nanoseconds on Linux and macOS but only to the
// system clock tick on Windows — a millisecond or worse — so two rows written
// one after the other routinely carry the identical timestamp there. That is
// how this was found: a store test that had always passed failed two runs in
// five on Windows.
func NewID(prefix string) string {
	var b [8]byte
	rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

const userColumns = `id, email, name, initials, title, role, status, sso_subject, created_at, last_active_at, trace_pubkey`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created string
	var lastActive sql.NullString
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Initials, &u.Title, &u.Role,
		&u.Status, &u.SSOSubject, &created, &lastActive, &u.TracePubKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if lastActive.Valid {
		u.LastActiveAt, _ = time.Parse(time.RFC3339, lastActive.String)
	}
	return &u, nil
}

// UserByID looks a user up by id.
func (s *Store) UserByID(id string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// UserByEmail looks a user up by email.
func (s *Store) UserByEmail(email string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE email = ?`, email))
}

// UserBySSOSubject looks a user up by the identity provider's subject.
func (s *Store) UserBySSOSubject(sub string) (*User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE sso_subject = ?`, sub))
}

// Users lists everyone, newest last.
func (s *Store) Users() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpsertSSOUser records a sign-in from the identity provider: it finds the
// user by subject (then by email, for the first sign-in after provisioning),
// creates them when new, and refreshes name and activity. The first user
// whose email matches adminEmail becomes an administrator — that is the whole
// of the bootstrap trust chain.
func (s *Store) UpsertSSOUser(subject, email, name, adminEmail string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, fmt.Errorf("store: sign-in with no email")
	}
	now := time.Now().UTC().Format(time.RFC3339)

	u, err := s.UserBySSOSubject(subject)
	if errors.Is(err, ErrNotFound) {
		u, err = s.UserByEmail(email)
	}
	if errors.Is(err, ErrNotFound) {
		role := "member"
		if adminEmail != "" && strings.EqualFold(email, adminEmail) {
			role = "admin"
		}
		id := NewID("u")
		_, err = s.db.Exec(`INSERT INTO users (id, email, name, initials, title, role, status, sso_subject, created_at, last_active_at)
			VALUES (?, ?, ?, ?, '', ?, 'active', ?, ?, ?)`,
			id, email, name, initialsOf(name, email), role, subject, now, now)
		if err != nil {
			return nil, err
		}
		return s.UserByID(id)
	}
	if err != nil {
		return nil, err
	}

	_, err = s.db.Exec(`UPDATE users SET sso_subject = ?, name = CASE WHEN ? != '' THEN ? ELSE name END,
		initials = CASE WHEN ? != '' THEN ? ELSE initials END, last_active_at = ? WHERE id = ?`,
		subject, name, name, name, initialsOf(name, email), now, u.ID)
	if err != nil {
		return nil, err
	}
	return s.UserByID(u.ID)
}

// SetUserTracePubKey records the age recipient a developer's client
// generated. It replaces whatever was there: a client re-keys when its
// identity file is gone, and from that moment new sessions are wrapped for
// the new key. Sessions already written keep the recipients they were written
// with — an admin grants access to those, which is the one recovery path.
func (s *Store) SetUserTracePubKey(id, recipient string) error {
	_, err := s.db.Exec(`UPDATE users SET trace_pubkey = ? WHERE id = ?`, recipient, id)
	return err
}

// SetUserRole moves a person between member and administrator. The caller
// decides who is allowed to ask; this decides only that the role is real.
func (s *Store) SetUserRole(id, role string) (*User, error) {
	if role != "admin" && role != "member" {
		return nil, fmt.Errorf("store: bad role %q", role)
	}
	res, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.UserByID(id)
}

// SetUserStatus suspends or reactivates a person.
//
// Suspending is how someone is offboarded: there is no delete, because a
// user id is written into sessions, usage rollups, requests and the change
// history, and removing the row would take the record of what they did with
// it. A suspended account resolves nowhere — not from a browser cookie, not
// from a machine token — and its machine credentials are revoked outright
// rather than left to expire on their own, so the machines they were signed
// in on stop at the next request instead of up to twelve hours later.
func (s *Store) SetUserStatus(id, status string) (*User, error) {
	if status != "active" && status != "suspended" {
		return nil, fmt.Errorf("store: bad status %q", status)
	}
	res, err := s.db.Exec(`UPDATE users SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if status == "suspended" {
		if err := s.RevokeMachineSessionsFor(id); err != nil {
			return nil, err
		}
		if err := s.RevokeWebSessionsFor(id); err != nil {
			return nil, err
		}
	}
	return s.UserByID(id)
}

// TouchUser refreshes last_active_at.
func (s *Store) TouchUser(id string) error {
	_, err := s.db.Exec(`UPDATE users SET last_active_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func initialsOf(name, email string) string {
	src := strings.TrimSpace(name)
	if src == "" {
		src = email
	}
	parts := strings.Fields(src)
	if len(parts) >= 2 {
		return strings.ToUpper(firstRune(parts[0]) + firstRune(parts[len(parts)-1]))
	}
	if len(src) >= 2 {
		return strings.ToUpper(src[:2])
	}
	return strings.ToUpper(src)
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}
