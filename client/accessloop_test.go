//go:build !windows

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/proxy"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/store"
)

// The whole of it, in one session: a request nobody wrote a policy for is
// blocked, the developer asks for one, an administrator approves it, and the
// next attempt goes through — with nothing restarted and nothing waited out.
//
// Every piece of this was tested on its own. Together they are the loop the
// product is about, and the join between them is where it went wrong: the
// backend never told a live session that an administrator had decided
// anything, and the client ignored the message when it did. What a developer
// got was an approval that worked whenever their snapshot happened to age out.
func TestABlockedRequestBecomesAnAllowedOneWithoutRestarting(t *testing.T) {
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("REACHED-ORIGIN"))
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(ln)
	t.Cleanup(func() { origin.Close() })

	// A group for the administrator to put the new policy in. Baseline, so it
	// applies to the one developer here without anybody joining a team.
	var groupID string
	s := newE2EWith(t, func(st *store.Store) {
		g, err := st.CreatePolicyGroup("Baseline", "Everything, everyone", true, "admin")
		if err != nil {
			t.Fatal(err)
		}
		groupID = g.ID
	})

	// The sandbox's route out, dialled the way the sandbox dials it.
	through := proxyClient(t, filepath.Join(os.TempDir(),
		"john-"+sandbox.WorkspaceKey(s.workdir)+".sock"))
	target := "http://" + ln.Addr().String() + "/orders"

	// Blocked: no policy covers it, and the proxy does not guess.
	if body := fetch(t, through, target); strings.Contains(body, "REACHED-ORIGIN") {
		t.Fatalf("a request no policy covers reached the origin: %q", body)
	}

	// john offers the developer the one thing they can do about it.
	// It asks how far the request should reach; the whole host is the answer
	// that unblocks a tool rather than the one URL it tried first.
	s.wait("REQUEST A POLICY")
	s.wait("the whole host")
	s.type_("h")
	s.wait("network access request REQ-")

	// It is a real request, in the administrator's queue.
	st := s.srv.Store()
	var requestID string
	deadline := time.Now().Add(8 * time.Second * timeScale)
	for time.Now().Before(deadline) {
		reqs, err := st.AccessRequests("pending", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(reqs) == 1 {
			requestID = reqs[0].ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if requestID == "" {
		t.Fatal("the filed request never reached the backend")
	}

	// An administrator approves it, which writes the policy the requester
	// proposed into the group the reviewer chose.
	decide(t, s, requestID, groupID)

	// The session finds out now rather than when its snapshot ages out.
	s.wait(" approved · ")
	s.waitAllowed(target)

	// And the request that was blocked goes through.
	if body := fetch(t, through, target); !strings.Contains(body, "REACHED-ORIGIN") {
		t.Fatalf("the approved request still does not reach the origin: %q", body)
	}
}

// proxyClient dials john's proxy on its Unix socket and speaks HTTP proxy to
// it, which is what a command inside the sandbox is doing when it honours
// http_proxy.
func proxyClient(t *testing.T, socket string) *http.Client {
	t.Helper()
	return &http.Client{
		Timeout: 15 * time.Second * timeScale,
		Transport: &http.Transport{
			Proxy: func(*http.Request) (*url.URL, error) { return url.Parse("http://john-proxy") },
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		},
	}
}

func fetch(t *testing.T, c *http.Client, target string) string {
	t.Helper()
	resp, err := c.Get(target)
	if err != nil {
		return "request failed: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// decide approves the request through the API the console uses, as the
// administrator this developer also happens to be.
func decide(t *testing.T, s *e2eSession, requestID, groupID string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"status": "approved", "groupId": groupID})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST",
		"http://"+s.srv.Addr()+"/api/v1/access-requests/"+requestID+"/decision", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.app.wire.currentToken())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("approving the request: %s — %s", resp.Status, out)
	}
}

