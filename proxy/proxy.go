package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/popstarapps/john/policy"
)

// Outcome is what the proxy did with a request, for reporting.
type Outcome struct {
	Method  string
	URL     string
	Verdict policy.Verdict
	Policy  string
	Allowed bool
	// Error is why an allowed request still failed: the origin could not be
	// reached, or the response broke off on its way to the sandbox. Without
	// it a request that never arrived is recorded as one that did, and the
	// only account of the failure is whatever the program inside prints.
	Error string
	// Rejected is the access request whose rejection refused this request,
	// when that is what did. The verdict is then Deny, the same as a deny
	// policy's, and this is what tells the two apart.
	Rejected string
}

// Approver is asked to decide an ask-developer request. It returns true to
// allow that one request. Outside a session there is no developer to ask, so
// the approver auto-rejects.
type Approver func(req policy.Request, bodyPreview string) bool

// Reporter receives every decision the proxy makes, for the trace and the
// console's network-request record.
type Reporter func(Outcome)

// SnapshotSource yields the current policy snapshot. It returns nil when no
// usable snapshot is held — past expiry, or before the first pull — and the
// proxy then fails closed.
type SnapshotSource func() *policy.Snapshot

// Proxy terminates TLS, buffers each request whole, and decides it locally
// against the snapshot. It never asks the backend about an individual request.
type Proxy struct {
	ca       *CA
	snapshot SnapshotSource
	approve  Approver
	report   Reporter
	open     bool
	rejected func(host string) string

	ln     net.Listener
	socket string // non-empty when listening on a Unix socket
	http   *http.Transport

	mu     sync.Mutex
	closed bool
}

// Config assembles a Proxy.
type Config struct {
	CA       *CA
	Snapshot SnapshotSource
	Approve  Approver
	Report   Reporter
	// UpstreamRoots overrides the roots the proxy trusts when forwarding to
	// origins. Nil uses the system roots; tests point it at a test origin.
	UpstreamRoots *x509.CertPool
	// Open makes the proxy a plain pass-through: no policy, no TLS
	// termination, nothing buffered — a tunnel for CONNECT and a forwarder
	// for plain HTTP. It is the environment build's route out under the
	// default "direct" build setting, and nothing else's: the build installs
	// a project's toolchain before any session exists, and a separate proxy
	// is what keeps that allowance off the route the agent's own commands
	// take. The sandbox has no network interface, so even an unrestricted
	// build reaches the network through a proxy; this one just stays out of
	// the way.
	Open bool
	// Rejected names the access request turned down for a host, or "" when
	// none was. A request no policy covers is still refused, but as what it
	// is: one the developer asked about and was told no. Without it the
	// sandbox heard "you can ask for a policy from john" after REQ-103 was
	// rejected, the agent told the user no policy had been approved, and the
	// trace said "no policy covers this request" while the screen said the
	// request was rejected.
	Rejected func(host string) string
}

// New creates a proxy. It does not listen until Serve.
func New(cfg Config) *Proxy {
	approve := cfg.Approve
	if approve == nil {
		approve = func(policy.Request, string) bool { return false }
	}
	report := cfg.Report
	if report == nil {
		report = func(Outcome) {}
	}
	tr := &http.Transport{
		Proxy:             nil,
		ForceAttemptHTTP2: false, // one request at a time; no multiplexing to inspect around
		// And not HTTP/2 by Go's default either, which a transport with no
		// TLS configuration of its own otherwise gets: the client inside the
		// sandbox speaks HTTP/1.1 to the proxy, and an HTTP/2 response
		// relayed to it as it came has no length and no chunking, so the
		// client waits for an end that never comes.
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:        32,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if cfg.UpstreamRoots != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: cfg.UpstreamRoots}
	}
	return &Proxy{
		ca: cfg.CA, snapshot: cfg.Snapshot, approve: approve, report: report, http: tr, open: cfg.Open,
		rejected: cfg.Rejected,
	}
}

