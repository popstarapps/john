package backend

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/store"
)

// --- network access requests ---

func (s *Server) requestJSON(r *store.AccessRequest) map[string]any {
	evidence := r.EvidenceIDs
	if evidence == nil {
		evidence = []string{}
	}
	comments := []map[string]any{}
	// A request that cannot load its conversation is still a request worth
	// showing; the thread is the one part of it that is safe to come back
	// empty rather than fail the whole read.
	if cs, err := s.store.RequestComments(r.ID); err == nil {
		for _, c := range cs {
			comments = append(comments, map[string]any{
				"id": c.ID, "authorId": c.AuthorID, "at": ts(c.At), "body": c.Body,
			})
		}
	}
	return map[string]any{
		"id": r.ID, "ref": r.Ref, "title": r.Title, "requesterId": r.RequesterID,
		"teamId": r.TeamID, "status": r.Status, "justification": r.Justification,
		"createdAt": ts(r.CreatedAt), "decidedAt": tsPtr(r.DecidedAt),
		"decidedById": r.DecidedByID, "decisionNote": r.DecisionNote,
		"proposed": map[string]any{
			"groupId": r.GroupID, "groupName": r.GroupName, "spec": r.Spec,
		},
		"evidenceRequestIds": evidence,
		"comments":           comments,
	}
}

const blockedHeading = "Blocked in a john session:"

// maxBlockedLines bounds the evidence kept on one request. The first few say
// what was being attempted; a build that fetched two hundred files does not
// need all two hundred to make its case.
const maxBlockedLines = 20

// withBlocked adds a blocked request to a justification's list of them, once.
func withBlocked(justification, blocked string) string {
	if blocked == "" {
		return justification
	}
	line := "- " + blocked
	lines := strings.Split(justification, "\n")
	n := 0
	for _, l := range lines {
		if l == line {
			return justification
		}
		if strings.HasPrefix(l, "- ") {
			n++
		}
	}
	switch {
	case n == maxBlockedLines:
		return justification + "\n- … and more"
	case n > maxBlockedLines:
		return justification
	}
	return strings.TrimRight(justification, "\n") + "\n" + line
}

// openRequestFor finds the requester's open request that spec belongs with:
// the same scheme, host and port, and a path one of them covers — either is
// the whole host, or both are the same path. Two different exact paths stay
// two requests; that is what the developer asked for.
func (s *Server) openRequestFor(requesterID string, spec policy.NetworkSpec) *store.AccessRequest {
	open, err := s.store.AccessRequests("open", requesterID, "", "")
	if err != nil {
		return nil
	}
	for _, r := range open {
		var have policy.NetworkSpec
		if json.Unmarshal(r.Spec, &have) != nil || have.Kind != "network" {
			continue
		}
		covers := have.Path == "/*" || spec.Path == "/*" || have.Path == spec.Path
		if strings.EqualFold(have.Host, spec.Host) && have.Scheme == spec.Scheme && samePort(have.Port, spec.Port) && covers {
			return r
		}
	}
	return nil
}

func samePort(a, b *int) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// mergeRequest widens an open request to cover another blocked request for
// its host: the method joins the list, a request for the whole host widens it
// to the whole host, and the blocked request joins the evidence.
func (s *Server) mergeRequest(r *store.AccessRequest, add policy.NetworkSpec, blocked string) (*store.AccessRequest, error) {
	var spec policy.NetworkSpec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, err
	}
	for _, m := range add.Methods {
		if !contains(spec.Methods, m) {
			spec.Methods = append(spec.Methods, m)
		}
	}
	if add.Path == "/*" {
		spec.Path = "/*"
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	justification := r.Justification
	if blocked != "" && !strings.Contains(justification, blockedHeading) {
		justification = strings.TrimRight(justification, "\n") + "\n\n" + blockedHeading
	}
	return s.store.ExtendAccessRequest(r.ID, raw, withBlocked(justification, blocked))
}

