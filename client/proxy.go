package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/proxy"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// networkProxy ties the pieces of the client's network enforcement together:
// the per-sandbox CA, the running MITM proxy, and the policy store that keeps
// its snapshot fresh. The sandbox is configured to reach the network only
// through it.
type networkProxy struct {
	ca     *proxy.CA
	proxy  *proxy.Proxy
	store  *policyStore
	socket string // Unix socket the proxy listens on; mounted into the sandbox

	// The environment build's proxy under the default "direct" build setting:
	// it refuses nothing, and only the machine that builds the environment is
	// wired to it. Empty when it could not listen, which leaves a build held
	// to the policies like everything else.
	build       *proxy.Proxy
	buildSocket string
}

// setupProxy builds the CA, pulls the first policy snapshot, and starts the
// proxy listening on the host. It returns nil (with a notice) when the pieces
// cannot be assembled, so a session can still run without enforced networking
// while being honest that it is unenforced.
func (a *app) setupProxy(ctx context.Context) *networkProxy {
	stateDir := a.sandboxStateDir()
	ca, err := proxy.LoadOrCreateCA(filepath.Join(stateDir, "ca"))
	if err != nil {
		a.notice(terminalui.NoticeWarn, "network proxy unavailable: "+err.Error())
		return nil
	}

	store := newPolicyStore(a.wire.url, a.wire.token)
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	if err := store.refresh(pctx); err != nil {
		// The proxy still starts; with no snapshot it fails closed, which is
		// the correct posture, not a reason to skip enforcement.
		a.notice(terminalui.NoticeWarn, "policy snapshot not yet available — network requests will block until it arrives")
	}
	// The hosts already turned down, before anything asks for one. Missing
	// them costs a refusal its reason, not the refusal, so a backend that
	// cannot list them is not worth a warning.
	a.loadRejected(pctx)
	cancel()

	np := &networkProxy{ca: ca, store: store}
	np.proxy = proxy.New(proxy.Config{
		CA:       ca,
		Snapshot: store.Usable,
		Approve:  a.proxyApprove,
		Report:   a.proxyReport,
		Rejected: a.rejectedRef,
	})
	// The proxy listens on a short-pathed Unix socket. The machine relays the
	// guest's connections to it, so the hop out of the sandbox is a file on
	// this disk rather than a network route and nothing is exposed on the
	// network. The path must stay under the ~104-char AF_UNIX limit, so it
	// lives in the temp dir keyed by the workspace, not under the (possibly
	// long) sandbox state dir.
	np.socket = filepath.Join(os.TempDir(), "john-"+sandbox.WorkspaceKey(a.workdir)+".sock")
	if err := np.proxy.ListenUnix(np.socket); err != nil {
		a.notice(terminalui.NoticeWarn, "network proxy could not listen: "+err.Error())
		return nil
	}

	// A policy change reaches a running session the same way a network policy
	// does: on the next snapshot, without waiting for the developer to
	// restart anything. That includes where a build's traffic goes, so an
	// administrator who turns it on reaches the next `/rebuild` rather than
	// the next john.
	store.onSnapshot = func(snap *policy.Snapshot) {
		a.protect(snap)
		if vm, ok := a.sb.(*sandbox.VM); ok {
			vm.SetBuildNetwork(snap.BuildNetwork())
		}
	}

	go np.proxy.Serve()
	go store.run(ctx)

	np.build = proxy.New(proxy.Config{CA: ca, Open: true})
	buildSocket := filepath.Join(os.TempDir(), "johnb-"+sandbox.WorkspaceKey(a.workdir)+".sock")
	if err := np.build.ListenUnix(buildSocket); err != nil {
		a.notice(terminalui.NoticeWarn, "the build proxy could not listen, so image builds are held to "+
			"the network policies: "+err.Error())
	} else {
		np.buildSocket = buildSocket
		go np.build.Serve()
	}
	return np
}

// sandboxStateDir is where this workspace's sandbox keeps what outlives it.
// The sandbox answers, rather than the client working it out again: the
// proxy's certificate authority belongs to the sandbox that trusts it, and two
// places computing the same path is two places for them to stop agreeing.
func (a *app) sandboxStateDir() string { return a.sb.StateDir() }

