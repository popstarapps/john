package backend

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/store"
)

// snapshotLifetime is how long a policy snapshot is honoured before it must be
// refreshed. Short enough that a policy change reaches machines quickly, long
// enough that a brief backend outage does not interrupt an in-flight command.
const snapshotLifetime = 5 * time.Minute

// buildSnapshot assembles the signed policy snapshot for a user: the network
// and workspace policies from every group that applies to them, compiled and
// signed with the org's key. The proxy pulls this and decides alone.
func (s *Server) buildSnapshot(user *store.User) (*policy.SignedSnapshot, error) {
	org, err := s.store.Org()
	if err != nil {
		return nil, err
	}
	priv, err := signingKey(org)
	if err != nil {
		return nil, err
	}
	policies, _, err := s.store.PoliciesForUser(user.ID)
	if err != nil {
		return nil, err
	}
	epoch, err := s.store.PolicyEpoch()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	snap := &policy.Snapshot{
		Epoch:     epoch,
		IssuedAt:  now,
		ExpiresAt: now.Add(snapshotLifetime),
		// Where an image build's traffic goes is an organization-wide
		// decision rather than a group's, so it comes from the org row rather
		// than from the policies that apply to this person. It travels signed
		// with the rest: a machine that could choose its own answer would be
		// choosing whether the proxy sees its build at all.
		Build: org.BuildNetwork,
		// And what a prompt may carry besides text, for the same reason: an
		// attachment is content on its way to a model that john's text
		// controls cannot read, so whether one may be sent at all is the
		// organization's answer and not the machine's.
		Attach: org.Attachments,
	}
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		switch p.Type {
		case "network":
			var spec policy.NetworkSpec
			if json.Unmarshal(p.Spec, &spec) == nil {
				snap.Network = append(snap.Network, policy.NetworkPolicy{ID: p.ID, Spec: spec})
			}
		case "workspace":
			var spec policy.WorkspaceSpec
			if json.Unmarshal(p.Spec, &spec) == nil {
				snap.Workspace = append(snap.Workspace, policy.WorkspacePolicy{ID: p.ID, Spec: spec})
			}
		case "replacement":
			snap.ReplacementsActive = true
		}
	}
	return policy.Sign(snap, priv)
}

func (s *Server) handlePolicySnapshot(w http.ResponseWriter, r *http.Request) {
	user := s.requireUser(w, r)
	if user == nil {
		return
	}
	signed, err := s.buildSnapshot(user)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, signed)
}

// handleSigningPublicKey serves the org's snapshot-signing public key, so a
// client can pin it at login. The private half never leaves the database.
func (s *Server) handleSigningPublicKey(w http.ResponseWriter, r *http.Request) {
	if s.requireUser(w, r) == nil {
		return
	}
	org, err := s.store.Org()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	priv, err := signingKey(org)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	pub := priv.Public().(ed25519.PublicKey)
	writeJSON(w, http.StatusOK, map[string]string{
		"public_key": base64.StdEncoding.EncodeToString(pub),
	})
}

func signingKey(org *store.Org) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(org.SigningKey)
	if err != nil {
		return nil, err
	}
	// ed25519 panics on a wrong-sized key, and the empty string is what an org
	// row carries until something writes a key into it. Refusing here turns a
	// crashed handler into an answer the caller can act on — and a client that
	// cannot pin a key holds no snapshot, which the proxy treats as deny-all.
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("org has no snapshot signing key: %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// keywordRules extracts the mid-session narrowing rules from a user's routing
// policies: restrict-mode policies that carry keywords.
func (s *Server) keywordRules(userID string) ([]policy.KeywordRule, error) {
	policies, groups, err := s.store.PoliciesForUser(userID)
	if err != nil {
		return nil, err
	}
	groupName := map[string]string{}
	for _, g := range groups {
		groupName[g.ID] = g.Name
	}
	var out []policy.KeywordRule
	for _, p := range policies {
		if p.Type != "model-routing" || !p.Enabled {
			continue
		}
		var spec policy.ModelRoutingSpec
		if json.Unmarshal(p.Spec, &spec) != nil {
			continue
		}
		if spec.Mode != "restrict" || len(spec.Keywords) == 0 {
			continue
		}
		out = append(out, policy.KeywordRule{
			PolicyID: p.ID, Group: groupName[p.GroupID],
			Keywords: spec.Keywords, ModelIDs: spec.ModelIDs,
		})
	}
	return out, nil
}

// replacementSpecs collects the replacement policies that apply to a user.
func (s *Server) replacementSpecs(userID string) ([]policy.ReplacementSpec, error) {
	policies, _, err := s.store.PoliciesForUser(userID)
	if err != nil {
		return nil, err
	}
	var out []policy.ReplacementSpec
	for _, p := range policies {
		if p.Type != "replacement" || !p.Enabled {
			continue
		}
		var spec policy.ReplacementSpec
		if json.Unmarshal(p.Spec, &spec) == nil {
			out = append(out, spec)
		}
	}
	return out, nil
}

// verifyWith is a test helper: verify a signed snapshot with the private key's
// public half.
func verifyWith(signed *policy.SignedSnapshot, priv ed25519.PrivateKey) (*policy.Snapshot, error) {
	return policy.Verify(signed, priv.Public().(ed25519.PublicKey))
}
