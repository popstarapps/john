package store

import (
	"testing"
	"time"
)

// normalTTL is a machine credential with room to breathe: nothing here
// expires during a test unless the test makes it.
var normalTTL = MachineTTL{Access: time.Hour, Refresh: 24 * time.Hour, Grace: time.Minute}

func TestRotationReplacesBothHalves(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	first, err := s.StartMachineSession(u.ID, "laptop", normalTTL)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserForMachineToken(first.Access); err != nil || got.ID != u.ID {
		t.Fatalf("first access token = %v, %v", got, err)
	}

	second, who, err := s.RotateMachineSession(first.Refresh, "laptop", normalTTL)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if who.ID != u.ID {
		t.Fatalf("rotated for %s, want %s", who.ID, u.ID)
	}
	if second.Access == first.Access || second.Refresh == first.Refresh {
		t.Fatal("rotation handed back a secret it was given")
	}
	if second.ChainID != first.ChainID {
		t.Fatalf("chain changed on rotation: %s -> %s", first.ChainID, second.ChainID)
	}
	if got, err := s.UserForMachineToken(second.Access); err != nil || got.ID != u.ID {
		t.Fatalf("second access token = %v, %v", got, err)
	}

	// The old access token keeps working until its own expiry. Rotation
	// replaces credentials; it is not a revocation, and a request already in
	// flight when the refresh happened must not fail.
	if _, err := s.UserForMachineToken(first.Access); err != nil {
		t.Fatalf("previous access token stopped working immediately: %v", err)
	}
}

// A client that never heard the answer to its own rotation asks again with
// the token it still has. Inside the grace window that has to work, or a
// dropped response locks the machine out of an account it legitimately holds.
func TestARetriedRotationInsideTheGraceWindowSucceeds(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	first, _ := s.StartMachineSession(u.ID, "laptop", normalTTL)
	lost, _, err := s.RotateMachineSession(first.Refresh, "laptop", normalTTL)
	if err != nil {
		t.Fatal(err)
	}

	// The same token again, immediately: still inside Grace.
	second, _, err := s.RotateMachineSession(first.Refresh, "laptop", normalTTL)
	if err != nil {
		t.Fatalf("retry inside the grace window: %v", err)
	}
	if _, err := s.UserForMachineToken(second.Access); err != nil {
		t.Fatalf("retry's access token does not work: %v", err)
	}

	// The successor nobody received is revoked rather than left alive: one
	// rotation must not leave two usable refresh tokens behind.
	if _, _, err := s.RotateMachineSession(lost.Refresh, "laptop", normalTTL); err != ErrRefreshReused {
		t.Fatalf("abandoned successor = %v, want ErrRefreshReused", err)
	}
}

// The point of rotation: a copy of a refresh token, used after the real
// machine has moved past it, is unmistakable and takes the sign-in down.
func TestReuseAfterTheGraceWindowRevokesTheWholeChain(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	noGrace := normalTTL
	noGrace.Grace = 0

	first, _ := s.StartMachineSession(u.ID, "laptop", noGrace)
	second, _, err := s.RotateMachineSession(first.Refresh, "laptop", noGrace)
	if err != nil {
		t.Fatal(err)
	}

	// Someone else's copy of the token the real machine already spent.
	if _, _, err := s.RotateMachineSession(first.Refresh, "attacker", noGrace); err != ErrRefreshReused {
		t.Fatalf("reuse = %v, want ErrRefreshReused", err)
	}

	// Both of them are locked out, not just the thief: which copy is the real
	// machine is exactly what cannot be known here.
	if _, _, err := s.RotateMachineSession(second.Refresh, "laptop", noGrace); err != ErrRefreshUnknown {
		t.Fatalf("current refresh token after reuse = %v, want ErrRefreshUnknown", err)
	}
	if _, err := s.UserForMachineToken(second.Access); err != ErrNotFound {
		t.Fatalf("access token survived the chain revocation: %v", err)
	}
	if _, err := s.UserForMachineToken(first.Access); err != ErrNotFound {
		t.Fatalf("first access token survived the chain revocation: %v", err)
	}
}

