package policy

import "strings"

// The workspace-protection modes, spelled as the console writes them into a
// WorkspaceSpec.
const (
	// ModeExclude keeps matching paths out of the sandbox entirely: never
	// seeded, never readable, never writable, never applied.
	ModeExclude = "exclude"
	// ModeReadOnly lets the agent read matching paths and not change them.
	ModeReadOnly = "read-only"
	// ModeNoDelete lets the agent change matching paths and not remove them.
	ModeNoDelete = "no-delete"
)

// PathRule is one protected path pattern and the policy that carries it, so a
// refusal can name the rule behind it rather than only the fact of it.
type PathRule struct {
	Pattern  string `json:"pattern"`
	PolicyID string `json:"policy_id"`
}

// Protections are the workspace-protection policies a sandbox enforces,
// sorted by what each mode forbids. The sandbox asks it about a path; it
// never asks the backend, exactly like the proxy and its snapshot.
//
// The modes are a ladder, not a set of independent switches: an excluded path
// cannot be read, and a read-only path cannot be deleted either, because
// deleting a file is the largest change you can make to it. Check* walks the
// ladder from the most restrictive mode down, so the reason a developer is
// given is the strongest rule that applies.
type Protections struct {
	Exclude  []PathRule `json:"exclude"`
	ReadOnly []PathRule `json:"read_only"`
	NoDelete []PathRule `json:"no_delete"`
}

// Protections extracts the workspace protections from a verified snapshot.
// Only a verified snapshot has one: Verify is the only thing that builds a
// Snapshot from bytes, so a set of protections is always something the
// backend signed.
func (s *Snapshot) Protections() Protections {
	var p Protections
	for _, wp := range s.Workspace {
		rules := make([]PathRule, 0, len(wp.Spec.Paths))
		for _, pattern := range wp.Spec.Paths {
			if normalizePattern(pattern) == "" {
				continue
			}
			rules = append(rules, PathRule{Pattern: pattern, PolicyID: wp.ID})
		}
		switch wp.Spec.Mode {
		case ModeExclude:
			p.Exclude = append(p.Exclude, rules...)
		case ModeReadOnly:
			p.ReadOnly = append(p.ReadOnly, rules...)
		case ModeNoDelete:
			p.NoDelete = append(p.NoDelete, rules...)
		}
		// An unknown mode is dropped rather than guessed at. A mode this
		// build does not understand is not a mode it can enforce, and
		// enforcing the wrong one is worse than enforcing none.
	}
	return p
}

// Empty reports whether no protection rules are configured.
func (p Protections) Empty() bool {
	return len(p.Exclude) == 0 && len(p.ReadOnly) == 0 && len(p.NoDelete) == 0
}

// Rules counts the path rules in force, for the line that tells a developer
// the control is on.
func (p Protections) Rules() int {
	return len(p.Exclude) + len(p.ReadOnly) + len(p.NoDelete)
}

// Excluded reports whether a path is kept out of the sandbox entirely. The
// seed walk asks this about directories too, and skips their whole subtree.
func (p Protections) Excluded(path string) bool {
	_, ok := match(p.Exclude, path)
	return ok
}

// CheckRead returns a *ProtectedError when the policies say the agent may not
// see this path, and nil when it may.
func (p Protections) CheckRead(path string) error {
	if rule, ok := match(p.Exclude, path); ok {
		return protected(path, ModeExclude, rule)
	}
	return nil
}

// CheckWrite returns a *ProtectedError when the policies say the agent may not
// change this path, and nil when it may.
func (p Protections) CheckWrite(path string) error {
	if err := p.CheckRead(path); err != nil {
		return err
	}
	if rule, ok := match(p.ReadOnly, path); ok {
		return protected(path, ModeReadOnly, rule)
	}
	return nil
}

// CheckDelete returns a *ProtectedError when the policies say the agent may
// not remove this path, and nil when it may.
func (p Protections) CheckDelete(path string) error {
	if err := p.CheckWrite(path); err != nil {
		return err
	}
	if rule, ok := match(p.NoDelete, path); ok {
		return protected(path, ModeNoDelete, rule)
	}
	return nil
}

func match(rules []PathRule, path string) (PathRule, bool) {
	path = normalizePath(path)
	if path == "" {
		return PathRule{}, false
	}
	for _, r := range rules {
		if matchPath(r.Pattern, path) {
			return r, true
		}
	}
	return PathRule{}, false
}

func protected(path, mode string, rule PathRule) error {
	return &ProtectedError{Path: path, Mode: mode, Pattern: rule.Pattern, PolicyID: rule.PolicyID}
}

// ProtectedError is a refusal by a workspace-protection policy. It is a
// decision, not a failure: the client turns it into a blocked tool result so
// the model learns the rule instead of retrying it, the same way the proxy
// answers a denied request.
type ProtectedError struct {
	Path     string
	Mode     string
	Pattern  string
	PolicyID string
}

func (e *ProtectedError) Error() string {
	switch e.Mode {
	case ModeExclude:
		return e.Path + " is excluded from the sandbox by workspace policy (" + e.Pattern + ")"
	case ModeReadOnly:
		return e.Path + " is read-only under workspace policy (" + e.Pattern + ")"
	default:
		return e.Path + " cannot be deleted under workspace policy (" + e.Pattern + ")"
	}
}

// matchPath reports whether a workspace-protection pattern covers a
// workspace-relative path.
//
// Patterns are matched segment by segment: `*` stands for any part of one
// segment, `**` for any run of segments, and nothing else is a metacharacter —
// the same deliberate plainness as the network matcher, so a pattern an
// administrator writes in the console behaves identically on every machine.
//
// A pattern that names a directory covers everything inside it: `.git` alone
// protects `.git/config`, because an administrator writing `.git` means the
// directory and everything it holds, and a rule that protected only the empty
// name would protect nothing at all.
//
// Matching is case-insensitive, on every platform. macOS and Windows resolve
// `.SECURE` to the same file as `.secure`, so a case-sensitive rule would hold
// on Linux and quietly fail to hold on the two platforms most developers run:
// the sandbox is Linux and would call the two names different files, and the
// host would then apply one over the other. A control that means different
// things on different machines is not a deterministic control. The cost is
// that on a case-sensitive filesystem the neighbouring spelling is protected
// too, which is the safe direction to be wrong in and the one the rest of the
// policy engine takes when rules conflict.
func matchPath(pattern, path string) bool {
	pat := strings.ToLower(normalizePattern(pattern))
	if pat == "" {
		return false
	}
	pp := strings.Split(pat, "/")
	segs := strings.Split(strings.ToLower(path), "/")
	// Try the path itself, then each ancestor directory: a pattern matching
	// an ancestor covers the whole subtree beneath it.
	for i := len(segs); i > 0; i-- {
		if matchSegments(pp, segs[:i]) {
			return true
		}
	}
	return false
}

func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if !matchGlob(pat[0], segs[0]) {
		return false
	}
	return matchSegments(pat[1:], segs[1:])
}

// normalizePattern reduces the ways of writing the same rule to one. A
// trailing `/**` says "everything under this", which is what naming the
// directory already says, so both spellings land on the same pattern.
func normalizePattern(pattern string) string {
	p := normalizePath(pattern)
	p = strings.TrimSuffix(p, "/**")
	p = strings.TrimSuffix(p, "/*")
	return strings.TrimSuffix(p, "/")
}

func normalizePath(path string) string {
	p := strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return strings.TrimSuffix(p, "/")
}
