package backend

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/popstarapps/john/store"
)

// adminAndMember boots a backend, signs an administrator in through the real
// browser flow, and gives a second person a machine token — the two identities
// every admin endpoint has to tell apart.
func adminAndMember(t *testing.T) (*Server, *apiClient, *apiClient, *store.User) {
	t.Helper()
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	browser := browserClient(t)
	resp, err := browser.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin := &apiClient{t: t, base: base, http: browser}

	user, err := srv.Store().UpsertSSOUser("dev-2", "casey@example.com", "Casey Lee", "")
	if err != nil {
		t.Fatal(err)
	}
	token, err := srv.Store().CreateMachineToken(user.ID, "laptop", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return srv, admin, &apiClient{t: t, base: base, token: token}, user
}

func TestAdministratorsChangeRolesAndAccess(t *testing.T) {
	srv, admin, member, memberUser := adminAndMember(t)

	var viewer struct {
		User struct{ ID string }
	}
	admin.get("/api/v1/viewer", &viewer)

	// Nobody but an administrator may reach either control.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"PUT", "/api/v1/users/" + viewer.User.ID + "/role", map[string]any{"role": "member"}},
		{"PUT", "/api/v1/users/" + viewer.User.ID + "/status", map[string]any{"status": "suspended"}},
	} {
		if code, body := member.do(c.method, c.path, c.body); code != http.StatusForbidden {
			t.Fatalf("member %s %s = %d, want 403: %s", c.method, c.path, code, body)
		}
	}

	// An administrator cannot demote or suspend themselves — the one change
	// that could leave the organization with nobody able to undo it.
	for _, c := range []struct {
		path string
		body any
	}{
		{"/api/v1/users/" + viewer.User.ID + "/role", map[string]any{"role": "member"}},
		{"/api/v1/users/" + viewer.User.ID + "/status", map[string]any{"status": "suspended"}},
	} {
		if code, body := admin.do("PUT", c.path, c.body); code != http.StatusForbidden {
			t.Fatalf("self %s = %d, want 403: %s", c.path, code, body)
		}
	}

	// Promotion is real: the member can read an admin-only surface afterwards
	// and not before.
	if code, _ := member.do("GET", "/api/v1/users", nil); code != http.StatusForbidden {
		t.Fatal("a member could already list users")
	}
	var promoted struct{ Role, Status string }
	admin.post("PUT", "/api/v1/users/"+memberUser.ID+"/role", map[string]any{"role": "admin"}, &promoted)
	if promoted.Role != "admin" {
		t.Fatalf("promoted = %+v", promoted)
	}
	if code, body := member.do("GET", "/api/v1/users", nil); code != http.StatusOK {
		t.Fatalf("newly promoted admin GET /users = %d: %s", code, body)
	}

	var demoted struct{ Role string }
	admin.post("PUT", "/api/v1/users/"+memberUser.ID+"/role", map[string]any{"role": "member"}, &demoted)
	if demoted.Role != "member" {
		t.Fatalf("demoted = %+v", demoted)
	}
	if code, _ := admin.do("PUT", "/api/v1/users/"+memberUser.ID+"/role",
		map[string]any{"role": "owner"}); code != http.StatusBadRequest {
		t.Fatal("an unknown role must be refused")
	}
	if code, _ := admin.do("PUT", "/api/v1/users/u_nobody/role",
		map[string]any{"role": "admin"}); code != http.StatusNotFound {
		t.Fatal("an unknown user must be a 404")
	}

	// Suspension has to reach the machine that is already signed in, not just
	// the next sign-in: the token in that client's hands stops working.
	var suspended struct{ Status string }
	admin.post("PUT", "/api/v1/users/"+memberUser.ID+"/status",
		map[string]any{"status": "suspended"}, &suspended)
	if suspended.Status != "suspended" {
		t.Fatalf("suspended = %+v", suspended)
	}
	if code, body := member.do("GET", "/api/v1/viewer", nil); code != http.StatusUnauthorized {
		t.Fatalf("a suspended person's machine still works: %d %s", code, body)
	}

	admin.post("PUT", "/api/v1/users/"+memberUser.ID+"/status",
		map[string]any{"status": "active"}, nil)
	// Reactivating does not resurrect the revoked credential; they sign in again.
	if code, _ := member.do("GET", "/api/v1/viewer", nil); code != http.StatusUnauthorized {
		t.Fatal("reactivating must not bring a revoked machine token back")
	}
	fresh, err := srv.Store().CreateMachineToken(memberUser.ID, "laptop", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	member.token = fresh
	if code, body := member.do("GET", "/api/v1/viewer", nil); code != http.StatusOK {
		t.Fatalf("a reactivated person cannot sign in again: %d %s", code, body)
	}

	// The whole of it is in the change history.
	var changes []struct{ Entity, Action, Summary string }
	admin.get("/api/v1/changes?entity=member", &changes)
	seen := map[string]bool{}
	for _, c := range changes {
		seen[c.Action] = true
	}
	for _, want := range []string{"updated", "disabled"} {
		if !seen[want] {
			t.Fatalf("change history is missing member/%s: %+v", want, changes)
		}
	}
}

func TestModelsCanBeEditedAndDeleted(t *testing.T) {
	srv, admin, member, _ := adminAndMember(t)

	var kept, spare struct{ ID string }
	admin.post("POST", "/api/v1/models", map[string]any{
		"name": "Claude", "provider": "anthropic", "hosting": "external",
		"endpoint": "https://api.anthropic.com", "modelId": "claude-opus-5",
		"contextWindow": 200000, "costPerMTokIn": 5.0, "costPerMTokOut": 25.0,
		"apiKey": "sk-original",
	}, &kept)
	admin.post("POST", "/api/v1/models", map[string]any{
		"name": "Spare", "provider": "anthropic", "modelId": "spare", "apiKey": "sk-spare",
	}, &spare)

	if code, _ := member.do("PUT", "/api/v1/models/"+kept.ID, map[string]any{"name": "x"}); code != http.StatusForbidden {
		t.Fatal("a member must not edit a model")
	}
	if code, _ := member.do("DELETE", "/api/v1/models/"+spare.ID, nil); code != http.StatusForbidden {
		t.Fatal("a member must not delete a model")
	}

	// The provider's own identifier comes back, because an edit form that
	// could not show it would blank it on the way out.
	var listedBefore []struct{ ID, ModelID string }
	admin.get("/api/v1/models", &listedBefore)
	for _, m := range listedBefore {
		if m.ID == kept.ID && m.ModelID != "claude-opus-5" {
			t.Fatalf("model identifier came back as %q", m.ModelID)
		}
	}

	// An edit that says nothing about the credential keeps the stored one.
	var edited struct {
		Name, Endpoint string
		ContextWindow  int
		CostPerMTokOut float64
	}
	admin.post("PUT", "/api/v1/models/"+kept.ID, map[string]any{
		"name": "Claude Opus", "provider": "anthropic", "hosting": "external",
		"endpoint": "https://anthropic.internal.example", "modelId": "claude-opus-5",
		"contextWindow": 1000000, "costPerMTokIn": 4.0, "costPerMTokOut": 20.0,
		"apiKey": "",
	}, &edited)
	if edited.Name != "Claude Opus" || edited.ContextWindow != 1000000 || edited.CostPerMTokOut != 20 {
		t.Fatalf("edited = %+v", edited)
	}
	stored, err := srv.Store().ModelByID(kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.APIKey != "sk-original" {
		t.Fatalf("api key = %q, want the stored one untouched by an edit", stored.APIKey)
	}
	// A key is write-only: no read of a model ever hands one back.
	var listed []map[string]any
	admin.get("/api/v1/models", &listed)
	for _, m := range listed {
		if _, ok := m["apiKey"]; ok {
			t.Fatalf("a model came back carrying its credential: %+v", m)
		}
	}

	admin.post("PUT", "/api/v1/models/"+kept.ID, map[string]any{
		"name": "Claude Opus", "provider": "anthropic", "endpoint": "https://anthropic.internal.example",
		"modelId": "claude-opus-5", "apiKey": "sk-rotated",
	}, nil)
	if stored, _ := srv.Store().ModelByID(kept.ID); stored.APIKey != "sk-rotated" {
		t.Fatalf("api key = %q, want it rotated", stored.APIKey)
	}

	// A routing policy naming a model holds it in place: deleting it would
	// leave the policy pointing at nothing, which in restrict mode narrows a
	// session to no model at all.
	var group struct{ ID string }
	admin.post("POST", "/api/v1/groups", map[string]any{"name": "routing", "baseline": false}, &group)
	admin.post("POST", "/api/v1/policies", map[string]any{
		"groupId": group.ID, "name": "only the internal one", "description": "",
		"spec": map[string]any{"kind": "model-routing", "mode": "restrict",
			"modelIds": []string{kept.ID}, "keywords": []string{}},
	}, nil)

	code, body := admin.do("DELETE", "/api/v1/models/"+kept.ID, nil)
	if code != http.StatusConflict {
		t.Fatalf("deleting a model a policy names = %d, want 409: %s", code, body)
	}
	// The refusal names the policy, so the admin knows where to go.
	if !strings.Contains(string(body), "only the internal one") {
		t.Fatalf("the refusal must name the policy holding it: %s", body)
	}
	if _, err := srv.Store().ModelByID(kept.ID); err != nil {
		t.Fatalf("a refused delete must leave the model alone: %v", err)
	}

	// One nothing points at goes.
	admin.post("DELETE", "/api/v1/models/"+spare.ID, nil, nil)
	if _, err := srv.Store().ModelByID(spare.ID); err != store.ErrNotFound {
		t.Fatalf("model survived delete: %v", err)
	}
	if code, _ := admin.do("DELETE", "/api/v1/models/"+spare.ID, nil); code != http.StatusNotFound {
		t.Fatal("deleting twice must be a 404")
	}

	var changes []struct{ Entity, Action string }
	admin.get("/api/v1/changes?entity=model", &changes)
	seen := map[string]bool{}
	for _, c := range changes {
		seen[c.Action] = true
	}
	for _, want := range []string{"created", "updated", "deleted"} {
		if !seen[want] {
			t.Fatalf("change history is missing model/%s: %+v", want, changes)
		}
	}
}

func TestDeletingATeamStopsItsPoliciesApplying(t *testing.T) {
	_, admin, member, memberUser := adminAndMember(t)

	var group struct{ ID string }
	admin.post("POST", "/api/v1/groups", map[string]any{
		"name": "payments-restricted", "baseline": false,
	}, &group)
	var team struct{ ID, Slug string }
	admin.post("POST", "/api/v1/teams", map[string]any{
		"name": "Payments", "slug": "payments", "description": "", "groupIds": []string{group.ID},
	}, &team)
	admin.post("PUT", "/api/v1/users/"+memberUser.ID+"/teams",
		map[string]any{"teamIds": []string{team.ID}}, nil)

	var groups []struct{ ID string }
	member.get("/api/v1/users/"+memberUser.ID+"/groups", &groups)
	if len(groups) != 1 {
		t.Fatalf("groups before delete = %d, want the team's one", len(groups))
	}

	if code, _ := member.do("DELETE", "/api/v1/teams/"+team.ID, nil); code != http.StatusForbidden {
		t.Fatal("a member must not delete a team")
	}
	// The slug is what the console's URLs carry, so it has to work here too.
	admin.post("DELETE", "/api/v1/teams/"+team.Slug, nil, nil)

	if code, _ := admin.do("GET", "/api/v1/teams/"+team.Slug, nil); code != http.StatusNotFound {
		t.Fatal("the team survived the delete")
	}
	member.get("/api/v1/users/"+memberUser.ID+"/groups", &groups)
	if len(groups) != 0 {
		t.Fatalf("groups after delete = %d, want none — the team carried them", len(groups))
	}
	if code, _ := admin.do("DELETE", "/api/v1/teams/"+team.ID, nil); code != http.StatusNotFound {
		t.Fatal("deleting twice must be a 404")
	}

	// Deleting a team loosens as much as it tightens, so the history says who
	// it affected rather than only that it happened.
	var changes []struct{ Entity, Action, Summary string }
	admin.get("/api/v1/changes?entity=team", &changes)
	found := false
	for _, c := range changes {
		if c.Action == "deleted" && strings.Contains(c.Summary, "1 member") {
			found = true
		}
	}
	if !found {
		t.Fatalf("change history does not record who the delete affected: %+v", changes)
	}
}

// The needs-info round trip is the whole reason the status exists: a reviewer
// hands the request back, and the answer hands it forward again without
// anybody moving it by hand.
func TestAskingForMoreInformationAndAnsweringIt(t *testing.T) {
	srv, admin, member, memberUser := adminAndMember(t)

	spec := map[string]any{"kind": "network", "methods": []string{"GET"}, "scheme": "https",
		"host": "registry.npmjs.org", "port": nil, "path": "/*",
		"headerMatches": []any{}, "queryMatches": []any{},
		"decision": "always-allow", "maxBodyBytes": nil}

	var filed struct{ ID, Ref, Status string }
	member.post("POST", "/api/v1/access-requests", map[string]any{
		"title": "npm registry", "justification": "dependencies", "teamId": "",
		"groupId": nil, "groupName": "", "spec": spec,
	}, &filed)

	type comment struct {
		ID       string
		AuthorID string
		At       string
		Body     string
	}
	type request struct {
		Status   string
		Comments []comment
	}

	// A comment needs something in it.
	if code, _ := admin.do("POST", "/api/v1/access-requests/"+filed.ID+"/comments",
		map[string]any{"body": "   "}); code != http.StatusBadRequest {
		t.Fatal("an empty comment must be refused")
	}

	// The reviewer asks, and the request leaves their queue for the requester's.
	var asked request
	admin.post("POST", "/api/v1/access-requests/"+filed.ID+"/comments",
		map[string]any{"body": "Which build needs this?", "needsInfo": true}, &asked)
	if asked.Status != "needs-info" {
		t.Fatalf("status after asking = %q, want needs-info", asked.Status)
	}
	if len(asked.Comments) != 1 || asked.Comments[0].Body != "Which build needs this?" {
		t.Fatalf("comments = %+v", asked.Comments)
	}
	if asked.Comments[0].At == "" {
		t.Fatal("a comment must come back with when it was said")
	}

	// The requester answers, and it is a reviewer's again — nobody moved it.
	var answered request
	member.post("POST", "/api/v1/access-requests/"+filed.ID+"/comments",
		map[string]any{"body": "The payments UI build."}, &answered)
	if answered.Status != "pending" {
		t.Fatalf("status after answering = %q, want pending", answered.Status)
	}
	if len(answered.Comments) != 2 || answered.Comments[1].AuthorID != memberUser.ID {
		t.Fatalf("comments = %+v", answered.Comments)
	}

	// A member cannot hand their own request back to themselves.
	var notAsked request
	member.post("POST", "/api/v1/access-requests/"+filed.ID+"/comments",
		map[string]any{"body": "bump", "needsInfo": true}, &notAsked)
	if notAsked.Status != "pending" {
		t.Fatalf("a requester moved their own request to %q", notAsked.Status)
	}

	// Both sides read the same thread through their own view of the request.
	var mine request
	member.get("/api/v1/access-requests/"+filed.ID, &mine)
	if len(mine.Comments) != 3 {
		t.Fatalf("requester sees %d comments, want 3", len(mine.Comments))
	}

	// Somebody else's request is nobody else's business.
	stranger, err := srv.Store().UpsertSSOUser("dev-3", "sam@example.com", "Sam", "")
	if err != nil {
		t.Fatal(err)
	}
	strangerToken, err := srv.Store().CreateMachineToken(stranger.ID, "laptop", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other := &apiClient{t: t, base: admin.base, token: strangerToken}
	if code, _ := other.do("POST", "/api/v1/access-requests/"+filed.ID+"/comments",
		map[string]any{"body": "me too"}); code != http.StatusForbidden {
		t.Fatal("a stranger must not comment on someone's request")
	}

	// Once decided, the conversation is closed: the argument for the decision
	// stays exactly as it was when the decision was made.
	admin.post("POST", "/api/v1/access-requests/"+filed.ID+"/decision",
		map[string]any{"status": "rejected"}, nil)
	if code, _ := admin.do("POST", "/api/v1/access-requests/"+filed.ID+"/comments",
		map[string]any{"body": "one more thing"}); code != http.StatusBadRequest {
		t.Fatal("a decided request must not take new comments")
	}

	var history []struct{ Entity, Action string }
	admin.get("/api/v1/changes?entity=request", &history)
	seen := map[string]bool{}
	for _, c := range history {
		seen[c.Action] = true
	}
	if !seen["updated"] || !seen["rejected"] {
		t.Fatalf("change history is missing the ask or the decision: %+v", history)
	}
}

// TestSignInIsHeldToTheAllowedDomains walks the setting from the console to
// the door: an administrator narrows the domains, cannot narrow them past
// their own, and a sign-in from outside them is refused before anyone is
// created.
func TestSignInIsHeldToTheAllowedDomains(t *testing.T) {
	requireConsole(t) // the sign-in ends on a console route, which must render
	srv, admin, member, _ := adminAndMember(t)
	base := "http://" + srv.Addr()

	var viewer struct{ User struct{ Email string } }
	admin.get("/api/v1/viewer", &viewer)
	own := viewer.User.Email[strings.LastIndex(viewer.User.Email, "@")+1:]

	if code, body := admin.do("PATCH", "/api/v1/org", map[string]any{
		"sso": map[string]any{"allowedDomains": []string{"elsewhere.example"}},
	}); code != http.StatusBadRequest || !strings.Contains(string(body), own) {
		t.Fatalf("narrowing past your own domain = %d %s, want 400 naming %s", code, body, own)
	}
	if code, body := admin.do("PATCH", "/api/v1/org", map[string]any{
		"sso": map[string]any{"allowedDomains": []string{"not a domain"}},
	}); code != http.StatusBadRequest {
		t.Fatalf("a malformed domain = %d %s, want 400", code, body)
	}
	if code, body := member.do("PATCH", "/api/v1/org", map[string]any{
		"sso": map[string]any{"allowedDomains": []string{own}},
	}); code != http.StatusForbidden {
		t.Fatalf("member PATCH = %d %s, want 403", code, body)
	}

	var org struct {
		SSO struct{ AllowedDomains []string }
	}
	admin.post("PATCH", "/api/v1/org", map[string]any{
		"sso": map[string]any{"allowedDomains": []string{"@" + strings.ToUpper(own), "acme.example"}},
	}, &org)
	if strings.Join(org.SSO.AllowedDomains, ",") != own+",acme.example" {
		t.Fatalf("allowed domains = %q", org.SSO.AllowedDomains)
	}

	// The administrator's own sign-in still works through the real flow.
	ok := browserClient(t)
	resp, err := ok.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sign-in from an allowed domain = %d", resp.StatusCode)
	}

	// Move the list off the administrator's domain behind the console's back,
	// as a database edit or a second administrator elsewhere would: the next
	// sign-in from it is refused at the callback.
	o, _ := srv.Store().Org()
	o.AllowedDomains = []string{"acme.example"}
	if err := srv.Store().SaveOrg(o); err != nil {
		t.Fatal(err)
	}
	refused := browserClient(t)
	resp, err = refused.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sign-in from a domain no longer allowed = %d, want 403", resp.StatusCode)
	}
}

// TestTheTraceStoreMovesFromTheConsole: Settings → Traces saves a new
// location, held to what setup holds it to, and the next trace is written
// there without a restart.
func TestTheTraceStoreMovesFromTheConsole(t *testing.T) {
	srv, admin, _, _ := adminAndMember(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	o, _ := srv.store.Org()
	o.TraceKey = id.Recipient().String()
	if err := srv.store.SaveOrg(o); err != nil {
		t.Fatal(err)
	}

	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "  ", blocker} {
		if code, body := admin.do("PATCH", "/api/v1/org", map[string]any{
			"traceStore": map[string]any{"location": bad},
		}); code != http.StatusBadRequest {
			t.Fatalf("location %q = %d %s, want 400", bad, code, body)
		}
	}

	where := filepath.Join(t.TempDir(), "moved", "traces")
	var org struct {
		TraceStore struct{ Location string }
	}
	admin.post("PATCH", "/api/v1/org", map[string]any{
		"traceStore": map[string]any{"location": `"` + where + `"`},
	}, &org)
	if org.TraceStore.Location != where {
		t.Fatalf("location = %q, want %q", org.TraceStore.Location, where)
	}
	if st, err := os.Stat(where); err != nil || !st.IsDir() {
		t.Fatalf("the new location was not made: %v", err)
	}
	if srv.traceWriter() == nil || !strings.HasPrefix(srv.tracesFor, where+"\x00") {
		t.Fatalf("the trace writer did not move to %s", where)
	}
}