func TestTheCapEndsAChainNoMatterHowRecentlyItRefreshed(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	// A chain that started 91 days ago under a 90-day cap, refreshed as
	// recently as a minute ago: still over.
	capped := normalTTL
	capped.MaxLife = 90 * 24 * time.Hour

	creds, _ := s.StartMachineSession(u.ID, "laptop", capped)
	backdateChain(t, s, creds.ChainID, 91*24*time.Hour)

	if _, _, err := s.RotateMachineSession(creds.Refresh, "laptop", capped); err != ErrRefreshCapped {
		t.Fatalf("over the cap = %v, want ErrRefreshCapped", err)
	}
	// Capped means done: the access token it was holding stops too.
	if _, err := s.UserForMachineToken(creds.Access); err != ErrNotFound {
		t.Fatalf("access token outlived the cap: %v", err)
	}
}

func TestNoCapMeansAChainKeepsGoing(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	// MaxLife zero — what standalone uses, having no identity provider to
	// send anyone back to.
	creds, _ := s.StartMachineSession(u.ID, "laptop", normalTTL)
	backdateChain(t, s, creds.ChainID, 5*365*24*time.Hour)

	if _, _, err := s.RotateMachineSession(creds.Refresh, "laptop", normalTTL); err != nil {
		t.Fatalf("uncapped chain refused after five years: %v", err)
	}
}

// A refresh token is never handed out with a life that runs past the cap: the
// machine is told to sign in again when the cap bites, not weeks later.
func TestARefreshTokenNeverOutlivesTheCap(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	ttl := MachineTTL{Access: time.Hour, Refresh: 30 * 24 * time.Hour, Grace: time.Minute,
		MaxLife: 2 * time.Hour}
	creds, err := s.StartMachineSession(u.ID, "laptop", ttl)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(creds.RefreshExpiresAt); d > 2*time.Hour+time.Minute {
		t.Fatalf("refresh token lives %v, past the %v cap", d, ttl.MaxLife)
	}
}

func TestASuspendedUserCannotRefresh(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	creds, _ := s.StartMachineSession(u.ID, "laptop", normalTTL)
	if _, err := s.db.Exec(`UPDATE users SET status = 'suspended' WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RotateMachineSession(creds.Refresh, "laptop", normalTTL); err != ErrRefreshUnknown {
		t.Fatalf("suspended refresh = %v, want ErrRefreshUnknown", err)
	}
	// And the credentials they were holding are gone, not merely unrenewable.
	if _, err := s.UserForMachineToken(creds.Access); err != ErrNotFound {
		t.Fatalf("suspended user's access token still resolves: %v", err)
	}
}

func TestRevokingAUsersSessionsStopsEveryMachine(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	laptop, _ := s.StartMachineSession(u.ID, "laptop", normalTTL)
	desktop, _ := s.StartMachineSession(u.ID, "desktop", normalTTL)
	other := mkUser(t, s, "other@example.com")
	theirs, _ := s.StartMachineSession(other.ID, "laptop", normalTTL)

	if err := s.RevokeMachineSessionsFor(u.ID); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*MachineCredentials{"laptop": laptop, "desktop": desktop} {
		if _, err := s.UserForMachineToken(c.Access); err != ErrNotFound {
			t.Fatalf("%s access token survived revocation: %v", name, err)
		}
		if _, _, err := s.RotateMachineSession(c.Refresh, name, normalTTL); err != ErrRefreshUnknown {
			t.Fatalf("%s refresh token survived revocation: %v", name, err)
		}
	}
	if _, err := s.UserForMachineToken(theirs.Access); err != nil {
		t.Fatalf("revoking one user's sessions hit another user: %v", err)
	}
}

func TestAnExpiredRefreshTokenIsJustUnknown(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	expired := MachineTTL{Access: time.Hour, Refresh: -time.Minute, Grace: time.Minute}
	creds, err := s.StartMachineSession(u.ID, "laptop", expired)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RotateMachineSession(creds.Refresh, "laptop", normalTTL); err != ErrRefreshUnknown {
		t.Fatalf("expired refresh = %v, want ErrRefreshUnknown", err)
	}
	if _, _, err := s.RotateMachineSession("never-existed", "laptop", normalTTL); err != ErrRefreshUnknown {
		t.Fatalf("unknown refresh = %v, want ErrRefreshUnknown", err)
	}
}

// backdateChain moves a chain's start (and its refresh token's issue time)
// into the past, so cap behaviour can be tested without waiting a quarter.
func backdateChain(t *testing.T, s *Store, chainID string, age time.Duration) {
	t.Helper()
	then := time.Now().UTC().Add(-age).Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE refresh_tokens SET chain_start = ?, created_at = ? WHERE chain_id = ?`,
		then, then, chainID); err != nil {
		t.Fatal(err)
	}
}
