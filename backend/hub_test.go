package backend

import (
	"strings"
	"testing"

	"github.com/popstarapps/john/protocol"
)

// A command the proxy refused requests for says so in its result. npm exits 0
// when only its audit is refused, and apt prints a bare "403 Forbidden"; the
// model told neither reported success, or blamed the mirror.
func TestARunResultNamesWhatTheProxyBlocked(t *testing.T) {
	res := &protocol.ToolResult{Status: "ok", Exit: 0, Stdout: "added 63 packages\n"}

	out, err := mapToolResult("run", res, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Blocks[0].Text; strings.Contains(got, "proxy") {
		t.Fatalf("nothing was blocked, but the result says:\n%s", got)
	}

	blocked := []blockedRequest{
		{Req: "POST https://registry.npmjs.org/-/npm/v1/security/advisories/bulk"},
		{Req: "POST https://registry.npmjs.org/-/npm/v1/security/audits/quick"},
	}
	out, err = mapToolResult("run", res, blocked)
	if err != nil {
		t.Fatal(err)
	}
	got := out.Blocks[0].Text
	if !strings.HasPrefix(got, "exit 0\nadded 63 packages") {
		t.Fatalf("the command's own output should come first:\n%s", got)
	}
	for _, want := range []string{blocked[0].Req, blocked[1].Req, "blocked 2 requests", "no network policy covers it"} {
		if !strings.Contains(got, want) {
			t.Fatalf("result does not mention %q:\n%s", want, got)
		}
	}

	many := make([]blockedRequest, blockedRequestsShown+3)
	for i := range many {
		many[i] = blockedRequest{Req: "GET https://example.com/" + string(rune('a'+i))}
	}
	got = blockedNote(many)
	if strings.Contains(got, many[blockedRequestsShown].Req) || !strings.Contains(got, "and 3 more") {
		t.Fatalf("a long list should be cut off and counted:\n%s", got)
	}
}

// A request refused because its access request was rejected is told to the
// model as that. Every refusal was "no network policy covers it … the
// developer can approve access", so after REQ-103 was rejected the model told
// the developer the policy still did not cover example.org.
func TestARejectedRequestIsToldAsRejected(t *testing.T) {
	m := &protocol.ProxyDecision{Method: "GET", URL: "https://example.org/", Verdict: "denied",
		Policy: "REQ-103 was rejected", Rejected: "REQ-103"}
	got := blockedNote([]blockedRequest{{Req: "GET https://example.org/", Refused: refusedBecause(m)}})
	for _, want := range []string{"REQ-103", "rejected", "don't ask for it again"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the model is not told %q:\n%s", want, got)
		}
	}
	for _, wrong := range []string{"no network policy covers", "can ask for access"} {
		if strings.Contains(got, wrong) {
			t.Fatalf("the model is told %q about a rejected request:\n%s", wrong, got)
		}
	}

	// The other refusals say who decided, too.
	if why := refusedBecause(&protocol.ProxyDecision{Verdict: "asked"}); !strings.Contains(why, "developer turned it down") {
		t.Fatalf("a request the developer refused when asked is described as %q", why)
	}
	if why := refusedBecause(&protocol.ProxyDecision{Verdict: "denied", Policy: "block telemetry"}); why != "denied by policy: block telemetry" {
		t.Fatalf("a deny policy's refusal is described as %q", why)
	}
	if why := refusedBecause(&protocol.ProxyDecision{Verdict: "unknown"}); why != "" {
		t.Fatalf("a request no policy covers is described as refused: %q", why)
	}
}
