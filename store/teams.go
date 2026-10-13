package store

import (
	"database/sql"
	"errors"
	"time"
)

// Team is a group of people policy groups attach to.
type Team struct {
	ID          string
	Name        string
	Slug        string
	Description string
	CreatedAt   time.Time
	MemberIDs   []string
	GroupIDs    []string // attached, non-baseline
}

// CreateTeam inserts a team and attaches the given policy groups.
func (s *Store) CreateTeam(name, slug, description string, groupIDs []string) (*Team, error) {
	id := NewID("t")
	_, err := s.db.Exec(`INSERT INTO teams (id, name, slug, description, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, name, slug, description, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	for _, g := range groupIDs {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO team_policy_groups (team_id, group_id) VALUES (?, ?)`, id, g); err != nil {
			return nil, err
		}
	}
	s.BumpPolicyEpoch(ChangeGroups)
	return s.TeamByIDOrSlug(id)
}

// TeamByIDOrSlug finds a team either way, as the console's URLs do.
func (s *Store) TeamByIDOrSlug(idOrSlug string) (*Team, error) {
	row := s.db.QueryRow(`SELECT id, name, slug, description, created_at FROM teams WHERE id = ? OR slug = ?`,
		idOrSlug, idOrSlug)
	t, err := scanTeam(row)
	if err != nil {
		return nil, err
	}
	if err := s.fillTeam(t); err != nil {
		return nil, err
	}
	return t, nil
}

// Teams lists every team with members and attached groups.
func (s *Store) Teams() ([]*Team, error) {
	rows, err := s.db.Query(`SELECT id, name, slug, description, created_at FROM teams ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Team
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range out {
		if err := s.fillTeam(t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteTeam removes a team along with its membership and the groups
// attached to it.
//
// Deleting is not always the safe direction: the policies a team carried stop
// applying to the people who were in it, and while that narrows what the
// network proxy will allow, it also lifts whatever workspace protection those
// groups held. Warning about that is the caller's job, not this one's — read
// the team with TeamByIDOrSlug first and it says who is in it.
//
// What the team was is not erased: access requests keep their team id, and
// the change history keeps the name it had.
func (s *Store) DeleteTeam(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The join rows go first: they reference the team, and nothing may point
	// at a row that is on its way out.
	if _, err := tx.Exec(`DELETE FROM team_members WHERE team_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM team_policy_groups WHERE team_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM teams WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.BumpPolicyEpoch(ChangeGroups)
	return nil
}

// SetUserTeams replaces a user's team membership.
func (s *Store) SetUserTeams(userID string, teamIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM team_members WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, t := range teamIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO team_members (team_id, user_id) VALUES (?, ?)`, t, userID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.BumpPolicyEpoch(ChangeGroups)
	return nil
}

// SetTeamGroups replaces a team's attached (non-baseline) policy groups.
func (s *Store) SetTeamGroups(teamID string, groupIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM team_policy_groups WHERE team_id = ?`, teamID); err != nil {
		return err
	}
	for _, g := range groupIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO team_policy_groups (team_id, group_id) VALUES (?, ?)`, teamID, g); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.BumpPolicyEpoch(ChangeGroups)
	return nil
}

// TeamIDsForUser returns the ids of the teams a user belongs to.
func (s *Store) TeamIDsForUser(userID string) ([]string, error) {
	return s.stringColumn(`SELECT team_id FROM team_members WHERE user_id = ? ORDER BY team_id`, userID)
}

func scanTeam(row interface{ Scan(...any) error }) (*Team, error) {
	var t Team
	var created string
	err := row.Scan(&t.ID, &t.Name, &t.Slug, &t.Description, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &t, nil
}

func (s *Store) fillTeam(t *Team) error {
	var err error
	t.MemberIDs, err = s.stringColumn(`SELECT user_id FROM team_members WHERE team_id = ? ORDER BY user_id`, t.ID)
	if err != nil {
		return err
	}
	t.GroupIDs, err = s.stringColumn(`SELECT group_id FROM team_policy_groups WHERE team_id = ? ORDER BY group_id`, t.ID)
	return err
}

func (s *Store) stringColumn(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
