package store

import (
	"database/sql"
	"strings"
	"time"
)

// Change is one entry in the append-only record of what happened to the
// controls. Nothing edits or deletes one, including the code that writes them
// — there is deliberately no update or delete statement in this file.
type Change struct {
	ID         string
	At         time.Time
	ActorID    string
	Entity     string // policy | group | team | member | model | request | settings
	EntityID   string
	EntityName string
	Action     string // created | updated | enabled | disabled | deleted | attached | detached | approved | rejected
	Version    *int
	Summary    string
}

// RecordChange appends one entry.
func (s *Store) RecordChange(actorID, entity, action, entityID, entityName, summary string, version *int) error {
	var v sql.NullInt64
	if version != nil {
		v = sql.NullInt64{Int64: int64(*version), Valid: true}
	}
	_, err := s.db.Exec(`INSERT INTO changes (id, at, actor_id, entity, entity_id, entity_name, action, version, summary)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		NewID("ch"), time.Now().UTC().Format(time.RFC3339Nano), actorID, entity, entityID, entityName, action, v, summary)
	return err
}

// Changes lists entries, newest first, optionally filtered.
func (s *Store) Changes(entity, search string, limit int) ([]*Change, error) {
	q := `SELECT id, at, actor_id, entity, entity_id, entity_name, action, version, summary FROM changes WHERE 1=1`
	var args []any
	if entity != "" {
		q += ` AND entity = ?`
		args = append(args, entity)
	}
	if search != "" {
		q += ` AND lower(entity_name || summary || action) LIKE ?`
		args = append(args, "%"+strings.ToLower(search)+"%")
	}
	// rowid and not id: see NewID. Two changes recorded in one clock tick share
	// an `at`, and only insertion order puts the later one first.
	q += ` ORDER BY at DESC, rowid DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Change
	for rows.Next() {
		var c Change
		var at string
		var v sql.NullInt64
		if err := rows.Scan(&c.ID, &at, &c.ActorID, &c.Entity, &c.EntityID, &c.EntityName, &c.Action, &v, &c.Summary); err != nil {
			return nil, err
		}
		c.At, _ = time.Parse(time.RFC3339Nano, at)
		if v.Valid {
			n := int(v.Int64)
			c.Version = &n
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}
