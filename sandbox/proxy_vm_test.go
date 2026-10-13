package sandbox

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/proxy"
)

// The network story, in the sandbox john actually ships.
//
// The claim is not "the sandbox uses a proxy" — a program can ignore an
// environment variable. It is that there is nothing else to use: the machine
// has no network interface, so the only way out is the channel the machine
// carries to the proxy, and everything on it arrives somewhere that decides.
// Both halves are asserted here, and the second one inside a booted kernel.
//
// They are separate subtests because they are answerable by different things.
// The route — an allowed host reaches, an unknown one does not, the proxy
// recorded both — is the guest's forwarder and the policy engine, and every
// machine here can answer it, including the loopback one. "And no other
// route" is a property of a machine, and only something that isolates can be
// asked it.
func TestTheSandboxReachesTheNetworkOnlyThroughTheProxy(t *testing.T) {
	// An origin on the host. The proxy reaches it on loopback; the sandbox
	// reaches the proxy over the machine's channel and never the origin.
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("REACHED-ORIGIN"))
	})}
	oln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(oln)
	t.Cleanup(func() { origin.Close() })
	originHost, originPort, _ := net.SplitHostPort(oln.Addr().String())

	snap := directSnapshot(t, policy.NetworkPolicy{ID: "allow-origin", Spec: policy.NetworkSpec{
		Kind: "network", Methods: []string{"GET"}, Scheme: "http", Host: originHost,
		Path: "/*", Decision: policy.DecisionAllow}})

	ca := mustCA(t)
	var mu sync.Mutex
	var seen []proxy.Outcome
	p := proxy.New(proxy.Config{
		CA:       ca,
		Snapshot: func() *policy.Snapshot { return snap },
		Report: func(o proxy.Outcome) {
			mu.Lock()
			seen = append(seen, o)
			mu.Unlock()
		},
	})
	// Short path: AF_UNIX names are capped near 108 bytes and a test's
	// temporary directory is nowhere near short enough.
	sock := shortSocket(t)
	if err := p.ListenUnix(sock); err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	t.Cleanup(func() { p.Close() })

	machines := realMachinesWithProxy(t, sock, ca.CertPEM())
	if len(machines) == 0 {
		t.Skip("no machine here can carry a route to the proxy")
	}
	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			vm, _ := bootRealMachine(t, m, hostTree(t, map[string]string{"a.txt": "a\n"}), policy.Protections{})
			if !vm.Routed() {
				t.Fatal("the sandbox came up with no route to the proxy")
			}
			needsAShell(t, vm)

			// Through the proxy, an allowed host reaches.
			allowed := run(t, vm, "wget -Y on -T 10 -q -O - http://"+originHost+":"+originPort+"/ 2>&1; echo")
			if !strings.Contains(string(allowed.Stdout), "REACHED-ORIGIN") {
				t.Fatalf("an allowed request did not reach the origin: %q", allowed.Stdout)
			}

			// And a host no policy covers does not.
			blocked := run(t, vm, "wget -Y on -T 10 -q -O - http://example.com/ 2>&1; echo DONE")
			if strings.Contains(string(blocked.Stdout), "REACHED-ORIGIN") {
				t.Fatal("a blocked host was reachable")
			}

			mu.Lock()
			defer mu.Unlock()
			var sawAllow, sawUnknown bool
			for _, o := range seen {
				switch o.Verdict {
				case policy.VerdictAllow:
					sawAllow = true
				case policy.VerdictUnknown:
					sawUnknown = true
				}
			}
			if !sawAllow || !sawUnknown {
				t.Fatalf("the proxy did not record both decisions: %+v", seen)
			}
			seen = nil

			// And there is no other way out. This is the half that makes the
			// rest of it a control rather than a convention, and it is a
			// claim about a machine: only something that isolates can be
			// asked whether anything gets past it.
			t.Run("and no other route", func(t *testing.T) {
				needsAMachine(t, vm)

				// One interface, and it is loopback.
				links := string(run(t, vm, "ip -o link show 2>/dev/null || cat /proc/net/dev").Stdout)
				for _, iface := range []string{"eth0", "ens", "enp", "wlan"} {
					if strings.Contains(links, iface) {
						t.Fatalf("the machine has a network interface: %q", links)
					}
				}

				// Off the proxy, nothing is reachable — not even the origin
				// the policy allows, because reaching it needs a route this
				// machine does not have.
				direct := run(t, vm, "wget -Y off -T 5 -q -O - http://"+originHost+":"+originPort+"/ 2>&1; echo DONE")
				if strings.Contains(string(direct.Stdout), "REACHED-ORIGIN") {
					t.Fatal("the sandbox reached the origin without the proxy")
				}
			})
		})
	}
}

