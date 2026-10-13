package policy

import "strings"

// Router is the model-routing controls for one session: which models are
// assigned, and the keyword rules that narrow the session when one is seen in
// what is about to reach a model.
//
// Keywords are literal, case-insensitive strings rather than a classifier —
// inspectable, testable, and identical on every machine, the same reason every
// other policy decision is made outside the model.
type Router struct {
	rules []routeRule
}

type routeRule struct {
	policyID string
	group    string
	keywords []string
	modelIDs []string // the set the session narrows to on a hit
}

// KeywordRule describes a routing policy that can narrow a session.
type KeywordRule struct {
	PolicyID string
	Group    string
	Keywords []string
	ModelIDs []string
}

// NewRouter builds a router from restrict-mode routing policies that carry
// keywords. Assign-mode and keyword-free policies decide the assigned set
// elsewhere (the backend's ModelsForUser); they never narrow mid-session.
func NewRouter(rules []KeywordRule) *Router {
	r := &Router{}
	for _, k := range rules {
		if len(k.Keywords) == 0 || len(k.ModelIDs) == 0 {
			continue
		}
		r.rules = append(r.rules, routeRule{
			policyID: k.PolicyID, group: k.Group, keywords: k.Keywords, modelIDs: k.ModelIDs,
		})
	}
	return r
}

// Narrowing is the result of a keyword hit: the session must narrow to Models.
type Narrowing struct {
	PolicyID string
	Group    string
	Keyword  string
	ModelIDs []string
}

// Match scans content for any keyword, returning the first rule that hits.
// The match happens before the request leaves for a provider — narrowing after
// the content had reached a model would be a record of a leak, not a defence
// against one.
//
// already is the set of policy ids that have already narrowed this session, so
// a rule never fires twice: a session narrows, and never un-narrows.
func (r *Router) Match(content string, already map[string]bool) *Narrowing {
	lower := strings.ToLower(content)
	for _, rule := range r.rules {
		if already[rule.policyID] {
			continue
		}
		for _, kw := range rule.keywords {
			if kw == "" {
				continue
			}
			if strings.Contains(lower, strings.ToLower(kw)) {
				return &Narrowing{
					PolicyID: rule.policyID, Group: rule.group,
					Keyword: kw, ModelIDs: rule.modelIDs,
				}
			}
		}
	}
	return nil
}

// HasRules reports whether any keyword rule exists, so a session with none
// skips the scan entirely.
func (r *Router) HasRules() bool { return len(r.rules) > 0 }