// Listen binds the proxy to addr (":0" for a free port) and returns the
// address. Serve then accepts on it.
func (p *Proxy) Listen(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	p.ln = ln
	return ln.Addr().String(), nil
}

// ListenUnix binds the proxy to a Unix domain socket at path. This is how the
// sandbox reaches the proxy: the machine relays the guest's connections to
// this socket, so the sandbox-to-host hop is a file on the developer's disk
// rather than a network route. Nothing is exposed on the network, and the
// socket is owner-only.
func (p *Proxy) ListenUnix(path string) error {
	os.Remove(path) // a stale socket from a crashed run would block the bind
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o600)
	p.ln = ln
	p.socket = path
	return nil
}

// Serve accepts connections until Close.
func (p *Proxy) Serve() error {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		go p.handle(conn)
	}
}

// Close stops the proxy.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	var err error
	if p.ln != nil {
		err = p.ln.Close()
	}
	if p.socket != "" {
		os.Remove(p.socket)
	}
	return err
}

// handle serves one client connection: a CONNECT for HTTPS (which the proxy
// then terminates), or a plain absolute-URI HTTP request.
func (p *Proxy) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if p.open {
		p.passThrough(conn, br, req)
		return
	}
	if req.Method == http.MethodConnect {
		p.handleConnect(conn, br, req)
		return
	}
	// Plain HTTP keeps its connection for the next request, the way the
	// tunnel does: a client such as apt sends several on one connection and
	// waits for every answer.
	for {
		if !p.handlePlainHTTP(conn, req, "http") {
			return
		}
		if req, err = http.ReadRequest(br); err != nil {
			return
		}
	}
}

// passThrough is the whole of an open proxy: a CONNECT is joined to its
// origin byte for byte, and plain HTTP is forwarded as it comes, one request
// after another on the same connection.
func (p *Proxy) passThrough(conn net.Conn, br *bufio.Reader, req *http.Request) {
	if req.Method == http.MethodConnect {
		up, err := net.DialTimeout("tcp", req.URL.Host, 15*time.Second)
		if err != nil {
			writeRefusal(conn, req, http.StatusBadGateway, "could not reach "+req.URL.Host+": "+err.Error())
			return
		}
		defer up.Close()
		p.report(Outcome{Method: req.Method, URL: req.URL.Host, Verdict: policy.VerdictAllow,
			Policy: "environment build (direct)", Allowed: true})
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		done := make(chan struct{}, 2)
		go func() { io.Copy(up, br); closeWrite(up); done <- struct{}{} }()
		go func() { io.Copy(conn, up); closeWrite(conn); done <- struct{}{} }()
		<-done
		<-done
		return
	}
	for {
		if req.URL.Host == "" {
			req.URL.Host = req.Host
		}
		if req.URL.Scheme == "" {
			req.URL.Scheme = "http"
		}
		p.report(Outcome{Method: req.Method, URL: req.URL.String(), Verdict: policy.VerdictAllow,
			Policy: "environment build (direct)", Allowed: true})
		out := req.Clone(context.Background())
		out.RequestURI = ""
		resp, err := p.http.RoundTrip(out)
		if err != nil {
			writeRefusal(conn, req, http.StatusBadGateway, "upstream error: "+err.Error())
			return
		}
		err = writeResponse(conn, resp)
		resp.Body.Close()
		if err != nil || req.Close || resp.Close {
			return
		}
		if req, err = http.ReadRequest(br); err != nil {
			return
		}
	}
}

