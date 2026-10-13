package backend

import (
	"crypto/ed25519"
	"net/http"
	"testing"

	"github.com/popstarapps/john/policy"
)

// snapshotFor builds the policy snapshot a machine would pull for one person
// and verifies it against the organization's public key, so these assertions
// are about what a client would actually honour rather than about a struct the
// backend filled in on the way past.
func snapshotFor(t *testing.T, srv *Server, userID string) *policy.Snapshot {
	t.Helper()
	user, err := srv.Store().UserByID(userID)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := srv.buildSnapshot(user)
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	org, err := srv.Store().Org()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := signingKey(org)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := policy.Verify(signed, priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return snap
}

func policyEpoch(t *testing.T, srv *Server) uint64 {
	t.Helper()
	e, err := srv.Store().PolicyEpoch()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// Where an image build's traffic goes was a setting the sandbox had honoured
// since the day it was written and nothing could set: the three values existed
// on the receiving end only, so every organization was on the default and an
// administrator who wanted a build recorded or policed had no way to say so.
//
// The two halves are asserted together on purpose. A control in the console
// that did not reach the signed snapshot would be a switch wired to nothing,
// and a snapshot field no console could write is where this started.
func TestWhereABuildsTrafficGoesIsSetInTheConsoleAndTravelsSigned(t *testing.T) {
	srv, admin, member, memberUser := adminAndMember(t)

	var org struct {
		Builds struct{ Network string }
	}
	admin.get("/api/v1/org", &org)
	if org.Builds.Network != policy.BuildDirect {
		t.Fatalf("a fresh organization builds over %q, want direct", org.Builds.Network)
	}
	if snap := snapshotFor(t, srv, memberUser.ID); snap.BuildIsProxied() {
		t.Fatal("the default sends a build through the proxy")
	}

	before := policyEpoch(t, srv)

	// An administrator turns it on; the machine's next snapshot carries it.
	admin.post("PATCH", "/api/v1/org", map[string]any{
		"builds": map[string]any{"network": policy.BuildProxied},
	}, &org)
	if org.Builds.Network != policy.BuildProxied {
		t.Fatalf("after the change the console says %q", org.Builds.Network)
	}
	snap := snapshotFor(t, srv, memberUser.ID)
	if snap.BuildNetwork() != policy.BuildProxied || !snap.BuildIsProxied() {
		t.Fatalf("snapshot says %q, proxied=%v", snap.BuildNetwork(), snap.BuildIsProxied())
	}

	// And the epoch moved, which is what tells a session already running that
	// its snapshot is behind, rather than leaving it on the old answer until
	// the one it holds expires.
	if after := policyEpoch(t, srv); after <= before {
		t.Fatalf("policy epoch %d -> %d: a build change must move it", before, after)
	}

	// A value the backend does not recognise is refused at the door rather
	// than stored and puzzled over later — "observed" among them, a setting
	// once offered and never built.
	for _, where := range []string{"wide-open", "observed"} {
		if code, body := admin.do("PATCH", "/api/v1/org", map[string]any{
			"builds": map[string]any{"network": where},
		}); code != http.StatusBadRequest {
			t.Fatalf("PATCH with build network %q = %d, want 400: %s", where, code, body)
		}
	}
	if snap = snapshotFor(t, srv, memberUser.ID); snap.BuildNetwork() != policy.BuildProxied {
		t.Fatalf("a refused change still moved the setting to %q", snap.BuildNetwork())
	}

	// An organization that chose "observed" before it was removed gets what
	// it always got: the build held to the network policies.
	stored, err := srv.Store().Org()
	if err != nil {
		t.Fatal(err)
	}
	stored.BuildNetwork = "observed"
	if err := srv.Store().SaveOrg(stored); err != nil {
		t.Fatal(err)
	}
	admin.get("/api/v1/org", &org)
	if snap = snapshotFor(t, srv, memberUser.ID); snap.BuildNetwork() != policy.BuildProxied ||
		org.Builds.Network != policy.BuildProxied {
		t.Fatalf("a stored observed reads as %q (console %q), want proxied", snap.BuildNetwork(), org.Builds.Network)
	}

	// It is an administrator's decision, so a member cannot make it.
	if code, body := member.do("PATCH", "/api/v1/org", map[string]any{
		"builds": map[string]any{"network": policy.BuildDirect},
	}); code != http.StatusForbidden {
		t.Fatalf("member PATCH /api/v1/org = %d, want 403: %s", code, body)
	}
}
