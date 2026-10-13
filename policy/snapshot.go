package policy

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Snapshot is the signed, expiring bundle of policy the proxy pulls and
// enforces locally. It never asks the backend about an individual request:
// everything needed to decide is here, verified once, and used until it
// expires. Past expiry every decision-needing request blocks — fail closed by
// expiry, which holds even when the backend is up and the clock drifted.
type Snapshot struct {
	Epoch              uint64            `json:"epoch"`
	IssuedAt           time.Time         `json:"issued_at"`
	ExpiresAt          time.Time         `json:"expires_at"`
	Network            []NetworkPolicy   `json:"network"`
	Workspace          []WorkspacePolicy `json:"workspace"`
	ReplacementsActive bool              `json:"replacements_active"`
	// Build is where an image build's network traffic goes: direct or
	// proxied. See build.go; unset means direct.
	Build string `json:"build,omitempty"`
	// Attach is what a prompt may carry besides text: guarded, on, or off.
	// See attachments.go; unset means guarded.
	Attach string `json:"attachments,omitempty"`

	// compiled, not serialized.
	network []compiledNetwork
}

// NetworkPolicy is one network policy in the snapshot: its id and spec.
type NetworkPolicy struct {
	ID   string      `json:"id"`
	Spec NetworkSpec `json:"spec"`
}

// WorkspacePolicy is one workspace-protection policy.
type WorkspacePolicy struct {
	ID   string        `json:"id"`
	Spec WorkspaceSpec `json:"spec"`
}

// SignedSnapshot is what crosses the wire: the snapshot bytes and a signature
// over them. The signature is about the cache — TLS already authenticates the
// fetch, but a snapshot loosened on disk is not one the proxy will honour.
type SignedSnapshot struct {
	Snapshot json.RawMessage `json:"snapshot"`
	Sig      string          `json:"sig"` // base64 Ed25519 over the snapshot bytes
}

// Sign serializes and signs a snapshot with the backend's private key.
func Sign(snap *Snapshot, priv ed25519.PrivateKey) (*SignedSnapshot, error) {
	body, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, body)
	return &SignedSnapshot{Snapshot: body, Sig: base64.StdEncoding.EncodeToString(sig)}, nil
}

// ErrBadSignature means the snapshot's signature did not verify against the
// pinned key — it will not be honoured.
var ErrBadSignature = errors.New("policy: snapshot signature does not verify")

// Verify checks the signature against the pinned public key and returns the
// compiled snapshot. A snapshot that does not verify is never returned.
func Verify(signed *SignedSnapshot, pub ed25519.PublicKey) (*Snapshot, error) {
	sig, err := base64.StdEncoding.DecodeString(signed.Sig)
	if err != nil {
		return nil, fmt.Errorf("policy: bad signature encoding: %w", err)
	}
	if !ed25519.Verify(pub, signed.Snapshot, sig) {
		return nil, ErrBadSignature
	}
	var snap Snapshot
	if err := json.Unmarshal(signed.Snapshot, &snap); err != nil {
		return nil, err
	}
	snap.compile()
	return &snap, nil
}

func (s *Snapshot) compile() {
	s.network = s.network[:0]
	for _, p := range s.Network {
		s.network = append(s.network, compiledNetwork{policyID: p.ID, spec: p.Spec})
	}
}

// Expired reports whether the snapshot may no longer be trusted.
func (s *Snapshot) Expired(now time.Time) bool {
	return now.After(s.ExpiresAt)
}

// RefreshDue reports whether the client should pull a new snapshot — at 60% of
// the lifetime, so a brief backend outage never interrupts an in-flight
// command but a stale snapshot is refreshed well before it dies.
func (s *Snapshot) RefreshDue(now time.Time) bool {
	life := s.ExpiresAt.Sub(s.IssuedAt)
	return now.After(s.IssuedAt.Add(life * 6 / 10))
}
