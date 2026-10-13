package policy

import (
	"crypto/ed25519"
	"net/url"
	"testing"
	"time"
)

func req(method, rawURL string) Request {
	u, _ := url.Parse(rawURL)
	return Request{Method: method, URL: u, Headers: map[string][]string{}}
}

func snap(policies ...NetworkPolicy) *Snapshot {
	s := &Snapshot{Network: policies}
	s.compile()
	return s
}

func net(id string, spec NetworkSpec) NetworkPolicy { return NetworkPolicy{ID: id, Spec: spec} }

func TestEvaluateVerdictsAndReasons(t *testing.T) {
	port443 := 443
	s := snap(
		net("p1", NetworkSpec{Kind: "network", Methods: []string{"GET"}, Scheme: "https",
			Host: "proxy.golang.org", Path: "/*", Decision: DecisionAllow}),
		net("p2", NetworkSpec{Kind: "network", Methods: []string{"POST"}, Scheme: "https",
			Host: "telemetry.vendor.io", Decision: DecisionDeny}),
		net("p3", NetworkSpec{Kind: "network", Scheme: "https", Host: "api.github.com",
			Path: "/repos/*", Decision: DecisionAsk, Port: &port443}),
	)

	cases := []struct {
		method, url string
		want        Verdict
	}{
		{"GET", "https://proxy.golang.org/github.com/x/y/@v/list", VerdictAllow},
		{"POST", "https://telemetry.vendor.io/collect", VerdictDeny},
		{"GET", "https://api.github.com/repos/acme/app/contents", VerdictAsk},
		{"GET", "https://unknown.example.com/", VerdictUnknown},
		{"DELETE", "https://proxy.golang.org/x", VerdictUnknown}, // method excludes it
	}
	for _, c := range cases {
		d := s.Evaluate(req(c.method, c.url))
		if d.Verdict != c.want {
			t.Errorf("%s %s → %s, want %s", c.method, c.url, d.Verdict, c.want)
		}
		// Every decision carries a non-empty reason — the property the old
		// TestPolicyEvaluate asserted, kept.
		if d.Reason == "" {
			t.Errorf("%s %s → empty reason", c.method, c.url)
		}
	}
}

func TestMostRestrictiveWins(t *testing.T) {
	// A broad allow and a narrow deny both match; deny wins.
	s := snap(
		net("allow-all", NetworkSpec{Kind: "network", Host: "*.internal", Decision: DecisionAllow}),
		net("deny-secrets", NetworkSpec{Kind: "network", Host: "secrets.internal", Decision: DecisionDeny}),
	)
	if d := s.Evaluate(req("GET", "https://secrets.internal/vault")); d.Verdict != VerdictDeny {
		t.Fatalf("verdict = %s, want deny (most restrictive)", d.Verdict)
	}
	if d := s.Evaluate(req("GET", "https://cache.internal/x")); d.Verdict != VerdictAllow {
		t.Fatalf("verdict = %s, want allow", d.Verdict)
	}
}

func TestHostWildcardAndPath(t *testing.T) {
	s := snap(net("p", NetworkSpec{Kind: "network", Host: "*.googleapis.com",
		Path: "/v1/*", Decision: DecisionAllow}))
	if s.Evaluate(req("GET", "https://storage.googleapis.com/v1/b/obj")).Verdict != VerdictAllow {
		t.Fatal("subdomain+path should match")
	}
	if s.Evaluate(req("GET", "https://googleapis.com/v1/x")).Verdict != VerdictUnknown {
		t.Fatal("apex should not match *.googleapis.com")
	}
	if s.Evaluate(req("GET", "https://storage.googleapis.com/v2/x")).Verdict != VerdictUnknown {
		t.Fatal("wrong path should not match")
	}
}

func TestHeaderAndBodySizeMatching(t *testing.T) {
	max := 1024
	s := snap(net("p", NetworkSpec{Kind: "network", Host: "api.example.com",
		Decision: DecisionAllow, MaxBodyBytes: &max,
		HeaderMatches: []AttributeMatch{{Name: "X-Env", Value: "prod"}}}))

	r := req("POST", "https://api.example.com/x")
	r.Headers = map[string][]string{"X-Env": {"prod"}}
	r.BodySize = 500
	if s.Evaluate(r).Verdict != VerdictAllow {
		t.Fatal("matching header and small body should allow")
	}
	r.BodySize = 5000 // over the cap → the policy no longer matches → unknown
	if s.Evaluate(r).Verdict != VerdictUnknown {
		t.Fatal("oversized body must not be allowed by a body-capped policy")
	}
	r.BodySize = 500
	r.Headers = map[string][]string{"X-Env": {"staging"}}
	if s.Evaluate(r).Verdict != VerdictUnknown {
		t.Fatal("wrong header value must not match")
	}
}