func (s *Server) handleListAccessRequests(w http.ResponseWriter, r *http.Request) {
	viewer := s.requireUser(w, r)
	if viewer == nil {
		return
	}
	q := r.URL.Query()
	requesterID := q.Get("requesterId")
	// A member sees their own requests, whatever they asked for. "me" is how
	// a client asks for its own developer's without knowing their id — and in
	// standalone the developer is the administrator, who would otherwise get
	// everyone's.
	if viewer.Role != "admin" || requesterID == "me" {
		requesterID = viewer.ID
	}
	reqs, err := s.store.AccessRequests(q.Get("status"), requesterID, q.Get("teamId"), q.Get("search"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, req := range reqs {
		out = append(out, s.requestJSON(req))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetAccessRequest(w http.ResponseWriter, r *http.Request) {
	viewer := s.requireUser(w, r)
	if viewer == nil {
		return
	}
	req, err := s.store.AccessRequestByIDOrRef(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such request")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if viewer.Role != "admin" && req.RequesterID != viewer.ID {
		writeErr(w, http.StatusForbidden, "not your request")
		return
	}
	writeJSON(w, http.StatusOK, s.requestJSON(req))
}

func (s *Server) handleCreateAccessRequest(w http.ResponseWriter, r *http.Request) {
	viewer := s.requireUser(w, r)
	if viewer == nil {
		return
	}
	var req struct {
		Title              string          `json:"title"`
		Justification      string          `json:"justification"`
		TeamID             string          `json:"teamId"`
		GroupID            *string         `json:"groupId"`
		GroupName          string          `json:"groupName"`
		Spec               json.RawMessage `json:"spec"`
		EvidenceRequestIDs []string        `json:"evidenceRequestIds"`
		// Blocked is the request that prompted this one, "GET https://…",
		// kept on the request as evidence.
		Blocked string `json:"blocked"`
		// MergeOnly adds to an open request for the same host and creates
		// nothing: a client that already filed for a host reports later
		// blocks this way, so a decided request is never re-filed unasked.
		MergeOnly bool `json:"mergeOnly"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Title == "" || len(req.Spec) == 0 {
		writeErr(w, http.StatusBadRequest, "title and spec are required")
		return
	}
	if req.Blocked != "" && req.Justification == "" {
		req.Justification = blockedHeading
	}
	req.Justification = withBlocked(req.Justification, req.Blocked)

	// One request per host per person while it is open. Asking for the same
	// host again — the next file apt fetches, the same command run twice —
	// adds to the request an administrator will read, rather than handing
	// them three to decide one question.
	var spec policy.NetworkSpec
	if json.Unmarshal(req.Spec, &spec) == nil && spec.Kind == "network" {
		if open := s.openRequestFor(viewer.ID, spec); open != nil {
			merged, err := s.mergeRequest(open, spec, req.Blocked)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			out := s.requestJSON(merged)
			out["merged"] = true
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	if req.MergeOnly {
		writeErr(w, http.StatusNotFound, "no open request for this host")
		return
	}
	// The request is filed for one of the requester's own teams.
	teams, err := s.store.TeamIDsForUser(viewer.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.TeamID != "" && !contains(teams, req.TeamID) && viewer.Role != "admin" {
		writeErr(w, http.StatusForbidden, "not one of your teams")
		return
	}
	created, err := s.store.CreateAccessRequest(viewer.ID, req.TeamID, req.Title,
		req.Justification, req.GroupName, req.GroupID, req.Spec, req.EvidenceRequestIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.requestJSON(created))
}

func (s *Server) handleDecideAccessRequest(w http.ResponseWriter, r *http.Request) {
	admin := s.requireAdmin(w, r)
	if admin == nil {
		return
	}
	var req struct {
		Status  string          `json:"status"`
		Spec    json.RawMessage `json:"spec"`
		GroupID *string         `json:"groupId"`
		// NewGroup is the group to create for this policy, when approving
		// into none of the existing ones: its name, and who it applies to.
		NewGroup *struct {
			Name     string   `json:"name"`
			Baseline bool     `json:"baseline"`
			TeamIDs  []string `json:"teamIds"`
		} `json:"newGroup"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.GroupID != nil && *req.GroupID == "" {
		req.GroupID = nil
	}

	// An approval writes a policy, and a policy lives in a group. Approving
	// into no group used to mark the request approved and write nothing, so
	// the developer waited on a policy that did not exist.
	if req.Status == "approved" {
		switch {
		case req.GroupID != nil && req.NewGroup != nil:
			writeErr(w, http.StatusBadRequest, "choose an existing group or a new one, not both")
			return
		case req.GroupID != nil:
			if _, err := s.store.PolicyGroupByID(*req.GroupID); err != nil {
				writeErr(w, http.StatusBadRequest, "no such group")
				return
			}
		case req.NewGroup != nil:
			ng := req.NewGroup
			ng.Name = strings.TrimSpace(ng.Name)
			if ng.Name == "" {
				writeErr(w, http.StatusBadRequest, "the new group needs a name")
				return
			}
			// A group attached to no team and not baseline applies to
			// nobody, and approving into it would be the same silence.
			if !ng.Baseline && len(ng.TeamIDs) == 0 {
				writeErr(w, http.StatusBadRequest, "the new group applies to nobody: make it baseline or attach it to a team")
				return
			}
			teams := make([]*store.Team, 0, len(ng.TeamIDs))
			for _, id := range ng.TeamIDs {
				t, err := s.store.TeamByIDOrSlug(id)
				if err != nil {
					writeErr(w, http.StatusBadRequest, "no such team: "+id)
					return
				}
				teams = append(teams, t)
			}
			g, err := s.store.CreatePolicyGroup(ng.Name, "Made when approving an access request.", ng.Baseline, admin.ID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			s.store.RecordChange(admin.ID, "group", "created", g.ID, g.Name, "Created policy group "+g.Name+".", nil)
			for _, t := range teams {
				if err := s.store.SetTeamGroups(t.ID, append(t.GroupIDs, g.ID)); err != nil {
					writeErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				s.store.RecordChange(admin.ID, "team", "attached", t.ID, t.Name, "Attached "+g.Name+" to "+t.Name+".", nil)
			}
			req.GroupID = &g.ID
		default:
			writeErr(w, http.StatusBadRequest, "choose the group this policy goes in")
			return
		}
	}
	groupName := ""
	if req.GroupID != nil {
		if g, err := s.store.PolicyGroupByID(*req.GroupID); err == nil {
			groupName = g.Name
		}
	}
	decided, err := s.store.DecideAccessRequest(r.PathValue("id"), req.Status, admin.ID, req.Spec, req.GroupID, groupName)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such request")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	verb := "Rejected"
	if decided.Status == "approved" {
		verb = "Approved"
	}
	s.store.RecordChange(admin.ID, "request", decided.Status, decided.ID, decided.Ref,
		verb+" "+decided.Ref+" — "+decided.Title+".", nil)

	// The developer who asked is told, in whatever session they have open —
	// before the policy is written, so that the decision is on their screen
	// ahead of the policy.refresh the write sends. The refresh that arrives
	// after it is then the approval they were just told about, and says
	// nothing; arriving first, it said "policies updated" for a change
	// nobody had explained yet.
	if s.hub != nil {
		var spec policy.NetworkSpec
		json.Unmarshal(decided.Spec, &spec)
		s.hub.tellUser(decided.RequesterID, &protocol.AccessDecision{
			Ref: decided.Ref, Title: decided.Title, Host: spec.Host, Status: decided.Status})
	}

	// Approving writes the policy the reviewer had in front of them — the
	// possibly-edited spec, into the chosen group — as its own recorded change.
	if decided.Status == "approved" && decided.GroupID != nil {
		requesterName := "a developer"
		if u, err := s.store.UserByID(decided.RequesterID); err == nil {
			requesterName = u.Name
		}
		p, err := s.store.CreatePolicy(*decided.GroupID, decided.Title,
			"From "+decided.Ref+", requested by "+requesterName+".", decided.Spec, admin.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "request approved but policy write failed: "+err.Error())
			return
		}
		groupName := *decided.GroupID
		if g, err := s.store.PolicyGroupByID(p.GroupID); err == nil {
			groupName = g.Name
		}
		s.store.RecordChange(admin.ID, "policy", "created", p.ID, p.Name,
			"Added "+p.Type+" policy "+p.Name+" to "+groupName+".", &p.Version)
	}
	writeJSON(w, http.StatusOK, s.requestJSON(decided))
}

// handleCommentAccessRequest is the conversation a request can have before
// it is decided.
//
// A reviewer who needs something explained marks the request needs-info,
// which takes it out of the queue of things waiting on them and puts it in
// front of the requester. The requester's reply puts it back: they have
// answered, so it is waiting on a reviewer again, and nobody has to remember
// to move it by hand. That round trip is the whole point of the status —
// without it, needs-info is a label a request never leaves.
func (s *Server) handleCommentAccessRequest(w http.ResponseWriter, r *http.Request) {
	viewer := s.requireUser(w, r)
	if viewer == nil {
		return
	}
	req, err := s.store.AccessRequestByIDOrRef(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such request")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The two people a request concerns, and nobody else.
	isAdmin := viewer.Role == "admin"
	if !isAdmin && req.RequesterID != viewer.ID {
		writeErr(w, http.StatusForbidden, "not your request")
		return
	}
	var body struct {
		Body      string `json:"body"`
		NeedsInfo bool   `json:"needsInfo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Body) == "" {
		writeErr(w, http.StatusBadRequest, "a comment body is required")
		return
	}
	open := req.Status == "pending" || req.Status == "needs-info"
	if !open {
		writeErr(w, http.StatusBadRequest, "request "+req.Ref+" is already "+req.Status)
		return
	}
	if _, err := s.store.AddRequestComment(req.ID, viewer.ID, body.Body); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	switch {
	case isAdmin && body.NeedsInfo:
		if _, err := s.store.SetAccessRequestOpenStatus(req.ID, "needs-info"); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.store.RecordChange(viewer.ID, "request", "updated", req.ID, req.Ref,
			"Asked for more information on "+req.Ref+" — "+req.Title+".", nil)
	case !isAdmin && req.Status == "needs-info":
		// The requester answered, so it is a reviewer's again.
		if _, err := s.store.SetAccessRequestOpenStatus(req.ID, "pending"); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	updated, err := s.store.AccessRequestByIDOrRef(req.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.requestJSON(updated))
}

// --- proxy traffic ---

func networkJSON(n *store.NetworkRequest) map[string]any {
	headers := [][]string{}
	for _, h := range n.Headers {
		headers = append(headers, []string{h[0], h[1]})
	}
	return map[string]any{
		"id": n.ID, "at": n.At.UTC().Format(time.RFC3339), "userId": n.UserID,
		"sessionId": n.SessionID, "method": n.Method, "scheme": n.Scheme,
		"host": n.Host, "port": n.Port, "path": n.Path, "query": n.Query,
		"status": n.Status, "policyId": n.PolicyID, "reason": n.Reason,
		"bytesOut": n.BytesOut, "bytesIn": n.BytesIn, "durationMs": n.DurationMS,
		"responseStatus": n.ResponseStatus, "headers": headers,
		"bodyPreview": n.BodyPreview, "contentType": n.ContentType,
	}
}

func (s *Server) handleListNetworkRequests(w http.ResponseWriter, r *http.Request) {
	viewer := s.requireUser(w, r)
	if viewer == nil {
		return
	}
	q := r.URL.Query()
	userID := q.Get("userId")
	if viewer.Role != "admin" {
		userID = viewer.ID // members see their own traffic only
	}
	var ids []string
	if v := q.Get("ids"); v != "" {
		ids = strings.Split(v, ",")
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		limit, _ = strconv.Atoi(v)
	}
	reqs, err := s.store.NetworkRequests(ids, userID, q.Get("sessionId"), q.Get("status"), q.Get("search"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]any{}
	for _, n := range reqs {
		out = append(out, networkJSON(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetNetworkRequest(w http.ResponseWriter, r *http.Request) {
	viewer := s.requireUser(w, r)
	if viewer == nil {
		return
	}
	n, err := s.store.NetworkRequestByID(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such request")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if viewer.Role != "admin" && n.UserID != viewer.ID {
		writeErr(w, http.StatusForbidden, "not your traffic")
		return
	}
	writeJSON(w, http.StatusOK, networkJSON(n))
}

// --- usage ---

func usageTotalsJSON(t store.UsageTotals) map[string]any {
	return map[string]any{
		"requests": t.Requests, "tokensIn": t.TokensIn, "tokensOut": t.TokensOut,
		"tokensCached": t.TokensCached, "sessions": t.Sessions, "allowed": t.Allowed,
		"approvals": t.Approvals, "blocked": t.Blocked, "costUsd": t.CostUSD,
	}
}

func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	viewer := s.requireUser(w, r)
	if viewer == nil {
		return
	}
	q := r.URL.Query()
	days := 30
	if v, err := strconv.Atoi(q.Get("days")); err == nil && v > 0 && v <= 365 {
		days = v
	}
	// The org view is an administrator's; everyone else gets themselves,
	// whatever the query said.
	scopeUser := viewer.ID
	if q.Get("scope") == "org" && viewer.Role == "admin" {
		scopeUser = ""
	}

	dayRows, totals, previous, byModel, byTeam, byUser, err := s.store.UsageSummary(scopeUser, days, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	daysOut := []map[string]any{}
	for _, d := range dayRows {
		models := []map[string]any{}
		for _, m := range d.ByModel {
			models = append(models, map[string]any{
				"modelId": m.ModelID, "requests": m.Requests, "tokens": m.Tokens, "costUsd": m.CostUSD,
			})
		}
		day := usageTotalsJSON(d.Totals)
		day["date"] = d.Date
		day["byModel"] = models
		daysOut = append(daysOut, day)
	}

	modelRows := []map[string]any{}
	for _, m := range byModel {
		model, err := s.store.ModelByID(m.ModelID)
		if err != nil {
			continue // a deleted model's rows still count in totals, just not here
		}
		modelRows = append(modelRows, map[string]any{
			"model": modelJSON(model), "requests": m.Requests, "tokens": m.Tokens, "costUsd": m.CostUSD,
		})
	}
	teamRows := []map[string]any{}
	for _, a := range byTeam {
		team, err := s.store.TeamByIDOrSlug(a.ID)
		if err != nil {
			continue
		}
		teamRows = append(teamRows, map[string]any{
			"team": teamJSON(team), "requests": a.Requests, "sessions": a.Sessions, "costUsd": a.CostUSD,
		})
	}
	userRows := []map[string]any{}
	for _, a := range byUser {
		u, err := s.store.UserByID(a.ID)
		if err != nil {
			continue
		}
		j, err := s.userWithTeams(u)
		if err != nil {
			continue
		}
		userRows = append(userRows, map[string]any{
			"user": j, "requests": a.Requests, "sessions": a.Sessions, "costUsd": a.CostUSD,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"days":     daysOut,
		"totals":   usageTotalsJSON(totals),
		"previous": usageTotalsJSON(previous),
		"byModel":  modelRows,
		"byTeam":   teamRows,
		"byUser":   userRows,
	})
}
