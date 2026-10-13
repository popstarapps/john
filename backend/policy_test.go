package backend

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/popstarapps/john/store"
)

// TestSigningKeyRefusesUnusableKeys pins the guard in front of ed25519, which
// panics on a wrong-sized key. An org row carries an empty signing key until
// setup writes one, and reaching Public() on that empty key took down the
// handler serving /api/v1/policy/signing-key.
func TestSigningKeyRefusesUnusableKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"never set up", ""},
		{"not base64", "!!!not base64!!!"},
		{"right encoding, wrong length", base64.StdEncoding.EncodeToString([]byte("too short"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := signingKey(&store.Org{SigningKey: tc.key})
			if err == nil {
				t.Fatalf("signingKey(%q) returned a key, want an error", tc.key)
			}
			if key != nil {
				t.Fatalf("signingKey(%q) returned %d bytes alongside its error", tc.key, len(key))
			}
		})
	}
}

func TestSigningKeyAcceptsARealKey(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	got, err := signingKey(&store.Org{SigningKey: base64.StdEncoding.EncodeToString(priv)})
	if err != nil {
		t.Fatalf("signingKey: %v", err)
	}
	// The panic was on Public(); this is the call the handler makes.
	if !got.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("public half does not match the generated key")
	}
}

// TestSigningPublicKeyOnUnsetUpOrgAnswers is the handler-level half: an org
// with no key must produce a reply, not a panicking goroutine.
func TestSigningPublicKeyOnUnsetUpOrgAnswers(t *testing.T) {
	srv := testServer(t)
	if err := srv.Store().SaveOrg(&store.Org{Name: "No Key", AdminEmail: "a@b.c"}); err != nil {
		t.Fatal(err)
	}
	status, body, _ := get(t, "http://"+srv.Addr()+"/api/v1/policy/signing-key")
	if status == 0 {
		t.Fatal("no response at all — the handler died")
	}
	if status == 200 && strings.Contains(body, "public_key") {
		t.Fatalf("an org with no signing key served one: %s", body)
	}
}
