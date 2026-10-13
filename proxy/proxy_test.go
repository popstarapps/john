package proxy

import (
	"bufio"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/popstarapps/john/policy"
)

func testCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func net1(id, host string, d policy.NetworkDecision) policy.NetworkPolicy {
	return policy.NetworkPolicy{ID: id, Spec: policy.NetworkSpec{
		Kind: "network", Host: host, Decision: d}}
}

// directSnapshot builds a snapshot through sign/verify so its compiled
// matchers are populated, exactly as the proxy would receive one.
func directSnapshot(t *testing.T, policies ...policy.NetworkPolicy) *policy.Snapshot {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	signed, err := policy.Sign(&policy.Snapshot{
		IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Network: policies,
	}, priv)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := policy.Verify(signed, pub)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// originRoots is the cert pool trusting a test origin's self-signed cert.
func originRoots(origin *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	return pool
}

// clientThroughProxy builds an http.Client whose traffic goes through the
// proxy and which trusts the sandbox CA, exactly as a sandbox would.
func clientThroughProxy(proxyAddr string, ca *CA) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: proxyAddr}),
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
}

func startProxy(t *testing.T, cfg Config) string {
	t.Helper()
	p := New(cfg)
	addr, err := p.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	t.Cleanup(func() { p.Close() })
	return addr
}

func TestProxyVerdicts(t *testing.T) {
	// A real HTTPS origin behind the proxy.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from origin")
	}))
	defer origin.Close()
	originURL, _ := url.Parse(origin.URL)
	host := originURL.Hostname()

	// The proxy must reach the httptest origin's self-signed cert.
	ca := testCA(t)

	var mu sync.Mutex
	var outcomes []Outcome
	report := func(o Outcome) { mu.Lock(); outcomes = append(outcomes, o); mu.Unlock() }

	var approveNext bool
	approve := func(policy.Request, string) bool { return approveNext }

	var snap *policy.Snapshot
	addr := startProxy(t, Config{
		CA:            ca,
		Snapshot:      func() *policy.Snapshot { return snap },
		Approve:       approve,
		Report:        report,
		UpstreamRoots: originRoots(origin),
	})
	// The sandbox-side client trusts the proxy's CA, as a sandbox would.
	client := clientThroughProxy(addr, ca)

	t.Run("allow forwards", func(t *testing.T) {
		snap = directSnapshot(t, net1("p", host, policy.DecisionAllow))
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "hello from origin" {
			t.Fatalf("allow → %d %q", resp.StatusCode, body)
		}
	})

	t.Run("deny refuses", func(t *testing.T) {
		snap = directSnapshot(t, net1("p", host, policy.DecisionDeny))
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("deny → %d, want 403", resp.StatusCode)
		}
	})

	t.Run("unknown blocks", func(t *testing.T) {
		snap = directSnapshot(t) // nothing matches
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("unknown → %d, want 403", resp.StatusCode)
		}
	})

	t.Run("ask honours the developer", func(t *testing.T) {
		snap = directSnapshot(t, net1("p", host, policy.DecisionAsk))
		approveNext = false
		resp, _ := client.Get(origin.URL)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("ask+reject → %d", resp.StatusCode)
		}
		resp.Body.Close()

		approveNext = true
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("ask+allow → %d", resp.StatusCode)
		}
	})

	t.Run("fail closed with no snapshot", func(t *testing.T) {
		snap = nil
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("no snapshot → %d, want 503", resp.StatusCode)
		}
	})

	mu.Lock()
	defer mu.Unlock()
	if len(outcomes) == 0 {
		t.Fatal("no decisions were reported")
	}
}

