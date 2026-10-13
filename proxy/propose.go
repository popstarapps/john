package proxy

import (
	"net/url"
	"strconv"

	"github.com/popstarapps/john/policy"
)

// ProposedPolicy is the network policy a blocked-unknown request asks an
// administrator for: the method that was blocked, to that host, defaulting to
// always-allow for the administrator to tighten. The developer chooses the
// reach. The whole host is usually what they mean — what gets blocked is
// rarely one URL: apt fetches an index and then a hundred packages, a
// language's package manager a tree of them, and a policy for the one file
// that happened to be asked for first would be approved and still leave the
// tool blocked. exactPath asks for that one path instead, for the developer
// who knows a single endpoint is all they need. The blocked URLs travel with
// the request as evidence for the reviewer either way.
func ProposedPolicy(o Outcome, exactPath bool) (policy.NetworkSpec, error) {
	u, err := url.Parse(o.URL)
	if err != nil {
		return policy.NetworkSpec{}, err
	}
	// A port the scheme implies is left out, so a policy for an HTTPS host
	// reads like one for an HTTP host, and matches the same.
	var port *int
	if p, err := strconv.Atoi(u.Port()); err == nil && p != 0 &&
		!(u.Scheme == "https" && p == 443) && !(u.Scheme == "http" && p == 80) {
		port = &p
	}
	return policy.NetworkSpec{
		Kind:          "network",
		Methods:       []string{o.Method},
		Scheme:        u.Scheme,
		Host:          u.Hostname(),
		Port:          port,
		Path:          proposedPath(u, exactPath),
		HeaderMatches: []policy.AttributeMatch{},
		QueryMatches:  []policy.AttributeMatch{},
		Decision:      policy.DecisionAllow,
	}, nil
}

func proposedPath(u *url.URL, exact bool) string {
	switch {
	case !exact:
		return "/*"
	case u.Path == "":
		return "/"
	default:
		return u.Path
	}
}
