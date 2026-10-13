package store

import "testing"

// The organization row is the whole of the backend's configuration — there is
// no file — so a setting that did not survive being written and read back
// would be a setting an administrator set once and lost.
func TestTheBuildSettingIsKeptOnTheOrganization(t *testing.T) {
	s := openTestStore(t)
	if err := s.SaveOrg(&Org{Name: "Acme", AdminEmail: "a@b.c", BuildNetwork: "proxied"}); err != nil {
		t.Fatal(err)
	}
	org, err := s.Org()
	if err != nil {
		t.Fatal(err)
	}
	if org.BuildNetwork != "proxied" {
		t.Fatalf("build network came back %q", org.BuildNetwork)
	}

	// Empty is the default an organization starts on, and it is a real value
	// rather than a missing one: the client reads it as direct.
	if err := s.SaveOrg(&Org{Name: "Acme", AdminEmail: "a@b.c"}); err != nil {
		t.Fatal(err)
	}
	if org, err = s.Org(); err != nil || org.BuildNetwork != "" {
		t.Fatalf("build network came back %q, %v", org.BuildNetwork, err)
	}
}

func TestAllowedDomainsAreKeptOnTheOrganization(t *testing.T) {
	s := openTestStore(t)
	if err := s.SaveOrg(&Org{Name: "Acme", AdminEmail: "a@acme.com", AllowedDomains: []string{"acme.com", "acme.dk"}}); err != nil {
		t.Fatal(err)
	}
	org, err := s.Org()
	if err != nil {
		t.Fatal(err)
	}
	if len(org.AllowedDomains) != 2 || org.AllowedDomains[0] != "acme.com" || org.AllowedDomains[1] != "acme.dk" {
		t.Fatalf("allowed domains came back %q", org.AllowedDomains)
	}

	// None is a real answer too: any domain, which is what an organization
	// set up before the setting existed runs with.
	org.AllowedDomains = nil
	if err := s.SaveOrg(org); err != nil {
		t.Fatal(err)
	}
	if org, err = s.Org(); err != nil || len(org.AllowedDomains) != 0 {
		t.Fatalf("allowed domains came back %q, %v", org.AllowedDomains, err)
	}
}