// proxyApprove shows one buffered ask-developer request to the developer and
// returns their answer about that one request.
func (a *app) proxyApprove(req policy.Request, bodyPreview string) bool {
	id := fmt.Sprintf("approval-%d", a.nextID())
	reply := make(chan terminalui.ApprovalDecision, 1)
	a.mu.Lock()
	a.approvals[id] = reply
	a.mu.Unlock()

	headers := []terminalui.Header{}
	for name, values := range req.Headers {
		for _, v := range values {
			headers = append(headers, terminalui.Header{Name: name, Value: v})
		}
	}
	a.ui.Send(terminalui.ApprovalRequest{
		ID:      id,
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: headers,
		Body:    bodyPreview,
		Verdict: "ask developer",
		Policy:  "this request matched an ask-developer policy",
		Options: terminalui.DefaultApprovalOptions(),
	})

	dec := <-reply
	return dec.Decision == terminalui.DecisionAllowOnce
}

// proxyReport records a decision: up the wire for the trace when a session is
// live, and — for an unknown request — an offer to file a network access
// request, the one outcome a developer can act on.
func (a *app) proxyReport(o proxy.Outcome) {
	verdict := "denied"
	switch o.Verdict {
	case policy.VerdictAllow:
		verdict = "allowed"
	case policy.VerdictAsk:
		verdict = "asked"
	case policy.VerdictUnknown:
		verdict = "unknown"
	}
	a.wire.send(&protocol.ProxyDecision{
		At:       time.Now().UTC().Format(time.RFC3339),
		Method:   o.Method,
		URL:      o.URL,
		Verdict:  verdict,
		Policy:   o.Policy,
		Allowed:  o.Allowed,
		Error:    o.Error,
		Rejected: o.Rejected,
	})
	// An ask-developer request already has its line: closing the approval
	// box writes the developer's answer into the transcript, and a second
	// line saying "asked" would be the same request told twice — unless it
	// then failed, which the box could not know.
	if o.Verdict != policy.VerdictAsk || o.Error != "" {
		// "unknown" is the engine's word, and the wire's; on screen it is
		// what happened to the request and why.
		shown := verdict
		switch {
		case o.Rejected != "":
			shown = "blocked · " + o.Rejected + " was rejected"
		case o.Verdict == policy.VerdictUnknown:
			shown = "blocked · no matching policy"
		}
		a.ui.Send(terminalui.ProxyDecision{Method: o.Method, URL: o.URL,
			Verdict: shown, Allowed: o.Allowed, Error: o.Error})
	}

	if o.Verdict == policy.VerdictUnknown {
		go a.blockedUnknown(o)
	}
}

// refreshPolicies pulls a fresh snapshot now, because the backend said the one
// held is out of date.
//
// The snapshot's own refresh timer is what makes this correct — a client that
// missed the message still catches up, and one that cannot reach the backend
// still expires into fail-closed. This is what makes it quick: an approved
// access request works on the next attempt rather than at the next refresh.
func (a *app) refreshPolicies(what []string) {
	np := a.np
	if np == nil {
		return
	}
	before := np.store.Current()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := np.store.refresh(ctx); err != nil {
		a.notice(terminalui.NoticeWarn, "policies changed, but the new ones could not be fetched: "+err.Error())
		return
	}
	if a.wasJustApproved(what, before, np.store.Current()) {
		return
	}
	a.notice(terminalui.NoticeInfo, policyChangeNotice(what))
}