func TestSnapshotSignVerifyAndExpiry(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	original := &Snapshot{
		Epoch: 7, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute),
		Network: []NetworkPolicy{net("p", NetworkSpec{Kind: "network", Host: "x.com", Decision: DecisionAllow})},
	}
	signed, err := Sign(original, priv)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Verify(signed, pub)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Epoch != 7 || len(got.network) != 1 {
		t.Fatalf("verified snapshot = %+v", got)
	}
	if got.Evaluate(req("GET", "https://x.com/")).Verdict != VerdictAllow {
		t.Fatal("verified snapshot should evaluate")
	}

	// A tampered snapshot does not verify.
	tampered := *signed
	body := append([]byte(nil), tampered.Snapshot...)
	body[len(body)/2] ^= 0x40
	tampered.Snapshot = body
	if _, err := Verify(&tampered, pub); err != ErrBadSignature {
		t.Fatalf("tampered verify = %v, want ErrBadSignature", err)
	}

	// The wrong key does not verify.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if _, err := Verify(signed, otherPub); err != ErrBadSignature {
		t.Fatalf("wrong-key verify = %v, want ErrBadSignature", err)
	}

	// Expiry and refresh timing.
	if got.Expired(now.Add(5 * time.Minute)) {
		t.Fatal("not expired at 5m of a 10m life")
	}
	if !got.Expired(now.Add(11 * time.Minute)) {
		t.Fatal("must be expired past its life")
	}
	if got.RefreshDue(now.Add(5 * time.Minute)) {
		t.Fatal("refresh at 50% is too early")
	}
	if !got.RefreshDue(now.Add(7 * time.Minute)) {
		t.Fatal("refresh should be due past 60%")
	}
}

func TestReplacerStableAliases(t *testing.T) {
	r, err := NewReplacer([]ReplacementSpec{
		{Kind: "replacement", Pattern: `ACCT-\d+`, Replacement: "ACCT-#"},
		{Kind: "replacement", Pattern: `sk-[a-z0-9]+`, Replacement: "[redacted-key]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Active() {
		t.Fatal("replacer should be active")
	}

	out := r.Apply("charge ACCT-4491 and ACCT-9000, key sk-abc123")
	// Same value → same alias; different values → different aliases.
	out2 := r.Apply("refund ACCT-4491")
	if !contains(out, "ACCT-1") || !contains(out, "ACCT-2") {
		t.Fatalf("aliases not minted: %q", out)
	}
	firstAlias := aliasOf(out, "4491-equivalent") // find which alias 4491 got
	_ = firstAlias
	// 4491 appeared first, so it is ACCT-1, and it stays ACCT-1 next turn.
	if !contains(out2, "ACCT-1") {
		t.Fatalf("alias not stable across turns: %q", out2)
	}
	// The keyless replacement collapses to a fixed string.
	if contains(out, "sk-abc123") {
		t.Fatal("secret key leaked through")
	}

	// ContainsAlias catches an edit that touches a redacted region.
	if _, ok := r.ContainsAlias("please edit the line with ACCT-1 in it"); !ok {
		t.Fatal("an alias in tool args must be detectable")
	}
	if _, ok := r.ContainsAlias("nothing sensitive here"); ok {
		t.Fatal("clean text must not report an alias")
	}
}

func TestRouterKeywordNarrowing(t *testing.T) {
	r := NewRouter([]KeywordRule{
		{PolicyID: "p1", Group: "payments-restricted", Keywords: []string{"C++", "cardholder"},
			ModelIDs: []string{"internal/codegen-l"}},
	})
	if !r.HasRules() {
		t.Fatal("router should have rules")
	}
	already := map[string]bool{}

	if n := r.Match("this is plain go code", already); n != nil {
		t.Fatal("no keyword, no narrowing")
	}
	n := r.Match("here is some C++ in bridge.cc", already)
	if n == nil || n.Keyword != "C++" || len(n.ModelIDs) != 1 {
		t.Fatalf("narrowing = %+v", n)
	}
	// Once a policy has narrowed, it does not fire again.
	already[n.PolicyID] = true
	if again := r.Match("more C++ later", already); again != nil {
		t.Fatal("a session never un-narrows: the same policy must not fire twice")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func aliasOf(s, _ string) string { return s }
