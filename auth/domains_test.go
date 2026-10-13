package auth

import (
	"strings"
	"testing"
)

func TestAdmission(t *testing.T) {
	acme := []string{"acme.com"}
	cases := []struct {
		name    string
		issuer  string
		allowed []string
		claims  idClaims
		refused string // a fragment of the reason, or empty to admit
	}{
		{"no list admits any domain", "https://idp.example", nil,
			idClaims{Email: "x@anywhere.org"}, ""},
		{"an unverified address is refused even with no list", "https://idp.example", nil,
			idClaims{Email: "x@acme.com", EmailVerified: false}, "not verified"},
		{"the string false is unverified too", "https://idp.example", nil,
			idClaims{Email: "x@acme.com", EmailVerified: "false"}, "not verified"},
		{"a provider that says nothing is not held against anyone", "https://idp.example", acme,
			idClaims{Email: "x@acme.com"}, ""},
		{"a listed domain is admitted", "https://idp.example", acme,
			idClaims{Email: "x@ACME.com", EmailVerified: true}, ""},
		{"another domain is refused", "https://idp.example", acme,
			idClaims{Email: "x@evil.com", EmailVerified: true}, "not in a domain"},
		{"a subdomain is another domain", "https://idp.example", acme,
			idClaims{Email: "x@eu.acme.com", EmailVerified: true}, "not in a domain"},
		{"a Workspace account of the domain is admitted", googleIssuer, acme,
			idClaims{Email: "x@acme.com", EmailVerified: true, HostedDomain: "acme.com"}, ""},
		{"a consumer Google account on a work address is refused", googleIssuer, acme,
			idClaims{Email: "x@acme.com", EmailVerified: true}, "Google Workspace"},
		{"a gmail account is refused", googleIssuer, acme,
			idClaims{Email: "x@gmail.com", EmailVerified: true}, "not in a domain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := admit(c.issuer, c.allowed, c.claims)
			switch {
			case c.refused == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.refused != "" && err == nil:
				t.Fatal("admitted")
			case c.refused != "" && !strings.Contains(err.Error(), c.refused):
				t.Fatalf("refused for the wrong reason: %v", err)
			}
		})
	}
}

func TestNormalizeDomains(t *testing.T) {
	got, err := NormalizeDomains([]string{" @Acme.com ", "", "acme.dk", "acme.com"})
	if err != nil || strings.Join(got, ",") != "acme.com,acme.dk" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"acme", "a b.com", "x@acme.com", "https://acme.com", ".acme.com"} {
		if _, err := NormalizeDomains([]string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
