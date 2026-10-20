package backend

import (
	"bytes"
	"encoding/json"

	"io"
	"net/http"
	"testing"
	"time"

	"github.com/popstarapps/john/store"
)

// apiClient calls the API as one identity: a browser cookie jar or a Bearer
// machine token.
type apiClient struct {
	t     *testing.T
	base  string
	http  *http.Client
	token string
}

func (c *apiClient) do(method, path string, in any) (int, []byte) {
	c.t.Helper()
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	cl := c.http
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (c *apiClient) get(path string, out any) {
	c.t.Helper()
	code, data := c.do("GET", path, nil)
	if code != http.StatusOK {
		c.t.Fatalf("GET %s = %d: %s", path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("GET %s: decode: %v\n%s", path, err, data)
		}
	}
}

func (c *apiClient) post(method, path string, in, out any) {
	c.t.Helper()
	code, data := c.do(method, path, in)
	if code != http.StatusOK {
		c.t.Fatalf("%s %s = %d: %s", method, path, code, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decode: %v\n%s", method, path, err, data)
		}
	}
}

// TestConsoleAPIScenario walks the whole admin-and-member story through the
// real HTTP surface: an admin builds teams, groups, policies and models; a
// member sees exactly their effective slice, files an access request; the
// admin approves it and the policy plus the change history fall out.
func TestConsoleAPIScenario(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	// The admin signs in through the real browser flow.
	browser := browserClient(t)
	resp, err := browser.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin := &apiClient{t: t, base: base, http: browser}

	// A member exists via the store (their IdP sign-in is covered elsewhere)
	// and calls the API with a machine token, like a CLI would.
	memberUser, err := srv.Store().UpsertSSOUser("dev-2", "casey@example.com", "Casey Lee", "")
	if err != nil {
		t.Fatal(err)
	}
	memberToken, err := srv.Store().CreateMachineToken(memberUser.ID, "laptop", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	member := &apiClient{t: t, base: base, token: memberToken}

	// --- teams, groups, policies, models ---

	var group struct{ ID, Name string }
	admin.post("POST", "/api/v1/groups", map[string]any{
		"name": "payments-restricted", "description": "payments controls", "baseline": false,
	}, &group)

	var baseline struct{ ID string }
	admin.post("POST", "/api/v1/groups", map[string]any{
		"name": "org-floor", "description": "the floor", "baseline": true,
	}, &baseline)

	var team struct{ ID, Slug string }
	admin.post("POST", "/api/v1/teams", map[string]any{
		"name": "Payments", "slug": "payments", "description": "", "groupIds": []string{group.ID},
	}, &team)

	admin.post("PUT", "/api/v1/users/"+memberUser.ID+"/teams",
		map[string]any{"teamIds": []string{team.ID}}, nil)

	var model struct{ ID string }
	admin.post("POST", "/api/v1/models", map[string]any{
		"name": "internal/codegen-l", "provider": "selfhosted", "hosting": "internal",
		"endpoint": "https://llm.internal/v1", "modelId": "codegen-l", "contextWindow": 128000,
	}, &model)

	var policy struct{ ID string }
	admin.post("POST", "/api/v1/policies", map[string]any{
		"groupId": group.ID, "name": "assign internal model", "description": "",
		"spec": map[string]any{"kind": "model-routing", "mode": "assign",
			"modelIds": []string{model.ID}, "keywords": []string{}},
	}, &policy)

	admin.post("POST", "/api/v1/policies", map[string]any{
		"groupId": group.ID, "name": "allow proxy.golang.org", "description": "",
		"spec": map[string]any{"kind": "network", "methods": []string{"GET"}, "scheme": "https",
			"host": "proxy.golang.org", "port": nil, "path": "/*",
			"headerMatches": []any{}, "queryMatches": []any{},
			"decision": "always-allow", "maxBodyBytes": nil},
	}, nil)

	// --- the member's view ---

	var myGroups []struct {
		ID       string
		Baseline bool
	}
	member.get("/api/v1/users/"+memberUser.ID+"/groups", &myGroups)
	if len(myGroups) != 2 {
		t.Fatalf("member groups = %d, want baseline + team group", len(myGroups))
	}

	var myModels []struct{ ID string }
	member.get("/api/v1/users/"+memberUser.ID+"/models", &myModels)
	if len(myModels) != 1 || myModels[0].ID != model.ID {
		t.Fatalf("member models = %+v", myModels)
	}

	var myPolicies []struct {
		ID    string
		Group struct{ Name string }
	}
	member.get("/api/v1/users/"+memberUser.ID+"/policies", &myPolicies)
	if len(myPolicies) != 2 {
		t.Fatalf("member effective policies = %d, want 2", len(myPolicies))
	}
	if myPolicies[0].Group.Name == "" {
		t.Fatal("effective policies must embed their group")
	}

	// Admin surfaces answer 403 to a member, whatever the console renders.
	for _, path := range []string{"/api/v1/changes", "/api/v1/groups", "/api/v1/users", "/api/v1/models"} {
		if code, _ := member.do("GET", path, nil); code != http.StatusForbidden {
			t.Fatalf("member GET %s = %d, want 403", path, code)
		}
	}
	if code, _ := member.do("GET", "/api/v1/usage?scope=org", nil); code != http.StatusOK {
		t.Fatal("usage must answer a member")
	}

	// --- the access request story ---

	spec := map[string]any{"kind": "network", "methods": []string{"GET"}, "scheme": "https",
		"host": "registry.npmjs.org", "port": nil, "path": "/*",
		"headerMatches": []any{}, "queryMatches": []any{},
		"decision": "always-allow", "maxBodyBytes": nil}

	var filed struct{ ID, Ref, Status string }
	member.post("POST", "/api/v1/access-requests", map[string]any{
		"title": "npm registry, read-only", "justification": "installing dependencies for the payments UI build",
		"teamId": team.ID, "groupId": nil, "groupName": "payments-restricted", "spec": spec,
	}, &filed)
	if filed.Status != "pending" || filed.Ref == "" {
		t.Fatalf("filed = %+v", filed)
	}

	// The member sees it; another member would not. The admin queue sees it.
	var open []struct{ ID string }
	member.get("/api/v1/access-requests?status=open", &open)
	if len(open) != 1 {
		t.Fatalf("member open requests = %d", len(open))
	}
	admin.get("/api/v1/access-requests?status=open", &open)
	if len(open) != 1 {
		t.Fatalf("admin open requests = %d", len(open))
	}

	var decided struct {
		Status   string
		Proposed struct{ GroupID *string }
	}
	admin.post("POST", "/api/v1/access-requests/"+filed.ID+"/decision", map[string]any{
		"status": "approved", "groupId": group.ID,
	}, &decided)
	if decided.Status != "approved" || decided.Proposed.GroupID == nil || *decided.Proposed.GroupID != group.ID {
		t.Fatalf("decided = %+v", decided)
	}

	// Approving wrote the policy into the group…
	var groupPolicies []struct{ Name, Type string }
	admin.get("/api/v1/policies?groupId="+group.ID, &groupPolicies)
	if len(groupPolicies) != 3 {
		t.Fatalf("group policies after approval = %d, want 3", len(groupPolicies))
	}

	// …and the whole story is in the change history, most recent first.
	var changes []struct{ Entity, Action string }
	admin.get("/api/v1/changes", &changes)
	wantActions := map[string]bool{}
	for _, c := range changes {
		wantActions[c.Entity+"/"+c.Action] = true
	}
	for _, want := range []string{"group/created", "team/created", "member/updated",
		"model/created", "policy/created", "request/approved"} {
		if !wantActions[want] {
			t.Fatalf("change history is missing %s; have %+v", want, changes)
		}
	}

	// A decision is final.
	if code, _ := admin.do("POST", "/api/v1/access-requests/"+filed.ID+"/decision",
		map[string]any{"status": "rejected"}); code != http.StatusBadRequest {
		t.Fatal("re-deciding a decided request must fail")
	}
}

func TestUsageSummaryRoundTrip(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	browser := browserClient(t)
	resp, err := browser.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin := &apiClient{t: t, base: base, http: browser}

	var viewer struct {
		User struct{ ID string }
	}
	admin.get("/api/v1/viewer", &viewer)

	var model struct{ ID string }
	admin.post("POST", "/api/v1/models", map[string]any{
		"name": "Claude", "provider": "anthropic", "hosting": "external", "modelId": "claude-opus-5",
	}, &model)

	st := srv.Store()
	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := st.AddUsage(now.AddDate(0, 0, -i), viewer.User.ID, model.ID, store.UsageDelta{
			Requests: 10, TokensIn: 1000, TokensOut: 500, Sessions: 1, CostUSD: 0.25,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Something in the previous window too.
	st.AddUsage(now.AddDate(0, 0, -10), viewer.User.ID, model.ID, store.UsageDelta{Requests: 5, CostUSD: 0.1})

	var usage struct {
		Days []struct {
			Date    string
			ByModel []struct{ ModelID string }
		}
		Totals   struct{ Requests int }
		Previous struct{ Requests int }
		ByModel  []struct {
			Model   struct{ ID string }
			Tokens  int
			CostUsd float64
		}
		ByUser []struct {
			User struct{ ID string }
		}
	}
	admin.get("/api/v1/usage?days=7&scope=org", &usage)
	if len(usage.Days) != 7 {
		t.Fatalf("days = %d, want a continuous 7-day axis", len(usage.Days))
	}
	if usage.Totals.Requests != 30 {
		t.Fatalf("totals.requests = %d, want 30", usage.Totals.Requests)
	}
	if usage.Previous.Requests != 5 {
		t.Fatalf("previous.requests = %d, want 5", usage.Previous.Requests)
	}
	if len(usage.ByModel) != 1 || usage.ByModel[0].Model.ID != model.ID || usage.ByModel[0].Tokens != 4500 {
		t.Fatalf("byModel = %+v", usage.ByModel)
	}
	if len(usage.ByUser) != 1 || usage.ByUser[0].User.ID != viewer.User.ID {
		t.Fatalf("byUser = %+v", usage.ByUser)
	}

}

// An active policy can be edited in place: the edit lands, bumps the version,
// is recorded, and cannot silently overwrite someone else's or change kind.
func TestEditingAnActivePolicy(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	browser := browserClient(t)
	resp, err := browser.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin := &apiClient{t: t, base: base, http: browser}

	var group struct{ ID string }
	admin.post("POST", "/api/v1/groups", map[string]any{"name": "g", "description": "", "baseline": true}, &group)

	network := func(methods ...string) map[string]any {
		return map[string]any{"kind": "network", "methods": methods, "scheme": "https",
			"host": "proxy.golang.org", "port": 443, "path": "/*",
			"headerMatches": []any{}, "queryMatches": []any{},
			"decision": "always-allow", "maxBodyBytes": 0}
	}
	type policy struct {
		ID      string
		Name    string
		Version int
		Spec    struct{ Methods []string }
	}
	var p policy
	admin.post("POST", "/api/v1/policies", map[string]any{
		"groupId": group.ID, "name": "go proxy", "description": "", "spec": network("GET"),
	}, &p)

	epoch, _ := srv.Store().PolicyEpoch()
	var edited policy
	admin.post("PUT", "/api/v1/policies/"+p.ID, map[string]any{
		"name": "go proxy (read)", "description": "module downloads", "spec": network("GET", "HEAD"),
		"version": p.Version,
	}, &edited)
	if edited.Name != "go proxy (read)" || len(edited.Spec.Methods) != 2 || edited.Version != p.Version+1 {
		t.Fatalf("edited = %+v, want the new name, GET+HEAD and v%d", edited, p.Version+1)
	}
	if after, _ := srv.Store().PolicyEpoch(); after == epoch {
		t.Fatal("editing a policy did not tell running sessions")
	}

	// Saved from the version before: someone else got there first.
	if code, _ := admin.do("PUT", "/api/v1/policies/"+p.ID, map[string]any{
		"name": "stale", "description": "", "spec": network("GET"), "version": p.Version,
	}); code != http.StatusConflict {
		t.Fatalf("stale edit = %d, want 409", code)
	}
	// A spec of another kind is a different policy.
	if code, _ := admin.do("PUT", "/api/v1/policies/"+p.ID, map[string]any{
		"name": "x", "description": "", "spec": map[string]any{"kind": "workspace", "paths": []string{"a"}, "mode": "exclude"},
	}); code != http.StatusBadRequest {
		t.Fatalf("kind change = %d, want 400", code)
	}
	if code, _ := admin.do("PUT", "/api/v1/policies/p-nope", map[string]any{
		"name": "x", "description": "", "spec": network("GET"),
	}); code != http.StatusNotFound {
		t.Fatalf("missing policy = %d, want 404", code)
	}

	// And it can be deleted: gone from the group, sessions told, history kept.
	epoch, _ = srv.Store().PolicyEpoch()
	admin.post("DELETE", "/api/v1/policies/"+p.ID, nil, nil)
	var left []policy
	admin.get("/api/v1/policies?groupId="+group.ID, &left)
	if len(left) != 0 {
		t.Fatalf("policies after delete = %+v, want none", left)
	}
	if after, _ := srv.Store().PolicyEpoch(); after == epoch {
		t.Fatal("deleting a policy did not tell running sessions")
	}
	if code, _ := admin.do("DELETE", "/api/v1/policies/"+p.ID, nil); code != http.StatusNotFound {
		t.Fatalf("deleting it again = %d, want 404", code)
	}
}