func TestProxyRefusesWebSocketUpgrade(t *testing.T) {
	ca := testCA(t)
	snap := directSnapshot(t, net1("p", "example.com", policy.DecisionAllow))
	addr := startProxy(t, Config{CA: ca, Snapshot: func() *policy.Snapshot { return snap }})
	client := clientThroughProxy(addr, ca)

	req, _ := http.NewRequest("GET", "https://example.com/socket", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := client.Do(req)
	if err != nil {
		return // a torn connection is an acceptable refusal too
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("the proxy must not let a websocket upgrade through uninspected")
	}
}

func TestCALeafIsTrustedAndPerHost(t *testing.T) {
	ca := testCA(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}
	leaf1, err := ca.leafFor("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(leaf1.Certificate[0])
	if err := x.VerifyHostname("api.example.com"); err != nil {
		t.Fatalf("leaf hostname: %v", err)
	}
	if _, err := x.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("leaf does not chain to the CA: %v", err)
	}
	// Cached per host.
	leaf2, _ := ca.leafFor("api.example.com")
	if &leaf1.Certificate[0][0] != &leaf2.Certificate[0][0] {
		t.Fatal("leaf for the same host should be cached")
	}
}

func TestCAPersistsAcrossLoad(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca1.CertPEM()) != string(ca2.CertPEM()) {
		t.Fatal("reloading the CA minted a different one; the sandbox trust store would break")
	}
}

// The build's proxy under the direct setting is a pass-through: HTTPS is
// tunnelled to the origin untouched — the client sees the origin's own
// certificate, not one minted by john — and no snapshot is consulted. The
// session's proxy, same origin, still refuses what no policy covers.
func TestAnOpenProxyTunnelsWhatTheSessionsProxyRefuses(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from origin")
	}))
	defer origin.Close()
	ca := testCA(t)

	addr := startProxy(t, Config{CA: ca, Open: true})
	open := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: addr}),
		TLSClientConfig: &tls.Config{RootCAs: originRoots(origin)}, // the origin's, not john's
	}}
	resp, err := open.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from origin" {
		t.Fatalf("open proxy → %d %q", resp.StatusCode, body)
	}

	session := clientThroughProxy(startProxy(t, Config{CA: ca, UpstreamRoots: originRoots(origin),
		Snapshot: func() *policy.Snapshot { return directSnapshot(t) }}), ca)
	resp, err = session.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("session proxy with no policy → %d, want 403", resp.StatusCode)
	}
}

// A client such as apt sends several plain-HTTP requests down one connection
// and waits for every answer. Both proxies answer each of them, rather than
// the first and then closing.
func TestPlainHTTPRequestsShareAConnection(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "answer to "+r.URL.Path)
	}))
	defer origin.Close()
	host := strings.TrimPrefix(origin.URL, "http://")
	ca := testCA(t)
	allow := directSnapshot(t, net1("p", "127.0.0.1", policy.DecisionAllow))

	for name, cfg := range map[string]Config{
		"open":   {CA: ca, Open: true},
		"policy": {CA: ca, Snapshot: func() *policy.Snapshot { return allow }},
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("tcp", startProxy(t, cfg))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			// Both requests go out before either answer is read: pipelined.
			fmt.Fprintf(conn, "GET http://%s/one HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
			fmt.Fprintf(conn, "GET http://%s/two HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
			br := bufio.NewReader(conn)
			for _, want := range []string{"answer to /one", "answer to /two"} {
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("waiting for %q: %v", want, err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(body) != want {
					t.Fatalf("got %q, want %q", body, want)
				}
			}
		})
	}
}

// An upstream response relayed to the sandbox is HTTP/1.1 whatever the origin
// spoke, and one of unknown length is chunked — an HTTP/2 response relayed as
// it came has neither, and the client waits for an end that never comes.
func TestAResponseIsRelayedAsHTTP11WithAnEnd(t *testing.T) {
	resp := &http.Response{
		StatusCode: 200, Proto: "HTTP/2.0", ProtoMajor: 2, Header: http.Header{},
		ContentLength: -1, Body: io.NopCloser(strings.NewReader("hello")),
	}
	var out strings.Builder
	if err := writeResponse(&out, resp); err != nil {
		t.Fatal(err)
	}
	got, err := http.ReadResponse(bufio.NewReader(strings.NewReader(out.String())), nil)
	if err != nil {
		t.Fatalf("the relayed response does not parse: %v\n%s", err, out.String())
	}
	body, _ := io.ReadAll(got.Body)
	if got.ProtoMajor != 1 || string(body) != "hello" || len(got.TransferEncoding) == 0 {
		t.Fatalf("relayed as %s, encoding %v, body %q", got.Proto, got.TransferEncoding, body)
	}
}

