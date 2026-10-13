package proxy

import "testing"

// A port the scheme implies is not written into the URL. CONNECT always names
// one, so HTTPS requests read https://host:443/… beside plain http://host/….
func TestTheSchemesOwnPortIsLeftOut(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"registry.npmjs.org:443", "https"}:  "registry.npmjs.org",
		{"registry.npmjs.org:8443", "https"}: "registry.npmjs.org:8443",
		{"deb.debian.org:80", "http"}:        "deb.debian.org",
		{"deb.debian.org:443", "http"}:       "deb.debian.org:443",
		{"[::1]:443", "https"}:               "[::1]",
		{"example.com", "https"}:             "example.com",
	} {
		if got := withoutDefaultPort(in[0], in[1]); got != want {
			t.Errorf("withoutDefaultPort(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// A proposed policy names a port only when it is not the scheme's own, so a
// policy for an HTTPS host reads, and matches, like one for an HTTP host.
func TestAProposalLeavesTheDefaultPortOut(t *testing.T) {
	spec, err := ProposedPolicy(Outcome{Method: "GET", URL: "https://registry.npmjs.org:443/react"}, false)
	if err != nil || spec.Port != nil || spec.Host != "registry.npmjs.org" {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	spec, _ = ProposedPolicy(Outcome{Method: "GET", URL: "https://example.com:8443/x"}, false)
	if spec.Port == nil || *spec.Port != 8443 {
		t.Fatalf("a non-default port was dropped: %+v", spec)
	}
}
