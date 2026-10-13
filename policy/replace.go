package policy

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Replacer applies replacement policies to content on its way to a model, and
// keeps the alias table that makes the substitution reversible and stable.
//
// A `#` in a replacement template becomes a stable alias per distinct matched
// value: the same secret always becomes the same alias, and two different
// secrets never collide. The model can still reason about "the account in the
// stack trace" without the real value ever leaving the organization.
//
// A Replacer is per-session: aliases must stay consistent across every turn of
// one conversation, and must not bleed between conversations.
type Replacer struct {
	rules []replaceRule

	mu      sync.Mutex
	aliases map[string]string // real value → alias
	reverse map[string]string // alias → real value
	counter int
}

type replaceRule struct {
	re       *regexp.Regexp
	template string
}

// NewReplacer compiles replacement specs. A spec whose pattern does not
// compile is skipped rather than failing the session — a broken policy must
// not become a way to stop all work — but that is worth surfacing, so the
// error is returned for logging alongside a usable Replacer.
func NewReplacer(specs []ReplacementSpec) (*Replacer, error) {
	r := &Replacer{aliases: map[string]string{}, reverse: map[string]string{}}
	err := r.SetRules(specs)
	return r, err
}

// SetRules replaces the rules and keeps every alias already minted, so a
// policy changed in the middle of a conversation applies from the next
// request without one value reaching the model under two names.
func (r *Replacer) SetRules(specs []ReplacementSpec) error {
	var rules []replaceRule
	var firstErr error
	for _, s := range specs {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("policy: replacement pattern %q: %w", s.Pattern, err)
			}
			continue
		}
		rules = append(rules, replaceRule{re: re, template: s.Replacement})
	}
	r.mu.Lock()
	r.rules = rules
	r.mu.Unlock()
	return firstErr
}

// Active reports whether any replacement rule is in effect.
func (r *Replacer) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rules) > 0
}

// Apply rewrites text, replacing every match and recording aliases. The same
// matched value always yields the same alias within this Replacer.
func (r *Replacer) Apply(text string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rules) == 0 {
		return text
	}
	for _, rule := range r.rules {
		text = rule.re.ReplaceAllStringFunc(text, func(match string) string {
			return r.aliasFor(match, rule.template)
		})
	}
	return text
}

// aliasFor returns the replacement for a matched value, minting a stable alias
// where the template contains `#`. Caller holds the lock.
func (r *Replacer) aliasFor(match, template string) string {
	if !strings.Contains(template, "#") {
		return template
	}
	if existing, ok := r.aliases[match]; ok {
		return existing
	}
	r.counter++
	alias := strings.ReplaceAll(template, "#", fmt.Sprintf("%d", r.counter))
	r.aliases[match] = alias
	r.reverse[alias] = match
	return alias
}

// ContainsAlias reports whether text carries any alias this Replacer has
// minted. The backend uses it to refuse an edit whose arguments touch redacted
// content — the model only ever saw the alias, so an edit against it would be
// writing a secret back on the strength of the model's output.
func (r *Replacer) ContainsAlias(text string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for alias := range r.reverse {
		if strings.Contains(text, alias) {
			return alias, true
		}
	}
	return "", false
}
