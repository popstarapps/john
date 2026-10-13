package auth

import (
	"errors"
	"fmt"
	"strings"
)

// Who may sign in is the identity provider's answer narrowed by john's own.
// The provider says who a person is; it does not say they belong to this
// organization, and how wide it answers is a setting somebody may have gotten
// wrong: a Google client left on External vouches for every Google account.
// So an organization names the email domains it admits, and sign-in holds the
// ID token to them before anyone is created or matched.

// googleIssuer is the one provider whose email domain does not settle
// membership. A consumer Google account can be registered on any address,
// a work one included; only the hd claim says the account belongs to the
// Workspace that owns the domain.
const googleIssuer = "https://accounts.google.com"

// idClaims is what admission reads from a verified ID token.
type idClaims struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	// EmailVerified is a bool from most providers and the string "true" from
	// some; absent from others (Entra), which say nothing either way.
	EmailVerified any    `json:"email_verified"`
	HostedDomain  string `json:"hd"`
}

// admit says why claims from issuer may not sign in to an organization that
// allows domains, or nil when they may. An empty allowed list admits any
// domain.
func admit(issuer string, allowed []string, c idClaims) error {
	if unverified(c.EmailVerified) {
		return errors.New("your identity provider has not verified the email address " + c.Email)
	}
	if len(allowed) == 0 {
		return nil
	}
	domain := emailDomain(c.Email)
	if !contains(allowed, domain) {
		return fmt.Errorf("%s is not in a domain this organization allows", c.Email)
	}
	if strings.TrimSuffix(issuer, "/") == googleIssuer && !contains(allowed, strings.ToLower(c.HostedDomain)) {
		return fmt.Errorf("%s is not a Google Workspace account of a domain this organization allows", c.Email)
	}
	return nil
}

// unverified is true only when the provider said outright that the address
// is not verified.
func unverified(v any) bool {
	switch v := v.(type) {
	case bool:
		return !v
	case string:
		return strings.EqualFold(v, "false")
	}
	return false
}

func emailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// NormalizeDomains cleans an administrator's list of domains: lowercase, no
// @, no blanks or repeats. It refuses anything that is not a domain, because
// a typo here is a door that is quietly shut or quietly open.
func NormalizeDomains(in []string) ([]string, error) {
	var out []string
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(d, "@")
		if d == "" {
			continue
		}
		if !strings.Contains(d, ".") || strings.ContainsAny(d, " ,@/:") ||
			strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
			return nil, fmt.Errorf("%q is not a domain", d)
		}
		if !contains(out, d) {
			out = append(out, d)
		}
	}
	return out, nil
}

// DomainOf is the domain of an email address, as NormalizeDomains spells it.
func DomainOf(email string) string { return emailDomain(email) }
