package store

import (
	"database/sql"
	"errors"
	"time"
)

// A machine credential is two secrets, not one. The access token is what
// every request carries and it lives hours; the refresh token is shown to one
// endpoint and it lives weeks. Splitting them means the credential in flight
// on every call is the one worth least, and the one worth most is seen rarely
// enough that its use is a meaningful event.
//
// Rotation is what makes that event useful. Each refresh mints a new pair and
// supersedes the old refresh token, so a copy someone else took stops working
// the moment the real machine refreshes past it — and when the copy is used
// anyway, the reuse is visible and takes the whole chain down with it.

// MachineCredentials is the pair handed to a developer machine. The secrets
// are returned once, here, and only their hashes are stored.
type MachineCredentials struct {
	ChainID          string
	Access           string
	AccessExpiresAt  time.Time
	Refresh          string
	RefreshExpiresAt time.Time
}

// MachineTTL is how long each half of the pair lives.
type MachineTTL struct {
	Access  time.Duration
	Refresh time.Duration

	// Grace is how long a superseded refresh token keeps working, covering a
	// client that never received the answer to its own rotation.
	Grace time.Duration

	// MaxLife caps the whole chain, measured from the identity provider
	// sign-in it descends from. Zero means no cap: the machine may refresh
	// for as long as nobody revokes it.
	MaxLife time.Duration
}

var (
	// ErrRefreshUnknown covers every unusable refresh token that is not worth
	// distinguishing to a caller: never existed, expired, revoked. The client
	// response to all of them is the same — sign in again.
	ErrRefreshUnknown = errors.New("store: unknown or expired refresh token")

	// ErrRefreshReused means a token that had already been rotated came back
	// after its grace window. The chain is revoked by the time this returns.
	ErrRefreshReused = errors.New("store: refresh token reused after rotation")

	// ErrRefreshCapped means the chain reached the organization's maximum
	// machine session lifetime. The chain is revoked by the time this returns.
	ErrRefreshCapped = errors.New("store: machine session reached its maximum lifetime")
)

// StartMachineSession mints the first pair of a new chain, which is what a
// completed sign-in through the identity provider produces.
func (s *Store) StartMachineSession(userID, host string, ttl MachineTTL) (*MachineCredentials, error) {
	now := time.Now().UTC()
	chainID := NewID("mc")
	return s.mintPair(userID, host, chainID, "", now, now, ttl)
}

// RotateMachineSession spends a refresh token and returns the pair that
// replaces it. The presented token is superseded, not deleted: it stays
// usable for MachineTTL.Grace so a lost response is survivable, and its
// reuse after that is what catches a stolen copy.
func (s *Store) RotateMachineSession(refreshSecret, host string, ttl MachineTTL) (*MachineCredentials, *User, error) {
	now := time.Now().UTC()
	hash := HashSecret(refreshSecret)

	var (
		id, chainID, userID     string
		chainStart, expires     string
		supersededAt, revokedAt sql.NullString
	)
	err := s.db.QueryRow(`SELECT id, chain_id, user_id, chain_start, expires_at, superseded_at, revoked_at
		FROM refresh_tokens WHERE token_hash = ?`, hash).Scan(
		&id, &chainID, &userID, &chainStart, &expires, &supersededAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrRefreshUnknown
	}
	if err != nil {
		return nil, nil, err
	}
	if revokedAt.Valid {
		// A revoked token in a chain that is otherwise dead is the ordinary
		// case: an administrator revoked it, or the person was suspended, and
		// the machine still holds what it was given. Sign in again.
		//
		// A revoked token in a chain that is still alive is not ordinary. The
		// only token revoked that way is a successor the real client never
		// received — so whoever is presenting it read a response that was
		// meant for someone else, and the chain goes down.
		alive, err := s.chainHasLiveToken(chainID)
		if err != nil {
			return nil, nil, err
		}
		if alive {
			s.RevokeMachineChain(chainID)
			return nil, nil, ErrRefreshReused
		}
		return nil, nil, ErrRefreshUnknown
	}
	if exp, perr := time.Parse(time.RFC3339, expires); perr != nil || !now.Before(exp) {
		return nil, nil, ErrRefreshUnknown
	}

	// Already rotated once. Inside the grace window this is the same client
	// asking again because it never heard the answer, so the successor that
	// went nowhere is dropped and a fresh one takes its place. Outside it,
	// two copies of this token exist and neither can be trusted.
	if supersededAt.Valid {
		at, perr := time.Parse(time.RFC3339, supersededAt.String)
		if perr != nil || now.After(at.Add(ttl.Grace)) {
			s.RevokeMachineChain(chainID)
			return nil, nil, ErrRefreshReused
		}
		if _, err := s.db.Exec(`UPDATE refresh_tokens SET revoked_at = ?
			WHERE parent_id = ? AND revoked_at IS NULL`, now.Format(time.RFC3339), id); err != nil {
			return nil, nil, err
		}
	}

	start, perr := time.Parse(time.RFC3339, chainStart)
	if perr != nil {
		return nil, nil, ErrRefreshUnknown
	}
	if ttl.MaxLife > 0 && now.After(start.Add(ttl.MaxLife)) {
		s.RevokeMachineChain(chainID)
		return nil, nil, ErrRefreshCapped
	}

	user, err := s.UserByID(userID)
	if err != nil {
		return nil, nil, err
	}
	if user.Status != "active" {
		s.RevokeMachineChain(chainID)
		return nil, nil, ErrRefreshUnknown
	}

	creds, err := s.mintPair(userID, host, chainID, id, start, now, ttl)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.db.Exec(`UPDATE refresh_tokens SET superseded_at = ?, last_used_at = ? WHERE id = ?`,
		now.Format(time.RFC3339), now.Format(time.RFC3339), id); err != nil {
		return nil, nil, err
	}
	s.TouchUser(userID)
	return creds, user, nil
}

