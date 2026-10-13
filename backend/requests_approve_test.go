package backend

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/popstarapps/john/policy"
)

// An approval writes a policy, and a policy lives in a group. Approving into
// no group used to mark the request approved and write nothing; now it is
// refused, and so is a new group that would apply to nobody. A new group
// made there, applying to everyone, is where the policy lands — and the
// requester's next snapshot allows what they asked for.
func TestAnApprovalAlwaysLandsInAGroupThatApplies(t *testing.T) {
	srv, admin, member, memberUser := adminAndMember(t)
	spec := map[string]any{"kind": "network", "methods": []string{"GET"}, "scheme": "http",
		"host": "deb.debian.org", "port": nil, "path": "/*",
		"headerMatches": []any{}, "queryMatches": []any{}, "decision": "always-allow", "maxBodyBytes": nil}

	var filed struct{ ID, Ref string }
	member.post("POST", "/api/v1/access-requests", map[string]any{
		"title": "Access to deb.debian.org", "blocked": "GET http://deb.debian.org/debian/dists/stable/InRelease",
		"spec": spec,
	}, &filed)

	for name, body := range map[string]map[string]any{
		"no group":            {"status": "approved"},
		"a group of nobody":   {"status": "approved", "newGroup": map[string]any{"name": "debian"}},
		"a nameless group":    {"status": "approved", "newGroup": map[string]any{"baseline": true}},
		"both kinds of group": {"status": "approved", "groupId": "g_x", "newGroup": map[string]any{"name": "d", "baseline": true}},
	} {
		if code, out := admin.do("POST", "/api/v1/access-requests/"+filed.ID+"/decision", body); code != http.StatusBadRequest {
			t.Fatalf("approving into %s = %d: %s", name, code, out)
		}
	}

	var decided struct {
		Status   string
		Proposed struct{ GroupID *string }
	}
	admin.post("POST", "/api/v1/access-requests/"+filed.ID+"/decision", map[string]any{
		"status": "approved", "spec": spec,
		"newGroup": map[string]any{"name": "Debian mirrors", "baseline": true},
	}, &decided)
	if decided.Status != "approved" || decided.Proposed.GroupID == nil {
		t.Fatalf("decided = %+v", decided)
	}
	g, err := srv.Store().PolicyGroupByID(*decided.Proposed.GroupID)
	if err != nil || g.Name != "Debian mirrors" || !g.Baseline {
		t.Fatalf("new group = %+v, %v", g, err)
	}

	u, _ := url.Parse("http://deb.debian.org/debian/pool/main/c/curl/curl.deb")
	snap := snapshotFor(t, srv, memberUser.ID)
	if v := snap.Evaluate(policy.Request{Method: "GET", URL: u, Headers: map[string][]string{}}).Verdict; v != policy.VerdictAllow {
		t.Fatalf("after approval the requester's snapshot says %v for the host, want allow", v)
	}
}

// One open request per host per person while the requests cover each other:
// a second block on the host joins a host-wide request, widening its methods
// and listing what was blocked. Two different exact paths — the developer
// chose "only this path" — stay two requests, until one of them asks for the
// whole host. Merge-only filings create nothing.
func TestBlocksOnOneHostMakeOneRequest(t *testing.T) {
	_, admin, member, _ := adminAndMember(t)
	file := func(path, blocked, method string, mergeOnly bool) (int, map[string]any) {
		spec := map[string]any{"kind": "network", "methods": []string{method}, "scheme": "http",
			"host": "deb.debian.org", "port": nil, "path": path,
			"headerMatches": []any{}, "queryMatches": []any{}, "decision": "always-allow", "maxBodyBytes": nil}
		var out map[string]any
		code, body := member.do("POST", "/api/v1/access-requests", map[string]any{
			"title": "Access to deb.debian.org", "blocked": method + " http://deb.debian.org" + blocked,
			"spec": spec, "mergeOnly": mergeOnly,
		})
		if code == http.StatusOK {
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
		}
		return code, out
	}
	openRequests := func() []struct {
		Justification string
		Proposed      struct{ Spec policy.NetworkSpec }
	} {
		var open []struct {
			Justification string
			Proposed      struct{ Spec policy.NetworkSpec }
		}
		admin.get("/api/v1/access-requests?status=open", &open)
		return open
	}

	if code, _ := file("/*", "/a", "GET", true); code != http.StatusNotFound {
		t.Fatalf("merge-only with nothing open = %d, want 404", code)
	}

	// Two exact paths: two requests.
	_, a := file("/a", "/a", "GET", false)
	_, b := file("/b", "/b", "GET", false)
	if a["ref"] == b["ref"] {
		t.Fatalf("two different exact paths were merged into %v", a["ref"])
	}
	// The same exact path again: the same request.
	if _, again := file("/a", "/a", "HEAD", false); again["ref"] != a["ref"] || again["merged"] != true {
		t.Fatalf("the same path again = %v, want merged into %v", again["ref"], a["ref"])
	}

	// The whole host: it joins an open request for the host and widens it.
	_, host := file("/*", "/c", "GET", false)
	if host["merged"] != true {
		t.Fatalf("a request for the whole host = %v, want merged into an open one", host)
	}
	open := openRequests()
	if len(open) != 2 {
		t.Fatalf("open requests = %d, want 2", len(open))
	}
	var widened *policy.NetworkSpec
	for i := range open {
		if open[i].Proposed.Spec.Path == "/*" {
			widened = &open[i].Proposed.Spec
			if !strings.Contains(open[i].Justification, "GET http://deb.debian.org/c") {
				t.Fatalf("the widened request does not list what was blocked:\n%s", open[i].Justification)
			}
		}
	}
	if widened == nil {
		t.Fatalf("no request was widened to the whole host: %+v", open)
	}
}
