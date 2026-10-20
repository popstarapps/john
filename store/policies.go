package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PolicyGroup bundles related policies; groups attach to teams, and baseline
// groups attach to every team automatically.
type PolicyGroup struct {
	ID          string
	Name        string
	Description string
	Baseline    bool
	Version     int
	UpdatedAt   time.Time
	UpdatedBy   string
	TeamIDs     []string // attached teams; for a baseline group, every team
}

// Policy is one deterministic control. Spec is the JSON discriminated union
// the console defines; Type always equals its "kind".
type Policy struct {
	ID          string
	GroupID     string
	Type        string
	Name        string
	Description string
	Enabled     bool
	Version     int
	UpdatedAt   time.Time
	UpdatedBy   string
	Spec        json.RawMessage
}

var policyTypes = map[string]bool{
	"network": true, "replacement": true, "model-routing": true, "workspace": true,
}

// --- groups ---

// CreatePolicyGroup inserts a group.
func (s *Store) CreatePolicyGroup(name, description string, baseline bool, actor string) (*PolicyGroup, error) {
	id := NewID("g")
	_, err := s.db.Exec(`INSERT INTO policy_groups (id, name, description, baseline, version, updated_at, updated_by)
		VALUES (?, ?, ?, ?, 1, ?, ?)`,
		id, name, description, boolInt(baseline), time.Now().UTC().Format(time.RFC3339), actor)
	if err != nil {
		return nil, err
	}
	// No epoch: a group is made empty, and an empty group changes nobody's
	// snapshot, models or routing. Moving it anyway told every session
	// "policies updated" when an approval made a group to put its policy in,
	// a line before the approval itself arrived.
	return s.PolicyGroupByID(id)
}

// PolicyGroupByID looks a group up, with its attached teams resolved.
func (s *Store) PolicyGroupByID(id string) (*PolicyGroup, error) {
	g, err := scanGroup(s.db.QueryRow(`SELECT id, name, description, baseline, version, updated_at, updated_by
		FROM policy_groups WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := s.fillGroupTeams(g); err != nil {
		return nil, err
	}
	return g, nil
}

// PolicyGroups lists every group, baseline first.
func (s *Store) PolicyGroups() ([]*PolicyGroup, error) {
	rows, err := s.db.Query(`SELECT id, name, description, baseline, version, updated_at, updated_by
		FROM policy_groups ORDER BY baseline DESC, name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PolicyGroup
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, g := range out {
		if err := s.fillGroupTeams(g); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GroupsForUser resolves every group that applies to a user: the baseline
// ones plus the ones attached to their teams.
func (s *Store) GroupsForUser(userID string) ([]*PolicyGroup, error) {
	all, err := s.PolicyGroups()
	if err != nil {
		return nil, err
	}
	teams, err := s.TeamIDsForUser(userID)
	if err != nil {
		return nil, err
	}
	member := map[string]bool{}
	for _, t := range teams {
		member[t] = true
	}
	var out []*PolicyGroup
	for _, g := range all {
		if g.Baseline {
			out = append(out, g)
			continue
		}
		for _, t := range g.TeamIDs {
			if member[t] {
				out = append(out, g)
				break
			}
		}
	}
	return out, nil
}

// --- policies ---

// CreatePolicy inserts a policy into a group and bumps the group's version.
func (s *Store) CreatePolicy(groupID, name, description string, spec json.RawMessage, actor string) (*Policy, error) {
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(spec, &kind); err != nil || !policyTypes[kind.Kind] {
		return nil, fmt.Errorf("store: policy spec needs a valid kind, got %q", kind.Kind)
	}
	if _, err := s.PolicyGroupByID(groupID); err != nil {
		return nil, fmt.Errorf("store: policy group %s: %w", groupID, err)
	}

	id := NewID("p")
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`INSERT INTO policies (id, group_id, type, name, description, enabled, version, spec, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, 1, 1, ?, ?, ?)`,
		id, groupID, kind.Kind, name, description, string(spec), now, actor)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE policy_groups SET version = version + 1, updated_at = ?, updated_by = ? WHERE id = ?`,
		now, actor, groupID); err != nil {
		return nil, err
	}
	s.BumpPolicyEpoch(policyChangeKind[kind.Kind])
	return s.PolicyByID(id)
}

// ErrStalePolicy means the policy changed since the caller read it: someone
// else saved first, and writing over them would lose their change unseen.
var ErrStalePolicy = errors.New("store: policy changed since it was read")

// UpdatePolicy rewrites a policy's name, description and spec in place. Its
// type is fixed — a spec of another kind is a different policy, not an edit to
// this one. When version is non-zero the write only lands on that version.
func (s *Store) UpdatePolicy(id, name, description string, spec json.RawMessage, version int, actor string) (*Policy, error) {
	cur, err := s.PolicyByID(id)
	if err != nil {
		return nil, err
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(spec, &kind); err != nil || kind.Kind != cur.Type {
		return nil, fmt.Errorf("store: a %s policy needs a %s spec, got %q", cur.Type, cur.Type, kind.Kind)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	q := `UPDATE policies SET name = ?, description = ?, spec = ?, version = version + 1, updated_at = ?, updated_by = ? WHERE id = ?`
	args := []any{name, description, string(spec), now, actor, id}
	if version != 0 {
		q += ` AND version = ?`
		args = append(args, version)
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrStalePolicy
	}
	if _, err := s.db.Exec(`UPDATE policy_groups SET version = version + 1, updated_at = ?, updated_by = ? WHERE id = ?`,
		now, actor, cur.GroupID); err != nil {
		return nil, err
	}
	s.BumpPolicyEpoch(policyChangeKind[cur.Type])
	return s.PolicyByID(id)
}

// DeletePolicy removes a policy for good. Requests it decided keep its id in
// their log, so they still say which rule let them through.
func (s *Store) DeletePolicy(id, actor string) (*Policy, error) {
	p, err := s.PolicyByID(id)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM policies WHERE id = ?`, id); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE policy_groups SET version = version + 1, updated_at = ?, updated_by = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), actor, p.GroupID); err != nil {
		return nil, err
	}
	s.BumpPolicyEpoch(policyChangeKind[p.Type])
	return p, nil
}