// A request is for the methods that were seen. Once it is decided, a block
// with a method it never asked for — npm's POST to the registry after its
// GETs — is asked about in its turn, not dropped: before, the host went quiet
// for the rest of the session and the POST failed with nobody told.
func TestANewMethodOnADecidedHostIsAskedAboutAgain(t *testing.T) {
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("REACHED-ORIGIN"))
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(ln)
	t.Cleanup(func() { origin.Close() })

	var groupID string
	s := newE2EWith(t, func(st *store.Store) {
		g, err := st.CreatePolicyGroup("Baseline", "Everything, everyone", true, "admin")
		if err != nil {
			t.Fatal(err)
		}
		groupID = g.ID
	})
	through := proxyClient(t, filepath.Join(os.TempDir(),
		"john-"+sandbox.WorkspaceKey(s.workdir)+".sock"))
	target := "http://" + ln.Addr().String() + "/-/npm/v1/security/audits/quick"

	requests := func(n int) []*store.AccessRequest {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second * timeScale)
		for {
			reqs, err := s.srv.Store().AccessRequests("", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(reqs) >= n || time.Now().After(deadline) {
				return reqs
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// A GET, asked about and approved: the request is for GET.
	fetch(t, through, target)
	s.wait("REQUEST A POLICY")
	s.type_("h")
	s.wait("network access request REQ-")
	first := requests(1)
	if len(first) != 1 {
		t.Fatalf("%d requests after the first block", len(first))
	}
	decide(t, s, first[0].ID, groupID)
	s.waitAllowed(target)
	if body := fetch(t, through, target); !strings.Contains(body, "REACHED-ORIGIN") {
		t.Fatalf("the approved GET does not reach the origin: %q", body)
	}

	// A POST to the same host is blocked — and asked about, as a request of
	// its own.
	resp, err := through.Post(target, "application/json", strings.NewReader("{}"))
	if err == nil {
		resp.Body.Close()
	}
	asked := func() bool {
		// An answered question is erased, so this one is the one drawn
		// after the POST's line.
		scr := s.screen()
		i := strings.Index(scr, "proxy POST")
		return i >= 0 && strings.Contains(scr[i:], "REQUEST A POLICY")
	}
	deadline := time.Now().Add(8 * time.Second * timeScale)
	for !asked() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !asked() {
		t.Fatalf("the POST on a decided host was not asked about:\n%s", s.screen())
	}
	// And it says it is the same host, asked about before for GET.
	s.wait("It was asked for GET.")
	s.type_("h")
	reqs := requests(2)
	if len(reqs) != 2 {
		t.Fatalf("%d requests after the POST, want a second one", len(reqs))
	}
	var spec string
	for _, r := range reqs {
		if r.ID != first[0].ID {
			spec = string(r.Spec)
		}
	}
	if !strings.Contains(spec, "POST") || strings.Contains(spec, "GET") {
		t.Fatalf("the second request is for %s, want POST alone", spec)
	}
}

// waitAllowed waits for the session's snapshot to allow a URL: the refresh
// an approval sends has arrived and been taken in.
func (s *e2eSession) waitAllowed(target string) {
	s.t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		s.t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second * timeScale)
	for time.Now().Before(deadline) {
		snap := s.app.np.store.Current()
		if snap != nil && snap.Evaluate(policy.Request{Method: "GET", URL: u, Headers: map[string][]string{}}).Verdict == policy.VerdictAllow {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("the session's snapshot never allowed %s:\n%s", target, s.screen())
}

// An approval is said once. The developer is told "REQ-1 approved · host",
// and the policy refresh the approval sends after it is that same news: it
// used to be followed by "policies updated" every time.
func TestAnApprovalIsNotAnnouncedTwice(t *testing.T) {
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("REACHED-ORIGIN"))
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(ln)
	t.Cleanup(func() { origin.Close() })

	var groupID string
	s := newE2EWith(t, func(st *store.Store) {
		g, err := st.CreatePolicyGroup("Baseline", "Everything, everyone", true, "admin")
		if err != nil {
			t.Fatal(err)
		}
		groupID = g.ID
	})
	through := proxyClient(t, filepath.Join(os.TempDir(),
		"john-"+sandbox.WorkspaceKey(s.workdir)+".sock"))
	target := "http://" + ln.Addr().String() + "/orders"

	fetch(t, through, target)
	s.wait("REQUEST A POLICY")
	s.type_("h")
	s.wait("network access request REQ-")
	reqs, err := s.srv.Store().AccessRequests("pending", "", "", "")
	if err != nil || len(reqs) != 1 {
		t.Fatalf("pending requests = %d, %v", len(reqs), err)
	}
	decide(t, s, reqs[0].ID, groupID)
	s.wait(reqs[0].Ref + " approved")
	s.waitAllowed(target)
	// Room for a notice that was going to come.
	time.Sleep(300 * time.Millisecond * timeScale)
	if strings.Contains(s.screen(), "policies updated") || strings.Contains(s.screen(), "network policies updated") {
		t.Fatalf("the approval was announced a second time:\n%s", s.screen())
	}

	// A change that is not the approval is still said, as what it is.
	if err := s.srv.Store().CreateModel(&store.Model{ID: "m_second", Name: "second-model",
		Provider: "selfhosted", Hosting: "internal", ModelID: "second"}); err != nil {
		t.Fatal(err)
	}
	s.srv.Store().BumpPolicyEpoch(store.ChangeModels)
	s.wait("models updated")
	if strings.Contains(s.screen(), "policies updated") {
		t.Fatalf("adding a model was called a policy change:\n%s", s.screen())
	}
}

// A host whose access request was rejected before this client started is
// refused as rejected, on screen and to the sandbox. The rejection was only
// ever held in the memory of the session that received it, so after a
// restart the host was blocked as one nobody had asked about, and the agent
// was told it could ask for a policy.
func TestARejectionOutlivesTheSessionThatHeardIt(t *testing.T) {
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("REACHED-ORIGIN"))
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(ln)
	t.Cleanup(func() { origin.Close() })
	target := "http://" + ln.Addr().String() + "/orders"

	var ref string
	s := newE2EWith(t, func(st *store.Store) {
		user, err := st.UpsertSSOUser("dev-1", "oliver@example.com", "Oliver K", "oliver@example.com")
		if err != nil {
			t.Fatal(err)
		}
		spec, err := proxy.ProposedPolicy(proxy.Outcome{Method: "GET", URL: target}, false)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(spec)
		r, err := st.CreateAccessRequest(user.ID, "", "Access to 127.0.0.1", "", "", nil, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DecideAccessRequest(r.ID, "rejected", user.ID, nil, nil, ""); err != nil {
			t.Fatal(err)
		}
		ref = r.Ref
	})
	through := proxyClient(t, filepath.Join(os.TempDir(),
		"john-"+sandbox.WorkspaceKey(s.workdir)+".sock"))

	body := fetch(t, through, target)
	if !strings.Contains(body, ref+" for 127.0.0.1 was rejected") || strings.Contains(body, "ask for a policy") {
		t.Fatalf("the sandbox was told %q, not that %s was rejected", body, ref)
	}
	s.wait("blocked · " + ref + " was rejected")

	// And it is not offered to be asked for again: the answer was no.
	time.Sleep(300 * time.Millisecond * timeScale)
	if strings.Contains(s.screen(), "REQUEST A POLICY") {
		t.Fatalf("john offered to ask again for a host whose request was rejected:\n%s", s.screen())
	}
}
