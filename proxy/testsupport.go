package proxy

import (
	"crypto/ed25519"
	"time"

	"github.com/popstarapps/john/policy"
)

// TestSnapshot builds a compiled snapshot from network policies, for tests in
// other packages that drive the proxy. It signs and verifies with a throwaway
// key so the compiled matchers are populated exactly as in production.
func TestSnapshot(policies []policy.NetworkPolicy) *policy.Snapshot {
	pub, priv, _ := ed25519.GenerateKey(nil)
	signed, err := policy.Sign(&policy.Snapshot{
		IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Network: policies,
	}, priv)
	if err != nil {
		panic(err)
	}
	snap, err := policy.Verify(signed, pub)
	if err != nil {
		panic(err)
	}
	return snap
}