// A blocked request, approved, works the next time — in the same sandbox,
// through the same proxy, with nothing restarted.
//
// This is the loop a developer lives in: the agent reaches for a host no
// policy covers, the proxy blocks it and says so, the developer files a
// request, an administrator approves it, and the agent's retry goes through.
// The approval here is the one the client proposes, stored and read back the
// way the backend does, and it reaches the proxy the way a policy.refresh
// does: as the next snapshot it reads.
func TestABlockedRequestWorksOnceItsPolicyIsApproved(t *testing.T) {
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("REACHED-ORIGIN"))
	})}
	oln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(oln)
	t.Cleanup(func() { origin.Close() })
	originURL := "http://" + oln.Addr().String() + "/"

	// No policies: every host is unknown.
	var snap atomic.Pointer[policy.Snapshot]
	snap.Store(directSnapshot(t))

	ca := mustCA(t)
	var mu sync.Mutex
	var seen []proxy.Outcome
	p := proxy.New(proxy.Config{
		CA:       ca,
		Snapshot: snap.Load,
		Report: func(o proxy.Outcome) {
			mu.Lock()
			seen = append(seen, o)
			mu.Unlock()
		},
	})
	sock := shortSocket(t)
	if err := p.ListenUnix(sock); err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	t.Cleanup(func() { p.Close() })

	machines := realMachinesWithProxy(t, sock, ca.CertPEM())
	if len(machines) == 0 {
		t.Skip("no machine here can carry a route to the proxy")
	}
	fetch := "wget -Y on -T 10 -q -O - " + originURL + " 2>&1; echo"
	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			snap.Store(directSnapshot(t))
			mu.Lock()
			seen = nil
			mu.Unlock()

			vm, _ := bootRealMachine(t, m, hostTree(t, map[string]string{"a.txt": "a\n"}), policy.Protections{})
			needsAShell(t, vm)

			// Blocked, and reported as the unknown request a developer can
			// file.
			if out := run(t, vm, fetch).Stdout; strings.Contains(string(out), "REACHED-ORIGIN") {
				t.Fatalf("a host no policy covers was reachable: %q", out)
			}
			mu.Lock()
			var blocked *proxy.Outcome
			for i, o := range seen {
				if o.Verdict == policy.VerdictUnknown && !o.Allowed {
					blocked = &seen[i]
				}
			}
			mu.Unlock()
			if blocked == nil {
				t.Fatalf("the proxy did not report the blocked request: %+v", seen)
			}

			// Filed and approved unchanged: the proposed spec goes into the
			// store as JSON and comes back out the way the backend builds a
			// snapshot from it.
			spec, err := proxy.ProposedPolicy(*blocked, true)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			var approved policy.NetworkSpec
			if err := json.Unmarshal(stored, &approved); err != nil {
				t.Fatal(err)
			}
			snap.Store(directSnapshot(t, policy.NetworkPolicy{ID: "approved", Spec: approved}))

			// The same request, from the same sandbox, now goes through.
			if out := run(t, vm, fetch).Stdout; !strings.Contains(string(out), "REACHED-ORIGIN") {
				t.Fatalf("the approved request still did not reach the origin: %q", out)
			}
			mu.Lock()
			defer mu.Unlock()
			last := seen[len(seen)-1]
			if last.Verdict != policy.VerdictAllow || !last.Allowed {
				t.Fatalf("the proxy did not record the retry as allowed: %+v", last)
			}
		})
	}
}

