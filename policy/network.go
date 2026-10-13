package policy

import (
	"net/url"
	"strings"
)

// Request is the buffered outbound request a network policy is evaluated
// against. Everything a policy may match on is here; the body is not, because
// bodies are never content-matched — only their size is.
type Request struct {
	Method   string
	URL      *url.URL
	Headers  map[string][]string
	BodySize int
}

// Verdict is the outcome of evaluating a request against the snapshot.
type Verdict string

const (
	// Allow: forward without interrupting the developer.
	VerdictAllow Verdict = "allow"
	// Ask: hold the request and show it to the developer, who decides about
	// that one request and nothing else.
	VerdictAsk Verdict = "ask"
	// Deny: refuse outright; an administrator already decided.
	VerdictDeny Verdict = "deny"
	// Unknown: nothing matched, so it is refused the moment it is made. The
	// one outcome a developer can act on, by asking for a policy.
	VerdictUnknown Verdict = "unknown"
)

// Decision is a verdict with the reason and matching policy behind it, for
// the trace and the developer.
type Decision struct {
	Verdict  Verdict
	Reason   string
	PolicyID string
}

// compiledNetwork is a NetworkSpec ready to match, carrying its source policy.
type compiledNetwork struct {
	policyID string
	spec     NetworkSpec
}

// Evaluate decides one request against the network policies. When several
// match, the most restrictive wins — deny over ask over allow — so a broad
// allow can never loosen a narrow deny. Nothing matching is Unknown, which is
// blocked; Unknown is the only thing a developer can file a request about.
func (s *Snapshot) Evaluate(req Request) Decision {
	best := Decision{Verdict: VerdictUnknown, Reason: "no policy covers this request"}
	rank := map[Verdict]int{VerdictUnknown: 0, VerdictAllow: 1, VerdictAsk: 2, VerdictDeny: 3}

	for _, c := range s.network {
		if !matchNetwork(c.spec, req) {
			continue
		}
		v := decisionToVerdict(c.spec.Decision)
		if rank[v] >= rank[best.Verdict] {
			best = Decision{
				Verdict:  v,
				Reason:   describeMatch(c.spec),
				PolicyID: c.policyID,
			}
		}
	}
	return best
}

func decisionToVerdict(d NetworkDecision) Verdict {
	switch d {
	case DecisionAllow:
		return VerdictAllow
	case DecisionAsk:
		return VerdictAsk
	default:
		return VerdictDeny
	}
}

func matchNetwork(spec NetworkSpec, req Request) bool {
	if len(spec.Methods) > 0 && !containsFold(spec.Methods, req.Method) {
		return false
	}
	if spec.Scheme != "" && spec.Scheme != "any" && !strings.EqualFold(spec.Scheme, req.URL.Scheme) {
		return false
	}
	if spec.Host != "" && !matchHost(spec.Host, req.URL.Hostname()) {
		return false
	}
	if spec.Port != nil && portOf(req.URL) != *spec.Port {
		return false
	}
	if spec.Path != "" && !matchGlob(spec.Path, req.URL.Path) {
		return false
	}
	for _, h := range spec.HeaderMatches {
		if !matchHeader(req.Headers, h) {
			return false
		}
	}
	q := req.URL.Query()
	for _, m := range spec.QueryMatches {
		if !matchAttr(q[m.Name], m.Value) {
			return false
		}
	}
	// A body larger than the policy permits fails the match: the policy that
	// would have allowed a small request does not allow a large one.
	if spec.MaxBodyBytes != nil && req.BodySize > *spec.MaxBodyBytes {
		return false
	}
	return true
}

func matchHeader(headers map[string][]string, m AttributeMatch) bool {
	// Header names are matched case-insensitively.
	for name, values := range headers {
		if strings.EqualFold(name, m.Name) {
			return matchAttr(values, m.Value)
		}
	}
	return false
}

func matchAttr(values []string, want string) bool {
	if want == "" {
		return len(values) > 0 // presence alone
	}
	for _, v := range values {
		if matchGlob(want, v) {
			return true
		}
	}
	return false
}

// matchHost allows a leading "*." wildcard: "*.internal" matches
// "api.internal" but not "internal" itself.
func matchHost(pattern, host string) bool {
	pattern, host = strings.ToLower(pattern), strings.ToLower(host)
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".internal"
		return strings.HasSuffix(host, suffix)
	}
	return matchGlob(pattern, host)
}

// matchGlob matches a string against a pattern with `*` wildcards. No other
// metacharacters, so it behaves identically everywhere — the same reason
// keywords are strings rather than a classifier.
func matchGlob(pattern, s string) bool {
	if pattern == "*" || pattern == s {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(s[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && idx != 0 {
			return false // no leading * means the first part must anchor
		}
		pos += idx + len(part)
	}
	// A trailing part with no closing * must reach the end.
	if last := parts[len(parts)-1]; last != "" {
		return strings.HasSuffix(s, last)
	}
	return true
}

func containsFold(xs []string, x string) bool {
	for _, v := range xs {
		if strings.EqualFold(v, x) {
			return true
		}
	}
	return false
}

func portOf(u *url.URL) int {
	if p := u.Port(); p != "" {
		n := 0
		for _, r := range p {
			n = n*10 + int(r-'0')
		}
		return n
	}
	if u.Scheme == "http" {
		return 80
	}
	return 443
}

func describeMatch(spec NetworkSpec) string {
	methods := "any method"
	if len(spec.Methods) > 0 {
		methods = strings.Join(spec.Methods, "/")
	}
	host := spec.Host
	if host == "" {
		host = "any host"
	}
	switch spec.Decision {
	case DecisionAllow:
		return "allowed: " + methods + " to " + host
	case DecisionAsk:
		return "requires approval: " + methods + " to " + host
	default:
		return "denied: " + methods + " to " + host
	}
}
