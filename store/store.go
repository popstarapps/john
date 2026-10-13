// Package store owns the backend's SQLite database: opening it, creating it
// when it does not exist, and bringing its schema to the current version.
//
// There is no configuration file anywhere in john; everything the backend
// knows lives in this database. Migrations are defined in code (migrations.go)
// and applied automatically on open, so operating the backend is: point it at
// a path, start it.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the open database. All queries in the backend go through it.
type Store struct {
	db *sql.DB

	mu sync.Mutex
	// policyListeners are called after every change that moves the policy
	// epoch. It is how a live session learns that an administrator decided
	// something, rather than finding out when its snapshot happens to expire.
	policyListeners []func(epoch uint64, what []string)
}

// Open opens the SQLite database at path, creating the file and its parent
// directories when they do not exist, and applies any pending migrations.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", dir, err)
		}
	}

	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite has one writer; a single connection sidesteps SQLITE_BUSY
	// entirely at the cost of serializing reads, which this backend — a
	// single vertically-scaled process — can afford.
	db.SetMaxOpenConns(1)
	db.SetConnMaxIdleTime(0)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for packages that own their own queries.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create migrations table: %w", err)
	}

	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d %s: %w", m.Version, m.Name, err)
		}
		if _, err := tx.Exec(`INSERT INTO migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.Version, m.Name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit migration %d: %w", m.Version, err)
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&v)
	return v, err
}
