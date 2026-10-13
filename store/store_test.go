package store

import (
	"path/filepath"
	"testing"
)

func TestOpenCreatesDatabaseAndMigrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "john.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if want := migrations[len(migrations)-1].Version; v != want {
		t.Fatalf("schema version = %d, want %d", v, want)
	}

	// The schema is actually there.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if err := s.db.QueryRow(`SELECT epoch FROM policy_epoch WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("query policy_epoch: %v", err)
	}
	if n != 1 {
		t.Fatalf("initial policy epoch = %d, want 1", n)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "john.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO users (id, email, created_at) VALUES ('u_1', 'a@b.c', '2026-08-21T00:00:00Z')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	var email string
	if err := s2.db.QueryRow(`SELECT email FROM users WHERE id = 'u_1'`).Scan(&email); err != nil {
		t.Fatalf("data lost across reopen: %v", err)
	}
	if email != "a@b.c" {
		t.Fatalf("email = %q", email)
	}

	// No migration ran twice.
	var count int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) {
		t.Fatalf("migrations recorded = %d, want %d", count, len(migrations))
	}
}

// The migration list is append-only and contiguous from 1. A gap or an edit
// to a shipped migration is how an operator's database quietly diverges from
// a fresh one, so the shape itself is under test.
func TestMigrationsAreContiguousFromOne(t *testing.T) {
	for i, m := range migrations {
		if m.Version != i+1 {
			t.Fatalf("migrations[%d].Version = %d, want %d", i, m.Version, i+1)
		}
		if m.Name == "" {
			t.Fatalf("migrations[%d] has no name", i)
		}
		if m.SQL == "" {
			t.Fatalf("migrations[%d] has no SQL", i)
		}
	}
}

// TestTracePubKeyRoundTrip: the backend stores the public half a client
// registers, and nothing else about the developer's key.
func TestTracePubKeyRoundTrip(t *testing.T) {
	s := openTestStore(t)
	u, err := s.UpsertSSOUser("sub-1", "dev@example.com", "Dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if u.TracePubKey != "" {
		t.Fatal("a new user starts with no registered trace key")
	}
	const recipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	if err := s.SetUserTracePubKey(u.ID, recipient); err != nil {
		t.Fatal(err)
	}
	again, err := s.UserByID(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.TracePubKey != recipient {
		t.Fatalf("trace key = %q", again.TracePubKey)
	}

	// Re-keying replaces it: sessions already written keep the recipients
	// they were written with, which is what a grant is for.
	if err := s.SetUserTracePubKey(u.ID, "age1replacement"); err != nil {
		t.Fatal(err)
	}
	again, _ = s.UserByID(u.ID)
	if again.TracePubKey != "age1replacement" {
		t.Fatalf("re-keyed trace key = %q", again.TracePubKey)
	}
}

// TestSessionListingAndReopen backs /resume: a developer sees their own
// conversations in a workspace, and can make an ended one active again.
func TestSessionListingAndReopen(t *testing.T) {
	s := openTestStore(t)
	mine, err := s.UpsertSSOUser("sub-1", "dev@example.com", "Dev", "")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := s.UpsertSSOUser("sub-2", "other@example.com", "Other", "")
	if err != nil {
		t.Fatal(err)
	}

	first, err := s.CreateSession(mine.ID, "wk1", "/src/a", "host", "m1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateSession(mine.ID, "wk1", "/src/a", "host", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(mine.ID, "wk2", "/src/b", "host", "m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(theirs.ID, "wk1", "/src/a", "host", "m1"); err != nil {
		t.Fatal(err)
	}

	got, err := s.SessionsForUser(mine.ID, "wk1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("sessions in this workspace = %d, want 2 (mine only)", len(got))
	}
	for _, row := range got {
		if row.UserID != mine.ID || row.WorkspaceKey != "wk1" {
			t.Fatalf("listing leaked %s of %s", row.ID, row.UserID)
		}
	}

	if err := s.EndSession(first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReopenSession(first.ID, mine.ID); err != nil {
		t.Fatal(err)
	}
	back, err := s.SessionByID(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Status != "active" || back.EndedAt != nil {
		t.Fatalf("reopened session = %s, ended %v", back.Status, back.EndedAt)
	}

	// Never somebody else's.
	if err := s.ReopenSession(second.ID, theirs.ID); err != ErrNotFound {
		t.Fatalf("reopening another developer's session = %v, want ErrNotFound", err)
	}

	all, err := s.AllSessions(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("AllSessions = %d, want 4", len(all))
	}
}