// DeletePolicyGroup removes a group, every policy in it, and its attachments
// to teams. A policy outside any group applies to nobody, so keeping them
// would only leave rules behind that nothing can reach.
func (s *Store) DeletePolicyGroup(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// What references the group goes first, as with a team.
	if _, err := tx.Exec(`DELETE FROM policies WHERE group_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM team_policy_groups WHERE group_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM policy_groups WHERE id = ?`, id)
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

// PolicyByID looks a policy up.
func (s *Store) PolicyByID(id string) (*Policy, error) {
	return scanPolicy(s.db.QueryRow(`SELECT `+policyColumns+` FROM policies WHERE id = ?`, id))
}

// Policies lists policies, optionally filtered by group and type.
func (s *Store) Policies(groupID, policyType string) ([]*Policy, error) {
	q := `SELECT ` + policyColumns + ` FROM policies WHERE 1=1`
	var args []any
	if groupID != "" {
		q += ` AND group_id = ?`
		args = append(args, groupID)
	}
	if policyType != "" {
		q += ` AND type = ?`
		args = append(args, policyType)
	}
	q += ` ORDER BY updated_at, rowid`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Policy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPolicyEnabled flips a policy and bumps its version.
func (s *Store) SetPolicyEnabled(id string, enabled bool, actor string) (*Policy, error) {
	res, err := s.db.Exec(`UPDATE policies SET enabled = ?, version = version + 1, updated_at = ?, updated_by = ? WHERE id = ?`,
		boolInt(enabled), time.Now().UTC().Format(time.RFC3339), actor, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	p, err := s.PolicyByID(id)
	if err != nil {
		s.BumpPolicyEpoch()
		return nil, err
	}
	s.BumpPolicyEpoch(policyChangeKind[p.Type])
	return p, nil
}

// PoliciesForUser flattens the effective policy set across a user's groups.
func (s *Store) PoliciesForUser(userID string) ([]*Policy, []*PolicyGroup, error) {
	groups, err := s.GroupsForUser(userID)
	if err != nil {
		return nil, nil, err
	}
	var policies []*Policy
	for _, g := range groups {
		ps, err := s.Policies(g.ID, "")
		if err != nil {
			return nil, nil, err
		}
		policies = append(policies, ps...)
	}
	return policies, groups, nil
}

// ModelsForUser resolves the models a user may start a session on: enabled
// assign-mode routing policies add models, and unconditional restrict-mode
// policies (no keywords) narrow to their set. Keyword restrictions do not
// apply here — they bite mid-session, when a keyword is seen.
func (s *Store) ModelsForUser(userID string) ([]*Model, error) {
	policies, _, err := s.PoliciesForUser(userID)
	if err != nil {
		return nil, err
	}
	type routingSpec struct {
		Kind     string   `json:"kind"`
		Mode     string   `json:"mode"`
		ModelIDs []string `json:"modelIds"`
		Keywords []string `json:"keywords"`
	}
	var routing []routingSpec
	for _, p := range policies {
		if p.Type != "model-routing" || !p.Enabled {
			continue
		}
		var spec routingSpec
		if err := json.Unmarshal(p.Spec, &spec); err != nil {
			continue
		}
		routing = append(routing, spec)
	}
	assigned := map[string]bool{}
	for _, spec := range routing {
		if spec.Mode == "assign" {
			for _, m := range spec.ModelIDs {
				assigned[m] = true
			}
		}
	}
	for _, spec := range routing {
		if spec.Mode == "restrict" && len(spec.Keywords) == 0 {
			allowed := map[string]bool{}
			for _, m := range spec.ModelIDs {
				allowed[m] = true
			}
			for m := range assigned {
				if !allowed[m] {
					delete(assigned, m)
				}
			}
			for _, m := range spec.ModelIDs {
				assigned[m] = true
			}
		}
	}
	all, err := s.Models()
	if err != nil {
		return nil, err
	}
	var out []*Model
	for _, m := range all {
		if assigned[m.ID] && m.Status == "enabled" {
			out = append(out, m)
		}
	}
	return out, nil
}

// PoliciesReferencingModel returns every model-routing policy that names a
// model, enabled or not. A dangling id in a restrict-mode policy is the
// dangerous one — it narrows a session to a model that no longer exists,
// which leaves the developer with nothing to run on — so a model is not
// deleted while anything still points at it.
func (s *Store) PoliciesReferencingModel(modelID string) ([]*Policy, error) {
	policies, err := s.Policies("", "model-routing")
	if err != nil {
		return nil, err
	}
	var out []*Policy
	for _, p := range policies {
		var spec struct {
			ModelIDs []string `json:"modelIds"`
		}
		if err := json.Unmarshal(p.Spec, &spec); err != nil {
			continue
		}
		for _, id := range spec.ModelIDs {
			if id == modelID {
				out = append(out, p)
				break
			}
		}
	}
	return out, nil
}

// --- policy epoch ---

// PolicyEpoch is the version of the whole policy configuration; snapshots
// carry it and clients refresh when it moves.
func (s *Store) PolicyEpoch() (uint64, error) {
	var e uint64
	err := s.db.QueryRow(`SELECT epoch FROM policy_epoch WHERE id = 1`).Scan(&e)
	return e, err
}

// What a change to the policy configuration was about, as BumpPolicyEpoch
// is told and a live session is told after it. A session that was only told
// "policies changed" said "policies updated" when an administrator added a
// model, and said it again under every access request it had just announced
// as approved.
const (
	ChangeNetwork      = "network"
	ChangeWorkspace    = "workspace"
	ChangeRouting      = "routing"
	ChangeReplacements = "replacements"
	ChangeModels       = "models"
	ChangeBuilds       = "builds"
	ChangePrompts      = "prompts"
	// ChangeGroups is who a group applies to, which can move any of the
	// others at once.
	ChangeGroups = "groups"
)

// policyChangeKind is what a change to a policy of a type is about.
var policyChangeKind = map[string]string{
	"network": ChangeNetwork, "workspace": ChangeWorkspace,
	"model-routing": ChangeRouting, "replacement": ChangeReplacements,
}

// BumpPolicyEpoch advances the epoch. Called by every mutation that can
// change what a snapshot would contain, with what it changed — one of the
// Change constants. A change that names nothing is any of them.
func (s *Store) BumpPolicyEpoch(what ...string) {
	s.db.Exec(`UPDATE policy_epoch SET epoch = epoch + 1 WHERE id = 1`)
	epoch, err := s.PolicyEpoch()
	if err != nil {
		return
	}
	s.mu.Lock()
	listeners := append([]func(uint64, []string){}, s.policyListeners...)
	s.mu.Unlock()
	// Each on its own goroutine: a listener sends on a network connection,
	// and an administrator approving a request must not wait on a developer's
	// laptop to read a frame.
	for _, f := range listeners {
		go f(epoch, what)
	}
}

// OnPolicyChange registers f to be called after every change that moves the
// policy epoch, with the new one and what the change was about.
//
// This is the push half of policy distribution. The pull half — a client
// refreshing at 60% of its snapshot's life — is what makes the system correct;
// this is what makes it quick, and quick is the difference between an approved
// access request that works now and one that works in a quarter of an hour,
// which is the whole experience of asking for access.
func (s *Store) OnPolicyChange(f func(epoch uint64, what []string)) {
	s.mu.Lock()
	s.policyListeners = append(s.policyListeners, f)
	s.mu.Unlock()
}

// --- scanning ---

const policyColumns = `id, group_id, type, name, description, enabled, version, spec, updated_at, updated_by`

func scanGroup(row interface{ Scan(...any) error }) (*PolicyGroup, error) {
	var g PolicyGroup
	var baseline int
	var updated string
	err := row.Scan(&g.ID, &g.Name, &g.Description, &baseline, &g.Version, &updated, &g.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.Baseline = baseline != 0
	g.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &g, nil
}

func (s *Store) fillGroupTeams(g *PolicyGroup) error {
	var err error
	if g.Baseline {
		g.TeamIDs, err = s.stringColumn(`SELECT id FROM teams ORDER BY id`)
	} else {
		g.TeamIDs, err = s.stringColumn(`SELECT team_id FROM team_policy_groups WHERE group_id = ? ORDER BY team_id`, g.ID)
	}
	return err
}

func scanPolicy(row interface{ Scan(...any) error }) (*Policy, error) {
	var p Policy
	var enabled int
	var spec, updated string
	err := row.Scan(&p.ID, &p.GroupID, &p.Type, &p.Name, &p.Description, &enabled, &p.Version, &spec, &updated, &p.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.Spec = json.RawMessage(spec)
	p.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &p, nil
}
