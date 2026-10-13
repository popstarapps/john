package store

import (
	"encoding/json"

	"github.com/oliverkofoed/gokit/agentkit/llm"
	"testing"
	"time"
)

// --- people ---

func TestUserRoleMovesBothWays(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")
	if u.Role != "member" {
		t.Fatalf("new user role = %q, want member", u.Role)
	}

	promoted, err := s.SetUserRole(u.ID, "admin")
	if err != nil || promoted.Role != "admin" {
		t.Fatalf("promote = %+v, %v", promoted, err)
	}
	demoted, err := s.SetUserRole(u.ID, "member")
	if err != nil || demoted.Role != "member" {
		t.Fatalf("demote = %+v, %v", demoted, err)
	}

	// A role the schema would refuse is refused before it reaches the schema.
	if _, err := s.SetUserRole(u.ID, "owner"); err == nil {
		t.Fatal("an unknown role must be refused")
	}
	if _, err := s.SetUserRole("u_nobody", "admin"); err != ErrNotFound {
		t.Fatalf("unknown user = %v, want ErrNotFound", err)
	}
}

// Suspension is the offboarding path, so it has to actually stop the machines
// the person is signed in on — not merely stop new sign-ins.
func TestSuspendingStopsEverySignedInMachineAtOnce(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	access, err := s.CreateMachineToken(u.ID, "laptop", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := s.StartMachineSession(u.ID, "desktop", MachineTTL{Access: time.Hour, Refresh: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	web, err := s.CreateWebSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	suspended, err := s.SetUserStatus(u.ID, "suspended")
	if err != nil || suspended.Status != "suspended" {
		t.Fatalf("suspend = %+v, %v", suspended, err)
	}

	if _, err := s.UserForMachineToken(access); err != ErrNotFound {
		t.Fatalf("a suspended person's access token still resolves: %v", err)
	}
	if _, err := s.UserForMachineToken(creds.Access); err != ErrNotFound {
		t.Fatalf("the machine session's access token still resolves: %v", err)
	}
	// The refresh token is the one that would quietly mint new access tokens
	// for the next ninety days, so it is the one that matters most.
	if _, _, err := s.RotateMachineSession(creds.Refresh, "desktop",
		MachineTTL{Access: time.Hour, Refresh: 24 * time.Hour}); err == nil {
		t.Fatal("a suspended person's machine could still refresh")
	}
	if _, err := s.UserForWebSession(web); err != ErrNotFound {
		t.Fatalf("the browser session survived suspension: %v", err)
	}

	// Reactivating does not hand the old credentials back: they were revoked,
	// not paused, so the person signs in again.
	if _, err := s.SetUserStatus(u.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForMachineToken(access); err != ErrNotFound {
		t.Fatal("reactivating must not resurrect a revoked token")
	}

	if _, err := s.SetUserStatus(u.ID, "on-leave"); err == nil {
		t.Fatal("an unknown status must be refused")
	}
}

// --- teams ---

func TestDeletingATeamTakesItsMembershipAndGroupsWithIt(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")

	g, err := s.CreatePolicyGroup("payments", "", false, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam("Payments", "payments", "", []string{g.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserTeams(u.ID, []string{team.ID}); err != nil {
		t.Fatal(err)
	}
	groups, err := s.GroupsForUser(u.ID)
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups before delete = %d, %v, want 1", len(groups), err)
	}

	before, _ := s.PolicyEpoch()
	if err := s.DeleteTeam(team.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.TeamByIDOrSlug(team.ID); err != ErrNotFound {
		t.Fatalf("team survived delete: %v", err)
	}
	// The join rows are gone too, so the group stops applying to the people
	// who were in it — which is exactly the part a client has to be told
	// about, hence the epoch.
	teams, err := s.TeamIDsForUser(u.ID)
	if err != nil || len(teams) != 0 {
		t.Fatalf("membership after delete = %v, %v", teams, err)
	}
	groups, err = s.GroupsForUser(u.ID)
	if err != nil || len(groups) != 0 {
		t.Fatalf("groups after delete = %d, %v, want 0", len(groups), err)
	}
	if g, err := s.PolicyGroupByID(g.ID); err != nil || len(g.TeamIDs) != 0 {
		t.Fatalf("group still attached to the deleted team: %+v, %v", g, err)
	}
	if after, _ := s.PolicyEpoch(); after <= before {
		t.Fatalf("policy epoch %d did not move past %d", after, before)
	}

	if err := s.DeleteTeam(team.ID); err != ErrNotFound {
		t.Fatalf("deleting twice = %v, want ErrNotFound", err)
	}
}

// --- models ---

func TestUpdatingAModelRebuildsWhatTheBackendCallsWith(t *testing.T) {
	s := openTestStore(t)
	m := &Model{
		Name: "Claude", Provider: "anthropic", Hosting: "external",
		Endpoint: "https://api.anthropic.com", ModelID: "claude-opus-5",
		ContextWindow: 200000, CostIn: 5, CostOut: 25, APIKey: "sk-original",
	}
	if err := s.CreateModel(m); err != nil {
		t.Fatal(err)
	}

	// An administrator corrects the endpoint and the price, and says nothing
	// about the credential.
	if err := s.UpdateModel(&Model{
		ID: m.ID, Name: "Claude Opus", Provider: "anthropic", Hosting: "external",
		Endpoint: "https://anthropic.internal.example/", ModelID: "claude-opus-5",
		ContextWindow: 1000000, CostIn: 4, CostCachedIn: 0.4, CostOut: 20,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ModelByID(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Claude Opus" || got.ContextWindow != 1000000 || got.CostOut != 20 {
		t.Fatalf("stored model = %+v", got)
	}
	if got.APIKey != "sk-original" {
		t.Fatalf("api key = %q, want the stored one kept", got.APIKey)
	}
	if got.Status != "enabled" || got.AddedAt.IsZero() {
		t.Fatalf("update must not disturb status or added_at: %+v", got)
	}
	// The derived config is what a request actually uses, so it has to have
	// moved with the fields — otherwise the console shows one endpoint and
	// the backend calls another.
	lm, err := got.LLM()
	if err != nil {
		t.Fatal(err)
	}
	if lm.BaseURL != "https://anthropic.internal.example" {
		t.Fatalf("llm base url = %q, still the old endpoint", lm.BaseURL)
	}
	if lm.ContextWindow != 1000000 || lm.Cost.Output != 20 {
		t.Fatalf("llm model did not follow the edit: %+v", lm)
	}

	// A key that is given replaces the one that was there.
	if err := s.UpdateModel(&Model{
		ID: m.ID, Name: "Claude Opus", Provider: "anthropic", Endpoint: "https://x.example",
		ModelID: "claude-opus-5", APIKey: "sk-rotated",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ModelByID(m.ID); got.APIKey != "sk-rotated" {
		t.Fatalf("api key = %q, want the rotated one", got.APIKey)
	}

	if err := s.UpdateModel(&Model{ID: "m_nobody", Name: "x"}); err != ErrNotFound {
		t.Fatalf("updating an unknown model = %v, want ErrNotFound", err)
	}
}

// The preset chooses the wire protocol, so a spelling of it that does not
// match is not a cosmetic problem: an Anthropic model reached over the
// OpenAI chat API fails every call. Matching is loose on purpose.
func TestTheProviderPresetPicksTheAPIWhateverItsSpelling(t *testing.T) {
	for _, spelling := range []string{"anthropic", "Anthropic", " ANTHROPIC "} {
		m := BuildLLMModel(spelling, "", "claude-opus-5", "Claude", 5, 0.5, 25, 1000)
		if m.API != llm.AnthropicMessages {
			t.Fatalf("preset %q chose API %v, want the Anthropic one", spelling, m.API)
		}
		if m.BaseURL != "https://api.anthropic.com" {
			t.Fatalf("preset %q defaulted to %q", spelling, m.BaseURL)
		}
	}
	if m := BuildLLMModel("OpenAI", "", "gpt-5.1", "GPT", 0, 0, 0, 0); m.API != llm.OpenAIResponses {
		t.Fatalf("OpenAI chose API %v", m.API)
	}
	// Anything unrecognised still builds, on the most widely implemented API.
	if m := BuildLLMModel("something-new", "https://x.example", "m", "M", 0, 0, 0, 0); m.API != llm.OpenAIChat {
		t.Fatalf("an unknown preset chose API %v", m.API)
	}
}

func TestModelDeletionAndWhatStillPointsAtIt(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "admin@example.com")

	keep := &Model{Name: "Keep", Provider: "anthropic", ModelID: "keep"}
	drop := &Model{Name: "Drop", Provider: "anthropic", ModelID: "drop"}
	for _, m := range []*Model{keep, drop} {
		if err := s.CreateModel(m); err != nil {
			t.Fatal(err)
		}
	}

	g, err := s.CreatePolicyGroup("routing", "", false, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(map[string]any{
		"kind": "model-routing", "mode": "restrict",
		"modelIds": []string{keep.ID}, "keywords": []string{},
	})
	if _, err := s.CreatePolicy(g.ID, "only the safe one", "", spec, u.ID); err != nil {
		t.Fatal(err)
	}

	using, err := s.PoliciesReferencingModel(keep.ID)
	if err != nil || len(using) != 1 {
		t.Fatalf("policies naming the kept model = %d, %v, want 1", len(using), err)
	}
	using, err = s.PoliciesReferencingModel(drop.ID)
	if err != nil || len(using) != 0 {
		t.Fatalf("policies naming the dropped model = %d, %v, want 0", len(using), err)
	}

	before, _ := s.PolicyEpoch()
	if err := s.DeleteModel(drop.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ModelByID(drop.ID); err != ErrNotFound {
		t.Fatalf("model survived delete: %v", err)
	}
	if after, _ := s.PolicyEpoch(); after <= before {
		t.Fatalf("policy epoch %d did not move past %d", after, before)
	}
	if err := s.DeleteModel(drop.ID); err != ErrNotFound {
		t.Fatalf("deleting twice = %v, want ErrNotFound", err)
	}
}

// A day's spend happened whether or not the model is still configured, so
// deleting one must not quietly change what the usage page adds up to.
func TestDeletingAModelLeavesItsUsageCounted(t *testing.T) {
	s := openTestStore(t)
	u := mkUser(t, s, "dev@example.com")
	m := &Model{Name: "Gone", Provider: "anthropic", ModelID: "gone"}
	if err := s.CreateModel(m); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.AddUsage(now, u.ID, m.ID, UsageDelta{Requests: 7, TokensIn: 100, CostUSD: 1.25}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(m.ID); err != nil {
		t.Fatal(err)
	}
	_, totals, _, byModel, _, _, err := s.UsageSummary("", 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 7 || totals.CostUSD != 1.25 {
		t.Fatalf("totals after deleting the model = %+v", totals)
	}
	if len(byModel) != 1 || byModel[0].ModelID != m.ID {
		t.Fatalf("the rows keep their model id: %+v", byModel)
	}
}

// --- the conversation on a request ---

func mkRequest(t *testing.T, s *Store, requesterID string) *AccessRequest {
	t.Helper()
	spec, _ := json.Marshal(map[string]any{
		"kind": "network", "methods": []string{"GET"}, "scheme": "https",
		"host": "registry.npmjs.org", "port": nil, "path": "/*",
		"headerMatches": []any{}, "queryMatches": []any{},
		"decision": "always-allow", "maxBodyBytes": nil,
	})
	r, err := s.CreateAccessRequest(requesterID, "", "npm", "installing deps", "", nil, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRequestCommentsAreAppendOnlyAndOrdered(t *testing.T) {
	s := openTestStore(t)
	dev := mkUser(t, s, "dev@example.com")
	admin := mkUser(t, s, "admin@example.com")
	req := mkRequest(t, s, dev.ID)

	if cs, err := s.RequestComments(req.ID); err != nil || len(cs) != 0 {
		t.Fatalf("a fresh request = %d comments, %v, want none", len(cs), err)
	}
	if _, err := s.AddRequestComment(req.ID, admin.ID, "   "); err == nil {
		t.Fatal("an empty comment must be refused")
	}
	if _, err := s.AddRequestComment("r_nobody", admin.ID, "hello"); err != ErrNotFound {
		t.Fatal("commenting on a request that does not exist must fail")
	}

	first, err := s.AddRequestComment(req.ID, admin.ID, "  Which build needs this?  ")
	if err != nil {
		t.Fatal(err)
	}
	if first.Body != "Which build needs this?" {
		t.Fatalf("body = %q, want it trimmed", first.Body)
	}
	if _, err := s.AddRequestComment(req.ID, dev.ID, "The payments UI build."); err != nil {
		t.Fatal(err)
	}

	cs, err := s.RequestComments(req.ID)
	if err != nil || len(cs) != 2 {
		t.Fatalf("comments = %d, %v, want 2", len(cs), err)
	}
	if cs[0].AuthorID != admin.ID || cs[1].AuthorID != dev.ID {
		t.Fatalf("comments are out of order: %+v", cs)
	}
	if cs[0].At.IsZero() {
		t.Fatal("a comment must carry when it was said")
	}
	// A second request's thread is its own.
	other := mkRequest(t, s, dev.ID)
	if cs, err := s.RequestComments(other.ID); err != nil || len(cs) != 0 {
		t.Fatalf("another request's thread = %d, %v", len(cs), err)
	}
}

func TestNeedsInfoIsAMoveBetweenOpenStatusesOnly(t *testing.T) {
	s := openTestStore(t)
	dev := mkUser(t, s, "dev@example.com")
	admin := mkUser(t, s, "admin@example.com")
	req := mkRequest(t, s, dev.ID)

	asked, err := s.SetAccessRequestOpenStatus(req.ID, "needs-info")
	if err != nil || asked.Status != "needs-info" {
		t.Fatalf("ask = %+v, %v", asked, err)
	}
	// Asking a question is not a decision: nothing was decided, so nothing
	// records a decider.
	if asked.DecidedAt != nil || asked.DecidedByID != nil {
		t.Fatalf("needs-info wrote a decision: %+v", asked)
	}
	// Idempotent, so a second reviewer asking again is not an error.
	if again, err := s.SetAccessRequestOpenStatus(req.ID, "needs-info"); err != nil || again.Status != "needs-info" {
		t.Fatalf("asking twice = %+v, %v", again, err)
	}

	back, err := s.SetAccessRequestOpenStatus(req.ID, "pending")
	if err != nil || back.Status != "pending" {
		t.Fatalf("answer = %+v, %v", back, err)
	}

	if _, err := s.SetAccessRequestOpenStatus(req.ID, "approved"); err == nil {
		t.Fatal("approving is a decision, not an open status")
	}

	// Once decided, it cannot be dragged back into the queue.
	if _, err := s.DecideAccessRequest(req.ID, "rejected", admin.ID, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAccessRequestOpenStatus(req.ID, "needs-info"); err == nil {
		t.Fatal("a rejected request must not reopen as needs-info")
	}
}