// chainHasLiveToken reports whether any refresh token in a chain is still
// usable, which is how a revoked-but-live-chain token is told apart from one
// whose whole sign-in was revoked.
func (s *Store) chainHasLiveToken(chainID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM refresh_tokens
		WHERE chain_id = ? AND revoked_at IS NULL`, chainID).Scan(&n)
	return n > 0, err
}

// mintPair writes one access token and one refresh token for a chain.
func (s *Store) mintPair(userID, host, chainID, parentID string, chainStart, now time.Time, ttl MachineTTL) (*MachineCredentials, error) {
	access := NewSecret()
	refresh := NewSecret()
	accessExp := now.Add(ttl.Access)
	refreshExp := now.Add(ttl.Refresh)

	// The chain may not outlive the cap, so neither may a refresh token in it:
	// a machine that would otherwise be told "expired" a month late is told
	// "sign in again" at the moment the cap actually bites.
	if ttl.MaxLife > 0 {
		if capAt := chainStart.Add(ttl.MaxLife); capAt.Before(refreshExp) {
			refreshExp = capAt
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`INSERT INTO machine_tokens (id, user_id, token_hash, host, created_at, expires_at, chain_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		NewID("mt"), userID, HashSecret(access), host,
		now.Format(time.RFC3339), accessExp.Format(time.RFC3339), chainID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO refresh_tokens
		(id, chain_id, parent_id, user_id, token_hash, host, created_at, chain_start, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		NewID("rt"), chainID, parentID, userID, HashSecret(refresh), host,
		now.Format(time.RFC3339), chainStart.Format(time.RFC3339), refreshExp.Format(time.RFC3339)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &MachineCredentials{
		ChainID:          chainID,
		Access:           access,
		AccessExpiresAt:  accessExp,
		Refresh:          refresh,
		RefreshExpiresAt: refreshExp,
	}, nil
}

// RevokeMachineChain kills a sign-in and everything it issued: the refresh
// tokens and the access tokens they minted, in one act, so revocation does
// not wait for a short expiry to catch up.
func (s *Store) RevokeMachineChain(chainID string) error {
	if chainID == "" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE refresh_tokens SET revoked_at = ?
		WHERE chain_id = ? AND revoked_at IS NULL`, now, chainID); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE machine_tokens SET revoked_at = ?
		WHERE chain_id = ? AND revoked_at IS NULL`, now, chainID)
	return err
}

// RevokeMachineSessionsFor kills every machine session a user holds. It is
// what suspending or offboarding someone should call.
func (s *Store) RevokeMachineSessionsFor(userID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE refresh_tokens SET revoked_at = ?
		WHERE user_id = ? AND revoked_at IS NULL`, now, userID); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE machine_tokens SET revoked_at = ?
		WHERE user_id = ? AND revoked_at IS NULL`, now, userID)
	return err
}
