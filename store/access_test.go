package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "john.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkUser(t *testing.T, s *Store, email string) *User {
	t.Helper()
	u, err := s.UpsertSSOUser("sub-"+email, email, email, "")
	if err != nil {
		t.Fatalf("UpsertSSOUser: %v", err)
	}
	return u
}

// --- tokens ---

func TestMachineTokenLifecycle(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	secret, err := s.CreateMachineToken(u.ID, "laptop", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UserForMachineToken(secret)
	if err != nil || got.ID != u.ID {
		t.Fatalf("resolve = %v, %v", got, err)
	}
	// A wrong secret resolves to nothing.
	if _, err := s.UserForMachineToken("not-a-token"); err != ErrNotFound {
		t.Fatalf("bad token = %v, want ErrNotFound", err)
	}
	// An expired token does not resolve.
	expired, _ := s.CreateMachineToken(u.ID, "laptop", -time.Minute)
	if _, err := s.UserForMachineToken(expired); err != ErrNotFound {
		t.Fatalf("expired token = %v, want ErrNotFound", err)
	}
}

func TestWebSessionLifecycle(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	secret, err := s.CreateWebSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.UserForWebSession(secret); err != nil || got.ID != u.ID {
		t.Fatalf("resolve = %v, %v", got, err)
	}
	if err := s.DeleteWebSession(secret); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForWebSession(secret); err != ErrNotFound {
		t.Fatalf("after delete = %v, want ErrNotFound", err)
	}
	expired, _ := s.CreateWebSession(u.ID, -time.Minute)
	if _, err := s.UserForWebSession(expired); err != ErrNotFound {
		t.Fatalf("expired session = %v, want ErrNotFound", err)
	}
}

// --- users ---

func TestUpsertSSOUserResolvesAndBootstrapsAdmin(t *testing.T) {
	s := openTestStore(t)

	// First sign-in whose email matches admin_email becomes an administrator.
	admin, err := s.UpsertSSOUser("sub-a", "boss@acme.example", "The Boss", "boss@acme.example")
	if err != nil {
		t.Fatal(err)
	}
	if admin.Role != "admin" {
		t.Fatalf("admin bootstrap role = %q", admin.Role)
	}
	// Anyone else is a member.
	member, _ := s.UpsertSSOUser("sub-b", "dev@acme.example", "Dev", "boss@acme.example")
	if member.Role != "member" {
		t.Fatalf("member role = %q", member.Role)
	}
	// Re-sign-in by subject returns the same row and refreshes activity.
	again, _ := s.UpsertSSOUser("sub-a", "boss@acme.example", "The Boss", "boss@acme.example")
	if again.ID != admin.ID {
		t.Fatalf("re-signin created a new user: %s != %s", again.ID, admin.ID)
	}
	// A provisioned user (created by email before their first IdP subject is
	// known) is matched by email and gets their subject stamped.
	byEmail, _ := s.UpsertSSOUser("", "boss@acme.example", "", "")
	if byEmail.ID != admin.ID {
		t.Fatalf("email match created a new user")
	}
}

// --- effective policy resolution ---

func TestEffectivePolicyResolution(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	// A baseline group (applies to everyone) and a team group (applies via
	// membership).
	s.CreatePolicyGroup("floor", "", true, u.ID) // baseline: applies to everyone
	teamGroup, _ := s.CreatePolicyGroup("payments", "", false, u.ID)
	team, _ := s.CreateTeam("Payments", "payments", "", []string{teamGroup.ID})
	if err := s.SetUserTeams(u.ID, []string{team.ID}); err != nil {
		t.Fatal(err)
	}

	// Two models; a routing policy in the team group assigns only one.
	lm, _ := json.Marshal(map[string]any{"id": "m-int", "api": "openai-chat"})
	s.CreateModel(&Model{ID: "m_internal", Name: "internal", ModelID: "m-int", Status: "enabled", LLMModel: string(lm)})
	s.CreateModel(&Model{ID: "m_other", Name: "other", ModelID: "m-oth", Status: "enabled", LLMModel: string(lm)})
	assign, _ := json.Marshal(map[string]any{"kind": "model-routing", "mode": "assign",
		"modelIds": []string{"m_internal"}, "keywords": []string{}})
	s.CreatePolicy(teamGroup.ID, "assign", "", assign, u.ID)

	groups, err := s.GroupsForUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, g := range groups {
		names[g.Name] = true
	}
	if !names["floor"] || !names["payments"] {
		t.Fatalf("groups for user = %+v, want floor + payments", names)
	}

	models, err := s.ModelsForUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "m_internal" {
		t.Fatalf("models for user = %+v, want just the assigned one", models)
	}

	// A user on no team sees only the baseline group and no assigned models.
	other := mkUser(t, s, "outsider@example.com")
	og, _ := s.GroupsForUser(other.ID)
	if len(og) != 1 || og[0].Name != "floor" {
		t.Fatalf("outsider groups = %+v, want only baseline", og)
	}
	om, _ := s.ModelsForUser(other.ID)
	if len(om) != 0 {
		t.Fatalf("outsider models = %+v, want none", om)
	}
}

// --- usage rollups ---

func TestUsageRollups(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")
	s.CreateModel(&Model{ID: "m1", Name: "m1", ModelID: "m1", Status: "enabled", LLMModel: `{"id":"m1"}`})
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		if err := s.AddUsage(now.AddDate(0, 0, -i), u.ID, "m1", UsageDelta{
			Requests: 10, TokensIn: 100, TokensOut: 50, CostUSD: 0.5,
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.AddUsage(now.AddDate(0, 0, -10), u.ID, "m1", UsageDelta{Requests: 5, CostUSD: 0.1}) // previous window

	days, totals, previous, byModel, _, byUser, err := s.UsageSummary(u.ID, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 7 {
		t.Fatalf("days = %d, want a continuous 7-day axis", len(days))
	}
	if totals.Requests != 30 {
		t.Fatalf("totals.requests = %d, want 30", totals.Requests)
	}
	if previous.Requests != 5 {
		t.Fatalf("previous.requests = %d, want 5", previous.Requests)
	}
	if len(byModel) != 1 || byModel[0].ModelID != "m1" {
		t.Fatalf("byModel = %+v", byModel)
	}
	// byUser is only populated for org scope (empty userID).
	_, _, _, _, _, orgByUser, _ := s.UsageSummary("", 7, now)
	if len(orgByUser) != 1 || orgByUser[0].ID != u.ID {
		t.Fatalf("org byUser = %+v", orgByUser)
	}
	_ = byUser
}

// --- access requests ---

func TestAccessRequestLifecycle(t *testing.T) {
	s := openTestStore(t)
	requester := mkUser(t, s, "dev@example.com")
	spec := json.RawMessage(`{"kind":"network","host":"registry.npmjs.org","decision":"always-allow"}`)

	r, err := s.CreateAccessRequest(requester.ID, "", "npm", "need deps", "grp", nil, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Ref == "" || r.Status != "pending" {
		t.Fatalf("created = %+v", r)
	}
	// Ref allocation increments.
	r2, _ := s.CreateAccessRequest(requester.ID, "", "npm2", "more", "grp", nil, spec, nil)
	if r2.Ref == r.Ref {
		t.Fatalf("refs collided: %s", r.Ref)
	}

	// Look up by id and by ref.
	if got, err := s.AccessRequestByIDOrRef(r.Ref); err != nil || got.ID != r.ID {
		t.Fatalf("by ref = %v, %v", got, err)
	}

	// Open filter returns pending ones.
	open, _ := s.AccessRequests("open", "", "", "")
	if len(open) != 2 {
		t.Fatalf("open = %d, want 2", len(open))
	}

	// Decide one; a decided request refuses re-deciding.
	decider := mkUser(t, s, "admin@example.com")
	decided, err := s.DecideAccessRequest(r.ID, "approved", decider.ID, nil, nil, "")
	if err != nil || decided.Status != "approved" {
		t.Fatalf("decide = %v, %v", decided, err)
	}
	if _, err := s.DecideAccessRequest(r.ID, "rejected", decider.ID, nil, nil, ""); err == nil {
		t.Fatal("re-deciding a decided request must fail")
	}
}

// --- change history ---

func TestChangeHistoryIsAppendOnlyAndFiltered(t *testing.T) {
	s := openTestStore(t)
	actor := mkUser(t, s, "admin@example.com")
	v := 1
	s.RecordChange(actor.ID, "policy", "created", "p1", "allow npm", "Added network policy allow npm.", &v)
	s.RecordChange(actor.ID, "team", "created", "t1", "Payments", "Created team Payments.", nil)

	all, err := s.Changes("", "", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("changes = %d, %v", len(all), err)
	}
	// Newest first.
	if all[0].Entity != "team" {
		t.Fatalf("expected newest-first ordering, got %s first", all[0].Entity)
	}
	// Entity filter.
	pol, _ := s.Changes("policy", "", 0)
	if len(pol) != 1 || pol[0].EntityName != "allow npm" {
		t.Fatalf("policy filter = %+v", pol)
	}
	// Search filter.
	found, _ := s.Changes("", "Payments", 0)
	if len(found) != 1 || found[0].Entity != "team" {
		t.Fatalf("search filter = %+v", found)
	}
}
