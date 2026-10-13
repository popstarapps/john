package policy

import "testing"

func TestBuildNetworkDefaultsToDirectAndFailsClosedOnAValueItCannotCarryOut(t *testing.T) {
	cases := []struct {
		field   string
		want    string
		proxied bool
	}{
		// A backend that predates the field is not making a claim about
		// builds; the behaviour it shipped with is what its administrators
		// expect.
		{"", BuildDirect, false},
		{BuildDirect, BuildDirect, false},
		{BuildProxied, BuildProxied, true},
		// An instruction this build cannot carry out is not a licence to open
		// the network — including "observed", once offered and never built,
		// which is what it always did in practice.
		{"observed", BuildProxied, true},
		{"whatever-comes-next", BuildProxied, true},
		{"DIRECT", BuildProxied, true},
	}
	for _, c := range cases {
		snap := &Snapshot{Build: c.field}
		if got := snap.BuildNetwork(); got != c.want {
			t.Errorf("Build=%q → %q, want %q", c.field, got, c.want)
		}
		if got := snap.BuildIsProxied(); got != c.proxied {
			t.Errorf("Build=%q → proxied %v, want %v", c.field, got, c.proxied)
		}
	}
}