// wasJustApproved reports whether a refresh brings nothing but the network
// policies of access requests the developer was just told were approved. The
// backend tells the requester before it writes the policy, so the decision
// is already on screen; a "policies updated" under "REQ-100 approved ·
// deb.debian.org" was the same news twice.
func (a *app) wasJustApproved(what []string, before, after *policy.Snapshot) bool {
	if len(what) != 1 || what[0] != "network" || after == nil {
		return false
	}
	had := map[string]bool{}
	if before != nil {
		for _, p := range before.Network {
			had[p.ID] = true
		}
	}
	var added []string
	for _, p := range after.Network {
		if !had[p.ID] {
			added = append(added, p.Spec.Host)
		}
		delete(had, p.ID)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Nothing taken away, and nothing new but what was approved.
	if len(a.approved) == 0 || len(had) > 0 {
		return false
	}
	for _, host := range added {
		if !a.approved[host] {
			return false
		}
	}
	for _, host := range added {
		delete(a.approved, host)
	}
	return true
}

// policyChangeNotice says what an administrator changed, in the backend's
// words for it. A backend from before PolicyRefresh said what changed says
// nothing, and that is "policies updated", which is all it could mean then.
func policyChangeNotice(what []string) string {
	names := map[string]string{
		"network":      "network policies",
		"workspace":    "workspace protections",
		"routing":      "model routing",
		"replacements": "replacements",
		"models":       "models",
		"builds":       "build settings",
		"prompts":      "prompt settings",
	}
	var parts []string
	seen := map[string]bool{}
	for _, w := range what {
		name := names[w]
		if name == "" {
			// Groups, or a kind newer than this client: either can be any
			// of the others.
			name = "policies"
		}
		if !seen[name] {
			seen[name] = true
			parts = append(parts, name)
		}
	}
	switch len(parts) {
	case 0:
		return "policies updated"
	case 1:
		return parts[0] + " updated"
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1] + " updated"
}

// accessDecided says what became of an access request the developer filed.
// An approval also arrives as a policy refresh, but only this says which
// request it answered; a rejection arrives as nothing else at all.
func (a *app) accessDecided(m *protocol.AccessDecision) {
	what := m.Host
	if what == "" {
		what = m.Title
	}
	if m.Status == "rejected" {
		if m.Host != "" {
			a.mu.Lock()
			if a.rejected == nil {
				a.rejected = map[string]string{}
			}
			a.rejected[m.Host] = m.Ref
			a.mu.Unlock()
		}
		a.notice(terminalui.NoticeWarn, m.Ref+" rejected · "+what+" stays blocked")
		return
	}
	a.mu.Lock()
	delete(a.rejected, m.Host)
	if m.Status == "approved" && m.Host != "" {
		if a.approved == nil {
			a.approved = map[string]bool{}
		}
		a.approved[m.Host] = true
	}
	a.mu.Unlock()
	a.notice(terminalui.NoticeInfo, m.Ref+" "+m.Status+" · "+what)
}

// rejectedRef is the access request turned down for a host, or "".
func (a *app) rejectedRef(host string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rejected[host]
}

// loadRejected rebuilds the rejected hosts from the backend's record of the
// developer's access requests. The map was only ever filled by the
// AccessDecision a live session received, so after a restart a rejected
// host was blocked as one nobody had asked about: "no matching policy" on
// screen, and an invitation to the agent to ask again. A host whose latest
// decided request was approved is not rejected, whatever came before it.
func (a *app) loadRejected(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", a.wire.url+"/api/v1/access-requests?requesterId=me", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.wire.currentToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend returned %d", resp.StatusCode)
	}
	var reqs []struct {
		Ref       string `json:"ref"`
		Status    string `json:"status"`
		DecidedAt string `json:"decidedAt"`
		Proposed  struct {
			Spec policy.NetworkSpec `json:"spec"`
		} `json:"proposed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reqs); err != nil {
		return err
	}
	// Oldest decision first, so each host ends on its latest. The times are
	// RFC 3339 in UTC, which sort as strings.
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].DecidedAt < reqs[j].DecidedAt })
	rejected := map[string]string{}
	for _, r := range reqs {
		host := r.Proposed.Spec.Host
		if host == "" || r.DecidedAt == "" {
			continue
		}
		switch r.Status {
		case "rejected":
			rejected[host] = r.Ref
		case "approved":
			delete(rejected, host)
		}
	}
	a.mu.Lock()
	// What a live AccessDecision said while this was being fetched is newer
	// than anything in the list.
	for host, ref := range a.rejected {
		rejected[host] = ref
	}
	a.rejected = rejected
	a.mu.Unlock()
	return nil
}

// proxySocket is the host path of the Unix socket the sandbox mounts to reach
// the proxy.
func (np *networkProxy) proxySocket() string { return np.socket }