// closeWrite ends one direction of a tunnel so the other side sees the end of
// what was sent, without cutting off what is still coming back.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// handleConnect terminates the tunnel: it answers 200, completes TLS with a
// leaf minted for the host, then reads one inner request at a time and
// evaluates each — including requests caused by keep-alive reuse.
func (p *Proxy) handleConnect(conn net.Conn, br *bufio.Reader, req *http.Request) {
	host := req.URL.Hostname()
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	leaf, err := p.ca.leafFor(host)
	if err != nil {
		return
	}
	tlsConn := tls.Server(&prefixConn{Conn: conn, r: br}, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		// One protocol only: h2 would multiplex requests the proxy could not
		// buffer and inspect one at a time.
		NextProtos: []string{"http/1.1"},
	})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()

	tbr := bufio.NewReader(tlsConn)
	for {
		inner, err := http.ReadRequest(tbr)
		if err != nil {
			return
		}
		inner.URL.Scheme = "https"
		inner.URL.Host = withoutDefaultPort(req.URL.Host, "https")
		if !p.serveInner(tlsConn, inner, "https") {
			return
		}
	}
}

func (p *Proxy) handlePlainHTTP(conn net.Conn, req *http.Request, scheme string) bool {
	if req.URL.Scheme == "" {
		req.URL.Scheme = scheme
	}
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}
	return p.serveInner(conn, req, req.URL.Scheme)
}

// serveInner buffers, decides, and either forwards or refuses one request. It
// returns false when the connection should not be reused.
func (p *Proxy) serveInner(w io.Writer, req *http.Request, scheme string) bool {
	// Request forms that could bypass inspection are refused unless
	// explicitly supported: a websocket upgrade, or a body we could not
	// buffer, is not something the proxy can be sure it inspected.
	if isUpgrade(req) {
		writeRefusal(w, req, http.StatusBadGateway, "protocol upgrades are not supported through the john proxy")
		return false
	}

	body, tooLarge := readBounded(req.Body, maxBufferBytes)
	req.Body.Close()

	snap := p.currentSnapshot()
	if snap == nil {
		// Fail closed by expiry: no usable snapshot means no decision, and no
		// decision means block.
		p.report(Outcome{Method: req.Method, URL: req.URL.String(), Verdict: policy.VerdictDeny,
			Policy: "no policy snapshot (fail closed)"})
		writeRefusal(w, req, http.StatusServiceUnavailable,
			"john has no current policy — the backend is unreachable, so requests that need a decision are blocked")
		return false
	}

	preq := policy.Request{
		Method:   req.Method,
		URL:      req.URL,
		Headers:  req.Header,
		BodySize: len(body),
	}
	if tooLarge {
		preq.BodySize = maxBufferBytes + 1 // larger than any cap → no allow can match
	}
	decision := snap.Evaluate(preq)

	outcome := Outcome{Method: req.Method, URL: req.URL.String(),
		Verdict: decision.Verdict, Policy: decision.Reason}

	switch decision.Verdict {
	case policy.VerdictAllow:
		// Reported once the response has gone, so the record says whether
		// it did.
		outcome.Allowed = true
		reuse, err := p.forward(w, req, body)
		outcome.Error = errorText(err)
		p.report(outcome)
		return reuse

	case policy.VerdictAsk:
		allowed := p.approve(preq, preview(body))
		outcome.Verdict = policy.VerdictAsk
		outcome.Allowed = allowed
		if allowed {
			reuse, err := p.forward(w, req, body)
			outcome.Error = errorText(err)
			p.report(outcome)
			return reuse
		}
		p.report(outcome)
		writeRefusal(w, req, http.StatusForbidden, "the developer rejected this request")
		return false

	case policy.VerdictDeny:
		p.report(outcome)
		writeRefusal(w, req, http.StatusForbidden, "denied by policy: "+decision.Reason)
		return false

	default: // unknown — refused the moment it is made
		if ref := p.rejectedFor(req.URL.Hostname()); ref != "" {
			// The same refusal, saying why it stands: the program reading
			// it is usually an agent, and "you can ask" is an invitation to
			// file the request that was just turned down.
			//
			// And a deny, not an unknown: an administrator has looked at this
			// host and said no. Traced as unknown, it read the same as a host
			// nobody had looked at yet, and the client offered to ask again.
			outcome.Verdict = policy.VerdictDeny
			outcome.Policy = ref + " was rejected"
			outcome.Rejected = ref
			p.report(outcome)
			writeRefusal(w, req, http.StatusForbidden,
				"this request is blocked: access request "+ref+" for "+req.URL.Hostname()+
					" was rejected. Don't ask for it again.")
			return false
		}
		p.report(outcome)
		writeRefusal(w, req, http.StatusForbidden,
			"no policy covers this request; it is blocked. You can ask for a policy from john.")
		return false
	}
}

