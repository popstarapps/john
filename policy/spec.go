// Package policy is the deterministic control plane, shared by the backend
// that assembles and signs policy and the client-side proxy that enforces it.
//
// Policies are not instructions a model is asked to follow. They are matched
// against the concrete details of an operation — a request's method and host,
// a file's path, the bytes on their way to a provider — and their decision is
// made outside the model, identically on every machine, which is the whole
// reason the enforcement can be trusted.
//
// The spec types here mirror console/src/api/types.ts exactly; that file is
// the contract and this is its Go side.
package policy

// NetworkDecision is what the proxy does with a request a policy matches.
type NetworkDecision string

const (
	DecisionAllow NetworkDecision = "always-allow"
	DecisionAsk   NetworkDecision = "ask-developer"
	DecisionDeny  NetworkDecision = "deny"
)

// AttributeMatch matches a header or query parameter. Value may contain `*`
// wildcards; an empty value matches presence alone.
type AttributeMatch struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// NetworkSpec restricts an outbound HTTP or HTTPS request. Bodies are never
// content-matched; a maximum size is the only thing said about them.
type NetworkSpec struct {
	Kind          string           `json:"kind"` // "network"
	Methods       []string         `json:"methods"`
	Scheme        string           `json:"scheme"` // https | http | any
	Host          string           `json:"host"`
	Port          *int             `json:"port"`
	Path          string           `json:"path"`
	HeaderMatches []AttributeMatch `json:"headerMatches"`
	QueryMatches  []AttributeMatch `json:"queryMatches"`
	Decision      NetworkDecision  `json:"decision"`
	MaxBodyBytes  *int             `json:"maxBodyBytes"`
}

// ReplacementSpec rewrites content on its way to a model. A `#` in Replacement
// becomes a stable alias per distinct matched value.
type ReplacementSpec struct {
	Kind        string `json:"kind"` // "replacement"
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

// ModelRoutingSpec assigns or restricts models, optionally narrowing a session
// when a keyword is seen in what is about to reach a model.
type ModelRoutingSpec struct {
	Kind     string   `json:"kind"` // "model-routing"
	Mode     string   `json:"mode"` // assign | restrict
	ModelIDs []string `json:"modelIds"`
	Keywords []string `json:"keywords"`
}

// WorkspaceSpec excludes, read-onlys, or protects-from-deletion matching paths.
type WorkspaceSpec struct {
	Kind  string   `json:"kind"` // "workspace"
	Paths []string `json:"paths"`
	Mode  string   `json:"mode"` // exclude | read-only | no-delete
}
