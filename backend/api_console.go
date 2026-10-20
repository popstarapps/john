package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/oliverkofoed/gokit/agentkit/llm"

	"github.com/popstarapps/john/auth"
	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/trace"
)

// requireAdmin authenticates and requires the administrator role. The console
// hides admin surfaces from members, but hiding is not access control — these
// answers are the control.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) *store.User {
	u := s.requireUser(w, r)
	if u == nil {
		return nil
	}
	if u.Role != "admin" {
		writeErr(w, http.StatusForbidden, "administrator role required")
		return nil
	}
	return u
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func tsPtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	v := ts(*t)
	return &v
}

// --- users ---

func (s *Server) userWithTeams(u *store.User) (userJSON, error) {
	teams, err := s.store.TeamIDsForUser(u.ID)
	if err != nil {
		return userJSON{}, err
	}
	return toUserJSON(u, teams), nil
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	users, err := s.store.Users()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	search := strings.ToLower(r.URL.Query().Get("search"))
	teamID := r.URL.Query().Get("teamId")
	out := []userJSON{}
	for _, u := range users {
		j, err := s.userWithTeams(u)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if teamID != "" && !contains(j.TeamIDs, teamID) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(u.Name+u.Email+u.Title), search) {
			continue
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	viewer := s.requireUser(w, r)
	if viewer == nil {
		return
	}
	id := r.PathValue("id")
	if viewer.ID != id && viewer.Role != "admin" {
		writeErr(w, http.StatusForbidden, "not your record")
		return
	}
	u, err := s.store.UserByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such user")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	j, err := s.userWithTeams(u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleSetUserTeams(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	id := r.PathValue("id")
	var req struct {
		TeamIDs []string `json:"teamIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	u, err := s.store.UserByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such user")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.SetUserTeams(id, req.TeamIDs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.RecordChange(admin.ID, "member", "updated", u.ID, u.Name,
		"Set "+u.Name+"'s teams to "+countOrNone(len(req.TeamIDs), "team"), nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// requireOtherUser is the shape every "act on a person" endpoint needs: an
// administrator, and a target who is somebody else.
//
// Refusing to act on yourself is not politeness. An administrator who
// suspends or demotes their own account is locked out of the console that
// would undo it, and the fix is a hand-edit of the database — so the one case
// nobody ever means is the one case that is refused outright.
//
// It is also the whole of the "do not strand the organization without an
// administrator" rule. An administrator acting here is necessarily active —
// a suspended one resolves to nobody — so a target who is an active
// administrator means there are at least two, and demoting or suspending them
// always leaves one behind. The only route to zero administrators is through
// yourself, and that is the route this closes. An administrator who wants to
// step down asks another one, or promotes someone first.
func (s *Server) requireOtherUser(w http.ResponseWriter, r *http.Request) (admin *store.User, target *store.User) {
	admin = s.requireAdmin(w, r)
	if admin == nil {
		return nil, nil
	}
	id := r.PathValue("id")
	if id == admin.ID {
		writeErr(w, http.StatusForbidden, "an administrator cannot change their own role or access")
		return nil, nil
	}
	target, err := s.store.UserByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such user")
		return nil, nil
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return nil, nil
	}
	return admin, target
}

func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	admin, target := s.requireOtherUser(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	u, err := s.store.SetUserRole(target.ID, req.Role)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such user")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	summary := u.Name + " is now a member."
	if u.Role == "admin" {
		summary = "Made " + u.Name + " an administrator."
	}
	s.store.RecordChange(admin.ID, "member", "updated", u.ID, u.Name, summary, nil)
	j, err := s.userWithTeams(u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleSetUserStatus(w http.ResponseWriter, r *http.Request) {
	admin, target := s.requireOtherUser(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	u, err := s.store.SetUserStatus(target.ID, req.Status)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such user")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	action, summary := "updated", "Reactivated "+u.Name+"."
	if u.Status == "suspended" {
		action = "disabled"
		summary = "Suspended " + u.Name + ". Their machines were signed out."
	}
	s.store.RecordChange(admin.ID, "member", action, u.ID, u.Name, summary, nil)
	j, err := s.userWithTeams(u)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) requireSelfOrAdmin(w http.ResponseWriter, r *http.Request) (viewer *store.User, id string) {
	viewer = s.requireUser(w, r)
	if viewer == nil {
		return nil, ""
	}
	id = r.PathValue("id")
	if viewer.ID != id && viewer.Role != "admin" {
		writeErr(w, http.StatusForbidden, "not your record")
		return nil, ""
	}
	return viewer, id
}

func (s *Server) handleUserGroups(w http.ResponseWriter, r *http.Request) {
	viewer, id := s.requireSelfOrAdmin(w, r)
	if viewer == nil {
		return
	}
	groups, err := s.store.GroupsForUser(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groupsJSON(groups))
}

func (s *Server) handleUserPolicies(w http.ResponseWriter, r *http.Request) {
	viewer, id := s.requireSelfOrAdmin(w, r)
	if viewer == nil {
		return
	}
	policies, groups, err := s.store.PoliciesForUser(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	groupByID := map[string]*store.PolicyGroup{}
	for _, g := range groups {
		groupByID[g.ID] = g
	}
	out := []map[string]any{}
	for _, p := range policies {
		j := policyJSON(p)
		j["group"] = groupJSON(groupByID[p.GroupID])
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUserModels(w http.ResponseWriter, r *http.Request) {
	viewer, id := s.requireSelfOrAdmin(w, r)
	if viewer == nil {
		return
	}
	models, err := s.store.ModelsForUser(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, modelsJSON(models))
}

// --- teams ---

func teamJSON(t *store.Team) map[string]any {
	return map[string]any{
		"id": t.ID, "name": t.Name, "slug": t.Slug, "description": t.Description,
		"memberIds": t.MemberIDs, "policyGroupIds": t.GroupIDs, "createdAt": ts(t.CreatedAt),
	}
}

func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	if s.requireUser(w, r) == nil {
		return
	}
	teams, err := s.store.Teams()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, t := range teams {
		out = append(out, teamJSON(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetTeam(w http.ResponseWriter, r *http.Request) {
	if s.requireUser(w, r) == nil {
		return
	}
	t, err := s.store.TeamByIDOrSlug(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such team")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, teamJSON(t))
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Name        string   `json:"name"`
		Slug        string   `json:"slug"`
		Description string   `json:"description"`
		GroupIDs    []string `json:"groupIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Slug == "" {
		writeErr(w, http.StatusBadRequest, "name and slug are required")
		return
	}
	t, err := s.store.CreateTeam(req.Name, req.Slug, req.Description, req.GroupIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.RecordChange(admin.ID, "team", "created", t.ID, t.Name,
		"Created team "+t.Name+" with "+countOrNone(len(req.GroupIDs), "attached group"), nil)
	writeJSON(w, http.StatusOK, teamJSON(t))
}

func (s *Server) handleSetTeamGroups(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	id := r.PathValue("id")
	var req struct {
		GroupIDs []string `json:"groupIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	t, err := s.store.TeamByIDOrSlug(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such team")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.SetTeamGroups(t.ID, req.GroupIDs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.RecordChange(admin.ID, "team", "attached", t.ID, t.Name,
		t.Name+" now carries "+countOrNone(len(req.GroupIDs), "attached policy group"), nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	t, err := s.store.TeamByIDOrSlug(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such team")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.DeleteTeam(t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Worth spelling out in the history: everyone who was in it lost the
	// groups it carried, which loosens workspace protection as readily as it
	// tightens network access.
	summary := "Deleted team " + t.Name + "."
	if len(t.MemberIDs) > 0 {
		summary = "Deleted team " + t.Name + ", which had " +
			countOrNone(len(t.MemberIDs), "member") + " Its groups no longer apply to them."
	}
	s.store.RecordChange(admin.ID, "team", "deleted", t.ID, t.Name, summary, nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- policy groups & policies ---

func groupJSON(g *store.PolicyGroup) map[string]any {
	if g == nil {
		return nil
	}
	return map[string]any{
		"id": g.ID, "name": g.Name, "description": g.Description, "baseline": g.Baseline,
		"version": g.Version, "updatedAt": ts(g.UpdatedAt), "updatedBy": g.UpdatedBy,
		"teamIds": g.TeamIDs,
	}
}

func groupsJSON(gs []*store.PolicyGroup) []map[string]any {
	out := []map[string]any{}
	for _, g := range gs {
		out = append(out, groupJSON(g))
	}
	return out
}

func policyJSON(p *store.Policy) map[string]any {
	return map[string]any{
		"id": p.ID, "groupId": p.GroupID, "type": p.Type, "name": p.Name,
		"description": p.Description, "enabled": p.Enabled, "version": p.Version,
		"updatedAt": ts(p.UpdatedAt), "updatedBy": p.UpdatedBy, "spec": p.Spec,
	}
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	groups, err := s.store.PolicyGroups()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groupsJSON(groups))
}

func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	g, err := s.store.PolicyGroupByID(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such group")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groupJSON(g))
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Baseline    bool   `json:"baseline"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	g, err := s.store.CreatePolicyGroup(req.Name, req.Description, req.Baseline, admin.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary := "Created policy group " + g.Name + "."
	if g.Baseline {
		summary = "Created baseline group " + g.Name + ", attached to every team."
	}
	s.store.RecordChange(admin.ID, "group", "created", g.ID, g.Name, summary, &g.Version)
	writeJSON(w, http.StatusOK, groupJSON(g))
}

func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	policies, err := s.store.Policies(r.URL.Query().Get("groupId"), r.URL.Query().Get("type"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, p := range policies {
		out = append(out, policyJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	p, err := s.store.PolicyByID(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such policy")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, policyJSON(p))
}

func (s *Server) handleCreatePolicy(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var req struct {
		GroupID     string          `json:"groupId"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Spec        json.RawMessage `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.GroupID == "" || len(req.Spec) == 0 {
		writeErr(w, http.StatusBadRequest, "groupId, name and spec are required")
		return
	}
	p, err := s.store.CreatePolicy(req.GroupID, req.Name, req.Description, req.Spec, admin.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	groupName := req.GroupID
	if g, err := s.store.PolicyGroupByID(p.GroupID); err == nil {
		groupName = g.Name
	}
	s.store.RecordChange(admin.ID, "policy", "created", p.ID, p.Name,
		"Added "+p.Type+" policy "+p.Name+" to "+groupName+".", &p.Version)
	writeJSON(w, http.StatusOK, policyJSON(p))
}

func (s *Server) handleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Spec        json.RawMessage `json:"spec"`
		Version     int             `json:"version"` // the one the editor started from
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || len(req.Spec) == 0 {
		writeErr(w, http.StatusBadRequest, "name and spec are required")
		return
	}
	p, err := s.store.UpdatePolicy(r.PathValue("id"), req.Name, req.Description, req.Spec, req.Version, admin.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such policy")
		return
	case errors.Is(err, store.ErrStalePolicy):
		writeErr(w, http.StatusConflict, "someone else changed this policy since you opened it — reopen it to see their change")
		return
	case err != nil:
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.store.RecordChange(admin.ID, "policy", "updated", p.ID, p.Name, "Edited "+p.Name+".", &p.Version)
	writeJSON(w, http.StatusOK, policyJSON(p))
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	p, err := s.store.DeletePolicy(r.PathValue("id"), admin.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such policy")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	groupName := p.GroupID
	if g, err := s.store.PolicyGroupByID(p.GroupID); err == nil {
		groupName = g.Name
	}
	s.store.RecordChange(admin.ID, "policy", "deleted", p.ID, p.Name,
		"Deleted "+p.Type+" policy "+p.Name+" from "+groupName+".", nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleSetPolicyEnabled(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	p, err := s.store.SetPolicyEnabled(r.PathValue("id"), req.Enabled, admin.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such policy")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	action, verb := "disabled", "Disabled "
	if req.Enabled {
		action, verb = "enabled", "Enabled "
	}
	s.store.RecordChange(admin.ID, "policy", action, p.ID, p.Name, verb+p.Name+".", &p.Version)
	writeJSON(w, http.StatusOK, policyJSON(p))
}

// --- models ---

func modelJSON(m *store.Model) map[string]any {
	return map[string]any{
		"id": m.ID, "name": m.Name, "provider": m.Provider, "hosting": m.Hosting,
		"endpoint": m.Endpoint, "modelId": m.ModelID,
		"contextWindow": m.ContextWindow, "status": m.Status,
		"costPerMTokIn": m.CostIn, "costPerMTokCachedIn": m.CostCachedIn, "costPerMTokOut": m.CostOut,
		"addedAt": ts(m.AddedAt),
	}
}

func modelsJSON(ms []*store.Model) []map[string]any {
	out := []map[string]any{}
	for _, m := range ms {
		out = append(out, modelJSON(m))
	}
	return out
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	models, err := s.store.Models()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, modelsJSON(models))
}

func (s *Server) handleCreateModel(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var d modelDraft
	// A model from the catalog is named by it; anything else needs a name.
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil || (d.Name == "" && d.ModelID == "") {
		writeErr(w, http.StatusBadRequest, "a model is required")
		return
	}
	m := &store.Model{
		Name: d.Name, Provider: d.Provider, Hosting: d.Hosting,
		Endpoint: d.Endpoint, ModelID: d.ModelID, ContextWindow: d.ContextWindow,
		CostIn: d.CostPerMTokIn, CostCachedIn: d.CostPerMTokCachedIn, CostOut: d.CostPerMTokOut,
		APIKey: d.APIKey,
	}
	if err := s.store.CreateModel(m); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	hosting := "external"
	if m.Hosting == "internal" {
		hosting = "internally hosted"
	}
	s.store.RecordChange(admin.ID, "model", "created", m.ID, m.Name,
		"Added "+hosting+" model "+m.Name+" ("+m.Provider+").", nil)
	s.store.BumpPolicyEpoch(store.ChangeModels)
	writeJSON(w, http.StatusOK, modelJSON(m))
}

func (s *Server) handleSetModelStatus(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	id := r.PathValue("id")
	if err := s.store.SetModelStatus(id, req.Status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "no such model")
		} else {
			writeErr(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	m, err := s.store.ModelByID(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	action, verb := "disabled", "Disabled "
	if req.Status == "enabled" {
		action, verb = "enabled", "Enabled "
	}
	s.store.RecordChange(admin.ID, "model", action, m.ID, m.Name, verb+"model "+m.Name+".", nil)
	s.store.BumpPolicyEpoch(store.ChangeModels)
	writeJSON(w, http.StatusOK, modelJSON(m))
}

// handleModelCatalog lists the models the console offers by name, from
// agentkit's catalog. It is served to anyone: it is what the providers publish
// themselves, and first-run setup needs it before there is anyone to sign in.
func (s *Server) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, m := range store.Catalog() {
		out = append(out, map[string]any{
			"provider": m.Provider, "id": m.ID, "name": m.Name,
			"contextWindow": m.ContextWindow, "maxOutput": m.MaxOutput,
			"costPerMTokIn": m.Cost.Input, "costPerMTokCachedIn": m.Cost.CacheRead, "costPerMTokOut": m.Cost.Output,
			"reasoning": m.Reasoning, "vision": m.Vision, "documents": m.Documents,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTestModel sends a model, as the console's dialog has it, one short
// prompt, so the console can say whether it answers before it is saved. An
// edit that leaves the key blank is tried with the stored one.
func (s *Server) handleTestModel(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	var d struct {
		modelDraft
		ID string `json:"id"` // the model being edited, if any
	}
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, http.StatusBadRequest, "bad model")
		return
	}
	key := d.APIKey
	if key == "" && d.ID != "" {
		if m, err := s.store.ModelByID(d.ID); err == nil {
			key = m.APIKey
		}
	}
	lm := store.BuildLLMModel(d.Provider, d.Endpoint, d.ModelID, d.Name, 0, 0, 0, d.ContextWindow)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	msg, err := s.streamer().Stream(ctx, llm.Request{
		Model:  lm,
		APIKey: key,
		// No MaxTokens: the test sends what a session sends, limits included,
		// so a model that would fail its first turn fails here instead.
		DisableCache: true,
		Messages:     []llm.Message{llm.UserText("Reply with the single word OK.")},
	}).Message()

	out := map[string]any{"ok": err == nil, "ms": time.Since(start).Milliseconds()}
	if err != nil {
		out["error"] = err.Error()
	} else {
		var reply strings.Builder
		for _, b := range msg.Blocks {
			if b.Type == llm.BlockText {
				reply.WriteString(b.Text)
			}
		}
		out["reply"] = strings.TrimSpace(reply.String())
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUpdateModel(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	id := r.PathValue("id")
	existing, err := s.store.ModelByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such model")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var d modelDraft
	// A model from the catalog is named by it; anything else needs a name.
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil || (d.Name == "" && d.ModelID == "") {
		writeErr(w, http.StatusBadRequest, "a model is required")
		return
	}
	m := &store.Model{
		ID: existing.ID, Name: d.Name, Provider: d.Provider, Hosting: d.Hosting,
		Endpoint: d.Endpoint, ModelID: d.ModelID, ContextWindow: d.ContextWindow,
		CostIn: d.CostPerMTokIn, CostCachedIn: d.CostPerMTokCachedIn, CostOut: d.CostPerMTokOut,
		APIKey: d.APIKey, // empty leaves the stored credential alone
	}
	if err := s.store.UpdateModel(m); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary := "Updated model " + m.Name + "."
	if d.APIKey != "" {
		summary = "Updated model " + m.Name + " and rotated its credential."
	}
	s.store.RecordChange(admin.ID, "model", "updated", m.ID, m.Name, summary, nil)
	s.store.BumpPolicyEpoch(store.ChangeModels)
	updated, err := s.store.ModelByID(m.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, modelJSON(updated))
}

func (s *Server) handleDeleteModel(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	id := r.PathValue("id")
	m, err := s.store.ModelByID(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such model")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A routing policy naming a model that is gone is worse than a model
	// nobody uses: in restrict mode it narrows a session to nothing at all.
	// So the policy is unpicked first, deliberately, by whoever wrote it.
	using, err := s.store.PoliciesReferencingModel(m.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(using) > 0 {
		names := make([]string, 0, len(using))
		for _, p := range using {
			names = append(names, p.Name)
		}
		noun := "policies"
		if len(using) == 1 {
			noun = "policy"
		}
		writeErr(w, http.StatusConflict,
			"still named by "+strconv.Itoa(len(using))+" model-routing "+noun+": "+
				strings.Join(names, ", ")+". Remove it there first, or disable the model instead.")
		return
	}
	if err := s.store.DeleteModel(m.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.store.RecordChange(admin.ID, "model", "deleted", m.ID, m.Name,
		"Deleted model "+m.Name+". Past usage keeps counting toward spend.", nil)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- change history ---

func (s *Server) handleListChanges(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		json.Unmarshal([]byte(v), &limit)
	}
	changes, err := s.store.Changes(q.Get("entity"), q.Get("search"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, c := range changes {
		out = append(out, map[string]any{
			"id": c.ID, "at": c.At.UTC().Format(time.RFC3339), "actorId": c.ActorID,
			"entity": c.Entity, "entityId": c.EntityID, "entityName": c.EntityName,
			"action": c.Action, "version": c.Version, "summary": c.Summary,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- org ---

func (s *Server) orgJSON(r *http.Request, org *store.Org) map[string]any {
	dbSize := int64(0)
	if fi, err := os.Stat(s.cfg.DBPath); err == nil {
		dbSize = fi.Size()
	}
	fingerprint := org.TraceKey
	if len(fingerprint) > 20 {
		fingerprint = fingerprint[:20] + "…"
	}
	traceSize := int64(0)
	if org.TracePath != "" {
		traceSize = trace.Size(org.TracePath)
	}
	return map[string]any{
		"name":       org.Name,
		"standalone": s.cfg.Standalone,
		"sso": map[string]any{
			"provider": org.SSOProvider, "issuer": org.OIDCIssuer, "clientId": org.OIDCClientID,
			"enforced": org.SSOEnforced, "allowedDomains": append([]string{}, org.AllowedDomains...),
		},
		"traceKey": map[string]any{
			"fingerprint": fingerprint, "algorithm": "age (X25519)",
			"createdAt": ts(org.CreatedAt), "custodian": org.AdminEmail,
		},
		"traceStore": map[string]any{"location": org.TracePath},
		"machineSessions": map[string]any{
			"maxDays": org.MachineSessionMaxDays,
		},
		// Answered as the setting in force rather than as the column, so the
		// console shows what builds actually do rather than an empty field
		// that happens to mean direct.
		"builds": map[string]any{
			"network": (&policy.Snapshot{Build: org.BuildNetwork}).BuildNetwork(),
		},
		"prompts": map[string]any{
			"attachments": (&policy.Snapshot{Attach: org.Attachments}).Attachments(),
		},
		"backend": map[string]any{
			"version": s.cfg.Version, "host": r.Host, "startedAt": ts(s.started),
			"dbSizeMb": float64(dbSize) / (1 << 20), "traceSizeMb": float64(traceSize) / (1 << 20),
		},
	}
}

func (s *Server) handleGetOrg(w http.ResponseWriter, r *http.Request) {
	if s.requireUser(w, r) == nil {
		return
	}
	org, err := s.store.Org()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.orgJSON(r, org))
}

// handleOrgPublic serves the little the sign-in page needs before anyone is
// signed in: a name to greet with, nothing an outsider could use.
func (s *Server) handleOrgPublic(w http.ResponseWriter, r *http.Request) {
	org, err := s.store.Org()
	if errors.Is(err, store.ErrNoOrg) {
		writeErr(w, http.StatusServiceUnavailable, "setup_required")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       org.Name,
		"standalone": s.cfg.Standalone,
		"sso":        map[string]any{"provider": org.SSOProvider},
		"backend": map[string]any{
			"version": s.cfg.Version, "host": r.Host,
		},
	})
}

func (s *Server) handleUpdateOrg(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var patch struct {
		Name *string `json:"name"`
		SSO  *struct {
			Issuer         *string   `json:"issuer"`
			ClientID       *string   `json:"clientId"`
			ClientSecret   *string   `json:"clientSecret"`
			Enforced       *bool     `json:"enforced"`
			AllowedDomains *[]string `json:"allowedDomains"`
		} `json:"sso"`
		TraceStore *struct {
			Location *string `json:"location"`
		} `json:"traceStore"`
		MachineSessions *struct {
			MaxDays *int `json:"maxDays"`
		} `json:"machineSessions"`
		Builds *struct {
			Network *string `json:"network"`
		} `json:"builds"`
		Prompts *struct {
			Attachments *string `json:"attachments"`
		} `json:"prompts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	// Standalone's sign-in is its own identity provider, and changing where
	// it points would lock this console out until the next start.
	if patch.SSO != nil && s.cfg.Standalone {
		writeErr(w, http.StatusConflict, "standalone signs you in as your local account; its identity settings are fixed")
		return
	}
	org, err := s.store.Org()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var changed []string
	if patch.Name != nil {
		org.Name = *patch.Name
		changed = append(changed, "name")
	}
	if patch.SSO != nil {
		if patch.SSO.Issuer != nil {
			org.OIDCIssuer = strings.TrimSuffix(strings.TrimSpace(*patch.SSO.Issuer), "/")
			org.SSOProvider = providerLabel(org.OIDCIssuer)
		}
		if patch.SSO.ClientID != nil {
			org.OIDCClientID = *patch.SSO.ClientID
		}
		if patch.SSO.ClientSecret != nil && *patch.SSO.ClientSecret != "" {
			org.OIDCClientSecret = *patch.SSO.ClientSecret
		}
		if patch.SSO.Enforced != nil {
			org.SSOEnforced = *patch.SSO.Enforced
		}
		if patch.SSO.AllowedDomains != nil {
			domains, err := auth.NormalizeDomains(*patch.SSO.AllowedDomains)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "allowed domains: "+err.Error())
				return
			}
			// The administrator making the change has to be able to sign in
			// after it, or the next expired session locks the console.
			if len(domains) > 0 && !slices.Contains(domains, auth.DomainOf(admin.Email)) {
				writeErr(w, http.StatusBadRequest,
					"allowed domains must include your own, "+auth.DomainOf(admin.Email)+", or you could not sign in again")
				return
			}
			org.AllowedDomains = domains
		}
		changed = append(changed, "sso")
	}
	if patch.TraceStore != nil && patch.TraceStore.Location != nil {
		// Held to what setup holds it to: somewhere this process can write,
		// or the next trace record has nowhere to go and is lost.
		where := strings.Trim(strings.TrimSpace(*patch.TraceStore.Location), `"'`)
		if where == "" {
			writeErr(w, http.StatusBadRequest, "traces need somewhere to be written")
			return
		}
		if abs, err := filepath.Abs(where); err == nil {
			where = abs
		}
		if err := writableDir(where); err != nil {
			writeErr(w, http.StatusBadRequest, "traces cannot be written to "+where+": "+err.Error())
			return
		}
		org.TracePath = where
		changed = append(changed, "traceStore")
	}
	if patch.MachineSessions != nil && patch.MachineSessions.MaxDays != nil {
		days := *patch.MachineSessions.MaxDays
		if days < 0 {
			writeErr(w, http.StatusBadRequest, "machine session lifetime cannot be negative")
			return
		}
		org.MachineSessionMaxDays = days
		changed = append(changed, "machineSessions")
	}
	if patch.Builds != nil && patch.Builds.Network != nil {
		where := *patch.Builds.Network
		switch where {
		case policy.BuildDirect, policy.BuildProxied:
		default:
			writeErr(w, http.StatusBadRequest, "unknown build network setting: "+where)
			return
		}
		org.BuildNetwork = where
		changed = append(changed, "builds")
	}
	if patch.Prompts != nil && patch.Prompts.Attachments != nil {
		mode := *patch.Prompts.Attachments
		switch mode {
		case policy.AttachmentsGuarded, policy.AttachmentsOn, policy.AttachmentsOff:
		default:
			writeErr(w, http.StatusBadRequest, "unknown attachment setting: "+mode)
			return
		}
		org.Attachments = mode
		changed = append(changed, "prompts")
	}
	if len(changed) == 0 {
		writeJSON(w, http.StatusOK, s.orgJSON(r, org))
		return
	}
	if err := s.store.SaveOrg(org); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var what []string
	if contains(changed, "builds") {
		what = append(what, store.ChangeBuilds)
	}
	if contains(changed, "prompts") {
		what = append(what, store.ChangePrompts)
	}
	if len(what) > 0 {
		// Both ride in the signed snapshot, so a machine holding one from
		// before the change is holding the old answer. Moving the epoch is
		// what sends every live session a policy.refresh and makes the new
		// one take effect at the next build, or the next prompt, rather than
		// at the next expiry.
		s.store.BumpPolicyEpoch(what...)
	}
	s.store.RecordChange(admin.ID, "settings", "updated", "org", org.Name,
		"Changed "+strings.Join(changed, ", ")+".", nil)
	writeJSON(w, http.StatusOK, s.orgJSON(r, org))
}

// --- little helpers ---

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func countOrNone(n int, noun string) string {
	switch n {
	case 0:
		return "none."
	case 1:
		return "1 " + noun + "."
	default:
		return strconv.Itoa(n) + " " + noun + "s."
	}
}
