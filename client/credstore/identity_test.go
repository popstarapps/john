package credstore

import "testing"

// Moved here from client/trace_test.go, which is e2e and therefore !windows:
// neither test needs a session, and the identity is a Windows file too.

// TestTheMachineIdentityIsStable: signing in again must not re-key, because a
// new key silently costs the developer every session they could read.
func TestTheMachineIdentityIsStable(t *testing.T) {
	machine(t)

	first, err := EnsureIdentity()
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if first.Private != second.Private || first.Recipient != second.Recipient {
		t.Fatal("EnsureIdentity re-keyed a machine that already had an identity")
	}

	loaded, err := LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Recipient != first.Recipient {
		t.Fatal("the stored identity is not the one that was generated")
	}

	p, err := IdentityPath()
	if err != nil {
		t.Fatal(err)
	}
	// The private half of the trace key: nobody else on this machine may read
	// it. How that is spelled is the platform's business.
	onlyThisUserCanRead(t, p)
}

func TestNoIdentityIsNotAnError(t *testing.T) {
	machine(t)
	if _, err := LoadIdentity(); err != ErrNoIdentity {
		t.Fatalf("LoadIdentity on a fresh machine = %v, want ErrNoIdentity", err)
	}
}
