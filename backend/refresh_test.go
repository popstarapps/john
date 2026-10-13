package backend

import (
	"net/http"
	"testing"
	"time"
)

// creds is what both the end of a sign-in and every refresh answer with.
type creds struct {
	Token            string
	ExpiresAt        time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	User             struct{ Email string }
}

// signIn runs the CLI login the whole way through and returns the pair.
func signIn(t *testing.T, base string) creds {
	t.Helper()
	var start struct{ Code, URL string }
	postJSON(t, http.DefaultClient, base+"/api/v1/auth/cli/start", map[string]string{}, &start)

	browser := browserClient(t)
	resp, err := browser.Get(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var out creds
	code := postJSONStatus(t, base+"/api/v1/auth/cli/poll",
		map[string]string{"code": start.Code, "host": "laptop"}, &out)
	if code != http.StatusOK || out.Token == "" {
		t.Fatalf("poll = %d %+v", code, out)
	}
	return out
}

func viewerStatus(t *testing.T, base, token string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/v1/viewer", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The whole point of the pair, over real HTTP: a machine keeps working
// without going near the identity provider again, and the credential it
// carries on every call is one that expires in hours.
func TestAMachineRenewsItselfWithoutSigningInAgain(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	first := signIn(t, base)
	if first.RefreshToken == "" {
		t.Fatal("sign-in handed out no refresh token")
	}
	if d := time.Until(first.ExpiresAt); d > 24*time.Hour {
		t.Fatalf("access token lives %v — the pair exists so this is short", d)
	}
	if !first.RefreshExpiresAt.After(first.ExpiresAt) {
		t.Fatal("the refresh token does not outlive the access token")
	}

	var second creds
	code := postJSONStatus(t, base+"/api/v1/auth/refresh",
		map[string]string{"refreshToken": first.RefreshToken, "host": "laptop"}, &second)
	if code != http.StatusOK {
		t.Fatalf("refresh = %d", code)
	}
	if second.Token == first.Token || second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh handed back a credential it was given")
	}
	if second.User.Email != "oliver@example.com" {
		t.Fatalf("refresh identified %q", second.User.Email)
	}
	if got := viewerStatus(t, base, second.Token); got != http.StatusOK {
		t.Fatalf("renewed access token = %d", got)
	}
}

// Refreshing must not require a live access token: the moment it is most
// needed is exactly the moment the access token has expired. Nothing in this
// test ever sends an Authorization header — postJSONStatus does not set one —
// which is precisely the property being asserted.
func TestRefreshingNeedsNoAccessToken(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	first := signIn(t, base)

	var next creds
	code := postJSONStatus(t, base+"/api/v1/auth/refresh",
		map[string]string{"refreshToken": first.RefreshToken}, &next)
	if code != http.StatusOK {
		t.Fatalf("unauthenticated refresh = %d, want 200", code)
	}
	if got := viewerStatus(t, base, next.Token); got != http.StatusOK {
		t.Fatalf("token from that refresh = %d", got)
	}
}

// Every dead end answers the same way, because the client's response to all
// of them is the same: sign in again.
func TestADeadRefreshTokenSaysSignInAgain(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	var out struct{ Error string }
	code := postJSONStatus(t, base+"/api/v1/auth/refresh",
		map[string]string{"refreshToken": "not-a-token"}, &out)
	if code != http.StatusUnauthorized {
		t.Fatalf("unknown refresh token = %d, want 401", code)
	}
	if out.Error != "sign_in_required" {
		t.Fatalf("unknown refresh token said %q", out.Error)
	}

	// And so does a revoked one. This is the offboarding path: revoking a
	// person's machine sessions has to stop the machines, not merely stop
	// them from being renewed some hours later.
	first := signIn(t, base)
	user, err := srv.Store().UserByEmail("oliver@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store().RevokeMachineSessionsFor(user.ID); err != nil {
		t.Fatal(err)
	}
	if got := viewerStatus(t, base, first.Token); got != http.StatusUnauthorized {
		t.Fatalf("access token after revocation = %d, want 401", got)
	}
	code = postJSONStatus(t, base+"/api/v1/auth/refresh",
		map[string]string{"refreshToken": first.RefreshToken}, &out)
	if code != http.StatusUnauthorized || out.Error != "sign_in_required" {
		t.Fatalf("revoked refresh token = %d %q, want 401 sign_in_required", code, out.Error)
	}
}

// The organization's cap is what eventually sends someone back to the
// identity provider. What is asserted here is that the setting reaches the
// credential: a machine is never handed a refresh token that would still be
// alive after the cap has passed. What happens at the boundary itself is
// store's to prove, where a chain can be aged without waiting a month.
func TestTheOrganizationsCapBoundsTheRefreshToken(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	org, err := srv.Store().Org()
	if err != nil {
		t.Fatal(err)
	}
	org.MachineSessionMaxDays = 1
	if err := srv.Store().SaveOrg(org); err != nil {
		t.Fatal(err)
	}

	capped := signIn(t, base)
	if d := time.Until(capped.RefreshExpiresAt); d > 25*time.Hour {
		t.Fatalf("refresh token lives %v under a one-day cap", d)
	}

	// Zero is not a very short cap, it is no cap: standalone relies on the
	// difference, having no identity provider to send anyone back to.
	org.MachineSessionMaxDays = 0
	if err := srv.Store().SaveOrg(org); err != nil {
		t.Fatal(err)
	}
	uncapped := signIn(t, base)
	if d := time.Until(uncapped.RefreshExpiresAt); d < 25*time.Hour {
		t.Fatalf("refresh token lives %v with the cap turned off", d)
	}
}

// The cap is an organization setting, which means an administrator has to be
// able to change it and see it. Neither the read nor the write was covered by
// anything above: those exercise the cap by writing the org row directly,
// which is not how anyone actually changes it.
func TestAnAdministratorCanChangeTheMachineSessionCap(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	browser := browserClient(t)
	resp, err := browser.Get(base + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	admin := &apiClient{t: t, base: base, http: browser}

	type orgJSONShape struct {
		MachineSessions struct {
			MaxDays int `json:"maxDays"`
		} `json:"machineSessions"`
	}

	// It is readable, so the console can show what is in force.
	var before orgJSONShape
	admin.get("/api/v1/org", &before)
	if before.MachineSessions.MaxDays != 0 {
		t.Fatalf("this org started at %d days", before.MachineSessions.MaxDays)
	}

	// It is writable, and the write sticks.
	admin.post("PATCH", "/api/v1/org", map[string]any{
		"machineSessions": map[string]any{"maxDays": 45},
	}, nil)
	var after orgJSONShape
	admin.get("/api/v1/org", &after)
	if after.MachineSessions.MaxDays != 45 {
		t.Fatalf("after PATCH the cap reads %d, want 45", after.MachineSessions.MaxDays)
	}

	// And it reaches the credentials, which is the only reason the setting
	// exists: a machine signing in now is bounded by what the admin just set.
	creds := signIn(t, base)
	if d := time.Until(creds.RefreshExpiresAt); d > 46*24*time.Hour {
		t.Fatalf("refresh token lives %v under a 45-day cap", d)
	}

	// A negative lifetime is refused rather than stored, where it would mean
	// something nobody intended — every refresh instantly past its cap.
	if code, _ := admin.do("PATCH", "/api/v1/org",
		map[string]any{"machineSessions": map[string]any{"maxDays": -1}}); code != http.StatusBadRequest {
		t.Fatalf("negative cap = %d, want 400", code)
	}
	var unchanged orgJSONShape
	admin.get("/api/v1/org", &unchanged)
	if unchanged.MachineSessions.MaxDays != 45 {
		t.Fatalf("a refused write changed the cap to %d", unchanged.MachineSessions.MaxDays)
	}
}

// A malformed refresh body is a bad request, not an unauthenticated one:
// the two mean different things to a client, and only one of them means
// "sign in again".
func TestAMalformedRefreshIsABadRequest(t *testing.T) {
	srv := testServerWithIdP(t)
	base := "http://" + srv.Addr()

	var out struct{ Error string }
	if code := postJSONStatus(t, base+"/api/v1/auth/refresh",
		map[string]string{}, &out); code != http.StatusBadRequest {
		t.Fatalf("empty refresh body = %d, want 400", code)
	}
}