// An allowed request that fails anyway is recorded as having failed, with the
// reason: the record is otherwise "allowed", and the only account of what went
// wrong is whatever the program inside prints about a reset connection.
func TestAnAllowedRequestThatFailsIsReportedWithWhy(t *testing.T) {
	// An origin that answers with a body it never finishes.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			io.WriteString(w, "fine")
			return
		}
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, "part of it")
		panic(http.ErrAbortHandler)
	}))
	defer origin.Close()
	host := strings.TrimPrefix(origin.URL, "http://")
	allow := directSnapshot(t, net1("p", "127.0.0.1", policy.DecisionAllow))

	var mu sync.Mutex
	var seen []Outcome
	addr := startProxy(t, Config{CA: testCA(t), Snapshot: func() *policy.Snapshot { return allow },
		Report: func(o Outcome) { mu.Lock(); seen = append(seen, o); mu.Unlock() }})

	for _, path := range []string{"/ok", "/broken"} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fmt.Fprintf(conn, "GET http://%s%s HTTP/1.1\r\nHost: %s\r\n\r\n", host, path, host)
		if resp, err := http.ReadResponse(bufio.NewReader(conn), nil); err == nil {
			io.ReadAll(resp.Body)
		}
		conn.Close()
	}

	// A response is reported once it has gone, which can be a moment after
	// the client has read what there was of it.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("reported %d outcomes, want 2: %+v", len(seen), seen)
	}
	if !seen[0].Allowed || seen[0].Error != "" {
		t.Errorf("the request that worked was reported as %+v", seen[0])
	}
	if !seen[1].Allowed || seen[1].Error == "" {
		t.Errorf("the request whose response broke off was reported as %+v, want an error", seen[1])
	}
}

// A request whose host had its access request rejected is refused as that,
// to the sandbox and in the record. It used to get the same body as a host
// nobody had asked about — "you can ask for a policy from john" — so the
// agent told the user nothing had been approved and was invited to ask again,
// and the trace said "no policy covers this request" while the screen said
// REQ-103 was rejected. It is a deny: an administrator has looked and said no.
func TestARejectedHostIsRefusedAsRejected(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "REACHED-ORIGIN")
	}))
	defer origin.Close()
	ca := testCA(t)

	var mu sync.Mutex
	var outcomes []Outcome
	addr := startProxy(t, Config{
		CA:       ca,
		Snapshot: func() *policy.Snapshot { return directSnapshot(t) },
		Report:   func(o Outcome) { mu.Lock(); outcomes = append(outcomes, o); mu.Unlock() },
		Rejected: func(host string) string {
			if host == "127.0.0.1" {
				return "REQ-103"
			}
			return ""
		},
	})
	resp, err := clientThroughProxy(addr, ca).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a rejected host → %d %q, want 403", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "REQ-103") || !strings.Contains(string(body), "rejected") ||
		!strings.Contains(string(body), "Don't ask for it again") {
		t.Fatalf("the sandbox was told %q, not that REQ-103 was rejected", body)
	}
	if strings.Contains(string(body), "ask for a policy") {
		t.Fatalf("the sandbox was invited to ask again: %q", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(outcomes) != 1 || outcomes[0].Verdict != policy.VerdictDeny ||
		outcomes[0].Policy != "REQ-103 was rejected" || outcomes[0].Rejected != "REQ-103" {
		t.Fatalf("outcomes = %+v, want one deny naming REQ-103", outcomes)
	}
}
