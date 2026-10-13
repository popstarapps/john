package sandbox

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The sandbox's route out, from the inside.
//
// The machine gives the guest one channel to the john proxy on the host, and
// that channel is not a network — under Virtualization.framework it is a
// socket device, under the loopback machine a Unix socket. Nothing that a
// project's build runs knows how to speak either. `apt-get`, `curl`, `go mod
// download` and `npm install` all know exactly one thing: an HTTP proxy at a
// host and a port, named in the environment.
//
// So the agent listens on loopback inside the sandbox and carries each
// connection over the channel. That is the whole of the sandbox's egress: the
// machine has no network interface, so a program that ignores the environment
// reaches nothing at all, and a program that honours it reaches the proxy and
// no further. The listener is on 127.0.0.1 and the machine has no route off
// itself, so this adds nowhere new to reach — it only makes the one place
// reachable in the vocabulary that build tools have.
type proxyForwarder struct {
	ln    net.Listener
	addr  string
	dial  func() (net.Conn, error)
	slots chan struct{}

	mu     sync.Mutex
	closed bool
}

// maxProxyStreams caps how many connections the forwarder carries at once;
// the rest wait for one to finish. Virtualization.framework drops every
// stream the machine has — the control link with them, so every tool call
// after it fails with EOF — once about 250 are open at the same time, and a
// build that opens a connection per download gets there; `npm install` did.
// Half that leaves the two links plenty of room, and is still more parallel
// downloads than anything gains from.
const maxProxyStreams = 128

// startProxyForwarder opens the loopback listener. addr is the guest link
// address of the host's proxy, in the spelling dialGuestLink understands.
func startProxyForwarder(addr string, dial func(string) (net.Conn, error)) (*proxyForwarder, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f := &proxyForwarder{
		ln:    ln,
		addr:  ln.Addr().String(),
		dial:  func() (net.Conn, error) { return dial(addr) },
		slots: make(chan struct{}, maxProxyStreams),
	}
	go f.serve()
	return f, nil
}

// Addr is what goes in http_proxy.
func (f *proxyForwarder) Addr() string { return f.addr }

func (f *proxyForwarder) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.carry(conn)
	}
}

// carry joins one connection from inside the sandbox to the host's proxy. A
// failure to reach the host closes the connection rather than answering it:
// the caller then sees a refused proxy, which is what has happened, instead of
// a silent success that fetched nothing.
func (f *proxyForwarder) carry(local net.Conn) {
	f.slots <- struct{}{}
	defer func() { <-f.slots }()
	remote, err := f.dial()
	if err != nil {
		local.Close()
		return
	}
	done := make(chan struct{})
	go func() {
		io.Copy(remote, local)
		// Pass the hang-up on. The proxy holds a connection open between
		// requests, as keep-alive does, until it reads the end of it; without
		// this, every finished request kept its slot, and the forwarder stopped
		// carrying anything after maxProxyStreams of them.
		closeWrite(remote)
		close(done)
	}()
	io.Copy(local, remote)
	local.Close()
	remote.Close()
	<-done
}

// closeWrite tells the far end that nothing more is coming, and lets what it
// still has to send arrive. A connection that cannot half-close is closed.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
		return
	}
	c.Close()
}

func (f *proxyForwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return f.ln.Close()
}

// The proxy terminates TLS, so everything inside has to trust its certificate
// authority or every HTTPS fetch fails on a name it cannot verify. The file
// goes where a Debian-style image already looks for a local authority, so
// nothing in the environment has to be told about it.
const guestCAPath = "/usr/local/share/ca-certificates/john.crt"

// guestCAFile is where the certificate lands, under whatever the agent's root
// is: "/" inside a real machine, and a directory on the developer's disk under
// the loopback machine, which is a process and has no /usr of its own to write.
func guestCAFile(root string) string {
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, filepath.FromSlash(guestCAPath))
}

// installGuestCA writes the proxy's certificate authority inside the sandbox.
func installGuestCA(root string, pem []byte) error {
	if len(pem) == 0 {
		return nil
	}
	path := guestCAFile(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, pem, 0o644)
}

// trustsJohn reports whether an environment from proxyEnviron points programs
// at john's certificate.
func trustsJohn(env []string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, "SSL_CERT_FILE=") {
			return true
		}
	}
	return false
}

// proxyEnviron is what a command inside the sandbox runs with so that it uses
// the proxy and trusts it. The names are the conventional ones, because a
// project's build should not have to know it is in a sandbox to obey the
// proxy. trustCA is false for a proxy that passes traffic through without
// terminating TLS: there is no certificate of john's to trust, and pointing
// programs at one would only make them fail to find it.
func proxyEnviron(proxyAddr, root string, trustCA bool) []string {
	if proxyAddr == "" {
		return nil
	}
	url := "http://" + proxyAddr
	guestCAPath := guestCAFile(root)
	env := []string{
		"HTTP_PROXY=" + url, "HTTPS_PROXY=" + url,
		"http_proxy=" + url, "https_proxy=" + url,
	}
	// Loopback is the sandbox talking to itself — a dev server it started, a
	// database it runs — and that traffic never leaves the machine, so there
	// is nothing for the proxy to decide and sending it out through one would
	// only break it.
	//
	// That reasoning is about a machine, and root is how the agent knows it
	// is inside one. Under the loopback machine it is a process on the
	// developer's own computer, where 127.0.0.1 is the developer's services
	// and not the sandbox's; exempting it there would hand a command the one
	// address it could reach without the proxy recording anything, which is
	// the opposite of what the exemption is for.
	if root == "" {
		env = append(env, "NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost,127.0.0.1")
	}
	if !trustCA {
		return env
	}
	return append(env,
		"SSL_CERT_FILE="+guestCAPath,
		"NODE_EXTRA_CA_CERTS="+guestCAPath,
		"REQUESTS_CA_BUNDLE="+guestCAPath,
		"GIT_SSL_CAINFO="+guestCAPath,
	)
}