// shortSocket is a Unix socket path short enough for AF_UNIX.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "p.sock")
}

// A build step that fetches goes through the proxy like everything else.
//
// This is the case the policy.Build setting exists for: RUN is where a project
// installs its compilers, it is the one moment a sandbox has a reason to want
// the network wide open, and the traffic still arrives somewhere that records
// it. The base image here is served locally and pulled by the host; what
// crosses the machine's boundary is the RUN step's own fetch.
func TestAnEnvironmentBuildFetchesThroughTheProxy(t *testing.T) {
	layer, ok := busyboxLayer(t)
	if !ok {
		t.Skip("no guest image built; see sandbox/guest/mkimage.sh")
	}
	registry := serveImage(t, layer)

	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("PACKAGE-PAYLOAD"))
	})}
	oln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(oln)
	t.Cleanup(func() { origin.Close() })
	originHost, originPort, _ := net.SplitHostPort(oln.Addr().String())

	snap := directSnapshot(t, policy.NetworkPolicy{ID: "allow-origin", Spec: policy.NetworkSpec{
		Kind: "network", Methods: []string{"GET"}, Scheme: "http", Host: originHost,
		Path: "/*", Decision: policy.DecisionAllow}})

	ca := mustCA(t)
	var mu sync.Mutex
	var seen []proxy.Outcome
	p := proxy.New(proxy.Config{
		CA:       ca,
		Snapshot: func() *policy.Snapshot { return snap },
		Report: func(o proxy.Outcome) {
			mu.Lock()
			seen = append(seen, o)
			mu.Unlock()
		},
	})
	sock := shortSocket(t)
	if err := p.ListenUnix(sock); err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	t.Cleanup(func() { p.Close() })

	machines := realMachinesWithProxy(t, sock, ca.CertPEM())
	if len(machines) == 0 {
		t.Skip("no machine here can carry a route to the proxy")
	}
	files := map[string]string{
		"main.go": "package main\n",
		EnvFile: "FROM " + registry + "/library/base:v1\n" +
			"RUN mkdir -p /opt && wget -O /opt/fetched http://" + originHost + ":" + originPort + "/\n",
	}
	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			vm, _ := bootRealMachine(t, m, hostTree(t, files), policy.Protections{})
			needsAnEnvironment(t, vm)
			needsAShell(t, vm)

			if got := strings.TrimSpace(string(run(t, vm, "cat /opt/fetched").Stdout)); got != "PACKAGE-PAYLOAD" {
				t.Fatalf("the build step did not fetch through the proxy: %q", got)
			}
			mu.Lock()
			defer mu.Unlock()
			var recorded bool
			for _, o := range seen {
				if o.Verdict == policy.VerdictAllow && strings.Contains(o.URL, originPort) {
					recorded = true
				}
			}
			if !recorded {
				t.Fatalf("the proxy did not record the build's fetch: %+v", seen)
			}
			seen = nil
		})
	}
}

// The certificate authority the proxy terminates TLS with, and a snapshot
// holding one network policy. They live here because the tests above are the
// only things that need them.