// rejectedFor is the access request turned down for host, if there is a hook
// to ask.
func (p *Proxy) rejectedFor(host string) string {
	if p.rejected == nil {
		return ""
	}
	return p.rejected(host)
}

// forward sends the buffered request on and streams the response back. It
// reports whether the connection can be reused, and why the request failed if
// it did.
func (p *Proxy) forward(w io.Writer, req *http.Request, body []byte) (bool, error) {
	out := req.Clone(context.Background())
	out.RequestURI = ""
	out.Body = io.NopCloser(newBytesReader(body))
	out.ContentLength = int64(len(body))

	resp, err := p.http.RoundTrip(out)
	if err != nil {
		writeRefusal(w, req, http.StatusBadGateway, "upstream error: "+err.Error())
		return false, fmt.Errorf("upstream: %w", err)
	}
	defer resp.Body.Close()
	if err := writeResponse(w, resp); err != nil {
		return false, fmt.Errorf("relaying the response: %w", err)
	}
	// Reuse the connection only when both sides will.
	return !req.Close && !resp.Close, nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// writeResponse relays an upstream response to a client that spoke HTTP/1.1
// to the proxy. It is written as HTTP/1.1 whatever the upstream spoke, so a
// body of unknown length goes out chunked and the client can tell where it
// ends.
func writeResponse(w io.Writer, resp *http.Response) error {
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	// Left to itself, Write marks a body of unknown length "Connection:
	// close" and expects the connection to end with it — while the proxy
	// keeps the connection for the client's next request.
	if resp.ContentLength < 0 && len(resp.TransferEncoding) == 0 {
		resp.TransferEncoding = []string{"chunked"}
	}
	return resp.Write(w)
}

func (p *Proxy) currentSnapshot() *policy.Snapshot {
	if p.snapshot == nil {
		return nil
	}
	snap := p.snapshot()
	if snap == nil || snap.Expired(time.Now()) {
		return nil
	}
	return snap
}

const maxBufferBytes = 10 << 20 // 10 MiB default request body cap

func isUpgrade(req *http.Request) bool {
	return req.Header.Get("Upgrade") != "" ||
		httpTokenContains(req.Header.Get("Connection"), "upgrade")
}

func writeRefusal(w io.Writer, req *http.Request, status int, msg string) {
	resp := &http.Response{
		StatusCode: status,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1, ProtoMinor: 1,
		Header:  http.Header{"Content-Type": {"text/plain; charset=utf-8"}, "Connection": {"close"}},
		Body:    io.NopCloser(newBytesReader([]byte("john proxy: " + msg + "\n"))),
		Request: req,
	}
	resp.ContentLength = int64(len("john proxy: " + msg + "\n"))
	resp.Write(w)
}

func preview(body []byte) string {
	const max = 2048
	if len(body) > max {
		return string(body[:max]) + "…"
	}
	return string(body)
}

// withoutDefaultPort drops a port that is the scheme's own. CONNECT always
// names one, so every HTTPS URL came out as https://host:443/… — on screen,
// in the trace, and in the access requests built from it — beside plain
// http://host/… for HTTP. Policies match the port either way.
func withoutDefaultPort(host, scheme string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		if strings.Contains(h, ":") {
			return "[" + h + "]" // an IPv6 address keeps its brackets
		}
		return h
	}
	return host
}
