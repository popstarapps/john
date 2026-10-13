package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is one conversation between a developer and the agent, tied to a
// workspace on a machine. State is agentkit's serialized session, written at
// message boundaries: a backend restart loses the turn in flight, never the
// conversation.
type Session struct {
	ID             string
	UserID         string
	WorkspaceKey   string
	WorkspacePath  string
	Hostname       string
	ModelID        string // models table row id
	Narrowed       bool
	NarrowedModels string // JSON list of model row ids; empty = not narrowed
	Status         string // active | ended
	StartedAt      time.Time
	EndedAt        *time.Time
	Turns          int
	TokensIn       int
	TokensOut      int
	CostUSD        float64
	State          string

	// TurnID is the turn this session is in the middle of, or empty when it
	// is idle. It is written when a turn starts and cleared when it ends, so
	// a backend that comes back up can tell a conversation that was waiting
	// for a model from one that was waiting for a person.
	TurnID string
	// LastTurn is the highest turn number the session has started.
	LastTurn int
}

// FindActiveSession returns the live session for a user's workspace, or
// ErrNotFound. One workspace, one conversation.
func (s *Store) FindActiveSession(userID, workspaceKey string) (*Session, error) {
	return scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions
		WHERE user_id = ? AND workspace_key = ? AND status = 'active'
		ORDER BY started_at DESC, rowid DESC LIMIT 1`, userID, workspaceKey))
}

// SessionByID looks a session up.
func (s *Store) SessionByID(id string) (*Session, error) {
	return scanSession(s.db.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))
}

// SessionsForUser lists a developer's sessions in a workspace, newest first.
// It is what `/resume` offers: the conversations this machine could pick back
// up, whether they were left active or ended.
func (s *Store) SessionsForUser(userID, workspaceKey string, limit int) ([]*Session, error) {
	if limit <= 0 {
		limit = 20
	}
	// rowid and not id, for the reason at NewID.
	rows, err := s.db.Query(`SELECT `+sessionColumns+` FROM sessions
		WHERE user_id = ? AND workspace_key = ?
		ORDER BY started_at DESC, rowid DESC LIMIT ?`, userID, workspaceKey, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		sn, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// AllSessions lists every session, newest first. It backs the administrator's
// trace browser: the rows say who and when, and what was said stays in the
// sealed file.
func (s *Store) AllSessions(limit int) ([]*Session, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT `+sessionColumns+` FROM sessions
		ORDER BY started_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Session
	for rows.Next() {
		sn, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// ReopenSession makes an ended conversation active again, so `/resume` can
// carry on where it stopped. It never reopens someone else's.
func (s *Store) ReopenSession(id, userID string) error {
	res, err := s.db.Exec(`UPDATE sessions SET status = 'active', ended_at = NULL
		WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateSession starts a conversation.
func (s *Store) CreateSession(userID, workspaceKey, workspacePath, hostname, modelID string) (*Session, error) {
	id := NewID("s")
	_, err := s.db.Exec(`INSERT INTO sessions
		(id, user_id, workspace_key, workspace_path, hostname, model_id, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?, 'active', ?)`,
		id, userID, workspaceKey, workspacePath, hostname, modelID,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return s.SessionByID(id)
}

// SaveSessionState persists the serialized agent state and the current model.
func (s *Store) SaveSessionState(id, state, modelID string) error {
	_, err := s.db.Exec(`UPDATE sessions SET state = ?, model_id = ? WHERE id = ?`, state, modelID, id)
	return err
}

// SetSessionTurn records the turn a session is running, or clears it with an
// empty id. It is one statement on purpose: it runs at the start and end of
// every turn, and the conversation state beside it is already being written
// at every message boundary.
//
// A turn id of the client's own form, tN, also moves last_turn on to N.
func (s *Store) SetSessionTurn(id, turnID string) error {
	n := 0
	fmt.Sscanf(turnID, "t%d", &n)
	_, err := s.db.Exec(`UPDATE sessions SET turn_id = ?, last_turn = MAX(last_turn, ?) WHERE id = ?`,
		turnID, n, id)
	return err
}

// AddSessionUsage folds one turn's numbers into the session row.
func (s *Store) AddSessionUsage(id string, turns, tokensIn, tokensOut int, costUSD float64) error {
	_, err := s.db.Exec(`UPDATE sessions SET turns = turns + ?, tokens_in = tokens_in + ?,
		tokens_out = tokens_out + ?, cost_usd = cost_usd + ? WHERE id = ?`,
		turns, tokensIn, tokensOut, costUSD, id)
	return err
}

// SetSessionNarrowed pins the session's narrowing. It never widens: once
// narrowed models are recorded, later writes may only narrow further.
func (s *Store) SetSessionNarrowed(id, narrowedModels string) error {
	_, err := s.db.Exec(`UPDATE sessions SET narrowed = 1, narrowed_models = ? WHERE id = ?`,
		narrowedModels, id)
	return err
}

// EndSession closes a conversation.
func (s *Store) EndSession(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET status = 'ended', ended_at = ? WHERE id = ? AND status = 'active'`,
		time.Now().UTC().Format(time.RFC3339), id)
	return err
}

const sessionColumns = `id, user_id, workspace_key, workspace_path, hostname, model_id,
	narrowed, narrowed_models, status, started_at, ended_at, turns, tokens_in, tokens_out, cost_usd,
	state, turn_id, last_turn`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var sn Session
	var narrowed int
	var started string
	var ended sql.NullString
	err := row.Scan(&sn.ID, &sn.UserID, &sn.WorkspaceKey, &sn.WorkspacePath, &sn.Hostname, &sn.ModelID,
		&narrowed, &sn.NarrowedModels, &sn.Status, &started, &ended,
		&sn.Turns, &sn.TokensIn, &sn.TokensOut, &sn.CostUSD, &sn.State, &sn.TurnID, &sn.LastTurn)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sn.Narrowed = narrowed != 0
	sn.StartedAt, _ = time.Parse(time.RFC3339, started)
	if ended.Valid {
		t, _ := time.Parse(time.RFC3339, ended.String)
		sn.EndedAt = &t
	}
	return &sn, nil
}