func mustCA(t *testing.T) *proxy.CA {
	t.Helper()
	ca, err := proxy.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func directSnapshot(t *testing.T, policies ...policy.NetworkPolicy) *policy.Snapshot {
	t.Helper()
	return proxy.TestSnapshot(policies)
}

// A build's worth of connections, and the sandbox still answers.
//
// Every request a program makes through the proxy is a stream of its own
// across the machine's socket device. The host held on to each one for the
// life of the machine, so they piled up — and once about 250 were open,
// Virtualization.framework dropped every stream the machine had, the control
// link among them. An `npm install` got there in fourteen seconds, and every
// tool call after it failed with EOF.
func TestManyRequestsThroughTheProxyLeaveTheSandboxWorking(t *testing.T) {
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("REACHED-ORIGIN"))
	})}
	oln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(oln)
	t.Cleanup(func() { origin.Close() })
	host, port, _ := net.SplitHostPort(oln.Addr().String())

	snap := directSnapshot(t, policy.NetworkPolicy{ID: "allow-origin", Spec: policy.NetworkSpec{
		Kind: "network", Methods: []string{"GET"}, Scheme: "http", Host: host,
		Path: "/*", Decision: policy.DecisionAllow}})
	ca := mustCA(t)
	p := proxy.New(proxy.Config{CA: ca, Snapshot: func() *policy.Snapshot { return snap },
		Report: func(proxy.Outcome) {}})
	sock := shortSocket(t)
	if err := p.ListenUnix(sock); err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	t.Cleanup(func() { p.Close() })

	machines := realMachinesWithProxy(t, sock, ca.CertPEM())
	if len(machines) == 0 {
		t.Skip("no machine here can carry a route to the proxy")
	}
	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			vm, _ := bootRealMachine(t, m, hostTree(t, map[string]string{"a.txt": "a\n"}), policy.Protections{})
			needsAShell(t, vm)
			res := run(t, vm, "n=0; for i in $(seq 1 400); do wget -Y on -T 10 -q -O - http://"+
				host+":"+port+"/ >/dev/null 2>&1 && n=$((n+1)); done; echo ok=$n")
			if got := strings.TrimSpace(string(res.Stdout)); got != "ok=400" {
				t.Fatalf("400 requests in a row: %q", got)
			}
			if res := run(t, vm, "echo still-here"); !strings.Contains(string(res.Stdout), "still-here") {
				t.Fatalf("the sandbox stopped answering: %q", res.Stdout)
			}
		})
	}
}

// What a package manager does to the route out: several large downloads at
// once, every byte of which has to arrive as the origin sent it. Small
// responses never showed it, but a build's dependencies are megabytes each,
// and a stream cut short or corrupted on the way in surfaces in the sandbox as
// a connection reset, or as a TLS record that fails its check.
func TestLargeDownloadsThroughTheProxyArriveWhole(t *testing.T) {
	const size, parallel = 12 << 20, 6
	body := func(seed int) []byte {
		b := make([]byte, size)
		rand.New(rand.NewSource(int64(seed))).Read(b)
		return b
	}
	origin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seed, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		w.Write(body(seed))
	})}
	oln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go origin.Serve(oln)
	t.Cleanup(func() { origin.Close() })
	host, port, _ := net.SplitHostPort(oln.Addr().String())

	snap := directSnapshot(t, policy.NetworkPolicy{ID: "allow-origin", Spec: policy.NetworkSpec{
		Kind: "network", Methods: []string{"GET"}, Scheme: "http", Host: host,
		Path: "/*", Decision: policy.DecisionAllow}})
	ca := mustCA(t)
	p := proxy.New(proxy.Config{CA: ca, Snapshot: func() *policy.Snapshot { return snap },
		Report: func(proxy.Outcome) {}})
	sock := shortSocket(t)
	if err := p.ListenUnix(sock); err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	t.Cleanup(func() { p.Close() })

	var want strings.Builder
	for i := 0; i < parallel; i++ {
		fmt.Fprintf(&want, "%x  /tmp/dl%d\n", sha256.Sum256(body(i)), i)
	}

	machines := realMachinesWithProxy(t, sock, ca.CertPEM())
	if len(machines) == 0 {
		t.Skip("no machine here can carry a route to the proxy")
	}
	for _, m := range machines {
		t.Run(m.name, func(t *testing.T) {
			vm, _ := bootRealMachine(t, m, hostTree(t, map[string]string{"a.txt": "a\n"}), policy.Protections{})
			needsAShell(t, vm)
			for round := 0; round < 3; round++ {
				res := run(t, vm, fmt.Sprintf("for i in $(seq 0 %d); do wget -Y on -T 30 -q -O /tmp/dl$i "+
					"http://%s:%s/$i & done; wait; for i in $(seq 0 %d); do sha256sum /tmp/dl$i; done; rm -f /tmp/dl*",
					parallel-1, host, port, parallel-1))
				if got := string(res.Stdout); got != want.String() {
					t.Fatalf("round %d: what arrived differs from what the origin sent:\n%s\nwant:\n%s\nstderr: %s",
						round, got, want.String(), res.Stderr)
				}
			}
		})
	}
}
