package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The guest link, on Windows.
//
// Hyper-V's socket device is hv_sock, and hv_sock is not vsock. Inside the
// machine it is: Linux registers it as a vsock transport, so the agent's
// dialGuestLink("vsock:<port>") is the same code under every hypervisor and
// nothing in link.go knows this file exists. On the host it is a socket family
// of its own — AF_HYPERV, with HV_PROTOCOL_RAW — whose addresses are a pair of
// GUIDs rather than a context id and a port.
//
// The two are bridged by a convention Linux and Windows both hold to: vsock
// port N is the service GUID N-facb-11e6-bd58-64006a7986d3. A guest connecting
// to port 2201 arrives at whoever bound that GUID for this virtual machine.
// That is the whole of the mapping, and it is why the guest side needed no
// Windows-specific code at all.
//
// Direction matters here the way it did on macOS: john's design has the guest
// dialing the host, so the host binds and listens and the guest connects. That
// is the opposite of most hv_sock examples, which drive a service inside a
// Windows guest from the host.

const (
	afHyperV      = 34
	hvProtocolRaw = 1
)

var (
	ws2_32     = windows.NewLazySystemDLL("ws2_32.dll")
	procBind   = ws2_32.NewProc("bind")
	procAccept = ws2_32.NewProc("accept")
	procSelect = ws2_32.NewProc("select")
)

// rawSockaddrHV is SOCKADDR_HV: the family, and then the machine and the
// service, as GUIDs.
type rawSockaddrHV struct {
	Family    uint16
	Reserved  uint16
	VMID      windows.GUID
	ServiceID windows.GUID
}

// fdSet is Winsock's, cut to the one socket this file ever waits on. select
// reads Count entries and writes back the ready ones, so a set of one is
// exactly enough and there is nothing to be gained from carrying sixty-four.
type fdSet struct {
	Count uint32
	Array [1]windows.Handle
}

type timeval struct {
	Sec  int32
	Usec int32
}

// hvServiceID is the service GUID a vsock port maps to.
//
// The suffix is not ours and is not arbitrary: it is the template Linux's
// hv_sock transport uses to turn a vsock port into a VMBus service, so a guest
// that thinks it is dialing vsock port 2201 arrives here. Changing it would
// mean patching the guest kernel, which is the wrong side of the boundary to
// be inventing anything on.
func hvServiceID(port uint32) windows.GUID {
	return windows.GUID{
		Data1: port,
		Data2: 0xfacb,
		Data3: 0x11e6,
		Data4: [8]byte{0xbd, 0x58, 0x64, 0x00, 0x6a, 0x79, 0x86, 0xd3},
	}
}

// hvListener is one bound service GUID, waiting for the guest to dial it.
type hvListener struct {
	// sock is read by whoever is waiting in accept and written by close, and
	// those are two different goroutines by design rather than by accident:
	// closing the listener is how the proxy's accept loop is stopped, so the
	// write lands while that loop is between polls. As a plain field that was
	// a data race, and an intermittent one: the detector caught it once in
	// tens of runs of the package, which is exactly the kind that reaches a
	// release.
	//
	// Atomic rather than a mutex because accept must not hold anything across
	// its wait — Stop would then block for up to a poll — and because Swap
	// gives close its close-once for free.
	sock atomic.Uintptr
	port uint32
}

// listenHvSock binds a service for one virtual machine and starts listening.
//
// It is bound to that machine's id rather than to the wildcard, so a listener
// here can only ever be reached by the sandbox it was made for — two john
// sessions on one developer's computer are two machines with two ids, and
// neither can arrive at the other's control stream.
func listenHvSock(vmID windows.GUID, port uint32) (*hvListener, error) {
	sock, err := windows.WSASocket(afHyperV, windows.SOCK_STREAM, hvProtocolRaw,
		nil, 0, windows.WSA_FLAG_OVERLAPPED)
	if err != nil {
		return nil, fmt.Errorf("hv_sock socket: %w", err)
	}
	sa := rawSockaddrHV{Family: afHyperV, VMID: vmID, ServiceID: hvServiceID(port)}
	// x/sys/windows.Bind takes a Sockaddr, an interface no package outside it
	// can implement, and there is no AF_HYPERV member of it — so this is the
	// one place the socket calls go straight to ws2_32.
	if r, _, e := procBind.Call(uintptr(sock),
		uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); int32(r) != 0 {
		windows.Closesocket(sock)
		return nil, fmt.Errorf("hv_sock bind to service %s: %w", hvServiceID(port).String(), e)
	}
	if err := windows.Listen(sock, 8); err != nil {
		windows.Closesocket(sock)
		return nil, fmt.Errorf("hv_sock listen: %w", err)
	}
	l := &hvListener{port: port}
	l.sock.Store(uintptr(sock))
	return l, nil
}

// accept waits for the guest to open this stream.
//
// It waits in short steps rather than one long one, for the same reason the
// macOS machine does: a cancelled context is then noticed while the machine is
// still booting, rather than at the end of a timeout nobody is waiting for any
// more. Closing the listener is what ends it — select on a closed socket fails
// rather than waiting, which is how the proxy's accept loop stops.
func (l *hvListener) accept(ctx context.Context, deadline time.Time) (net.Conn, error) {
	for {
		// One snapshot per poll, so that select and the accept that follows it
		// name the same socket even if close lands between them.
		sock := windows.Handle(l.sock.Load())
		if sock == windows.InvalidHandle {
			return nil, fmt.Errorf("hv_sock listener on port %d is closed", l.port)
		}
		set := fdSet{Count: 1, Array: [1]windows.Handle{sock}}
		tv := timeval{Usec: 200 * 1000}
		r, _, err := procSelect.Call(0, uintptr(unsafe.Pointer(&set)), 0, 0, uintptr(unsafe.Pointer(&tv)))
		switch n := int32(r); {
		case n < 0:
			// The listener has been closed, or Winsock has given up on it.
			// Either way there is nothing more to wait for.
			return nil, fmt.Errorf("hv_sock select on port %d: %w", l.port, err)
		case n > 0:
			var from rawSockaddrHV
			size := int32(unsafe.Sizeof(from))
			s, _, err := procAccept.Call(uintptr(sock),
				uintptr(unsafe.Pointer(&from)), uintptr(unsafe.Pointer(&size)))
			if windows.Handle(s) == windows.InvalidHandle {
				return nil, fmt.Errorf("hv_sock accept on port %d: %w", l.port, err)
			}
			return newHvConn(windows.Handle(s), "vsock:"+strconv.FormatUint(uint64(l.port), 10))
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, fmt.Errorf("the guest never opened the link on vsock port %d", l.port)
		}
	}
}

func (l *hvListener) close() error {
	if l == nil {
		return nil
	}
	sock := windows.Handle(l.sock.Swap(uintptr(windows.InvalidHandle)))
	if sock == windows.InvalidHandle {
		return nil
	}
	return windows.Closesocket(sock)
}

// newHvConn wraps a connected AF_HYPERV socket as a net.Conn.
//
// Go's net package has no address type for this family and cannot be talked
// into making one, so the descriptor is handed to the runtime's poller
// directly — which is the same answer link_socket.go reaches on Unix for
// AF_VSOCK, and for the same reason: nothing here dials, listens or resolves
// an address. The socket arrives already connected and is only ever read,
// written and closed.
//
// On Windows that is os.NewFile, which registers the handle with the runtime's
// completion port and issues overlapped reads and writes against it. What that
// buys is what a hand-rolled blocking wrapper would not: Close interrupts a
// read that is already waiting, deadlines work, and a machine with many
// connections open to the proxy costs goroutines rather than operating system
// threads.
//
// It takes TWO handles to the one socket, though, because an os.File is not
// one thing that can be read and written at the same time. It keeps a file
// offset and advances it after every read and after every write, and it does
// not synchronise the two — internal/poll says so in as many words and tells
// the caller to serialise them. This link cannot: one goroutine reads frames
// for the life of the machine while another writes them, and serialising would
// mean a blocked read stopping every write.
//
// A socket has no offset, so none of that bookkeeping means anything here —
// but it is still a data race, and the race detector is right to say so. Every
// other os.File in Go is read or written and not both, because the other thing
// with two directions is a pipe and a pipe comes as two files. So does this:
// the handle is duplicated, the reader gets one and the writer the other, and
// each keeps an offset nobody else touches. It costs one handle per stream.
func newHvConn(sock windows.Handle, name string) (net.Conn, error) {
	proc := windows.CurrentProcess()
	var dup windows.Handle
	if err := windows.DuplicateHandle(proc, sock, proc, &dup, 0, false,
		windows.DUPLICATE_SAME_ACCESS); err != nil {
		windows.Closesocket(sock)
		return nil, fmt.Errorf("hv_sock: %s could not be duplicated: %w", name, err)
	}
	return &hvConn{
		sock: sock,
		r:    os.NewFile(uintptr(sock), name),
		w:    os.NewFile(uintptr(dup), name),
		name: name,
	}, nil
}

// hvConn is a connected hv_sock stream. End of file means the peer has gone,
// which on this link means the machine has stopped — so it is the thin wrapper
// a socket deserves rather than the waiting-for-the-peer one a serial port
// needs.
type hvConn struct {
	sock windows.Handle // the socket both files are handles to; owned by r
	r    *os.File
	w    *os.File
	name string
}

func (c *hvConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *hvConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// CloseWrite tells the peer that nothing more is coming, and lets everything
// already written arrive first.
//
// Close does not. A written byte is only buffered for the guest, and closing
// the last handle to the socket while the guest is still draining that buffer
// throws away what is left: a twelve-megabyte response the relay had written
// in full arrived in the sandbox as nothing at all, which npm reports as a
// connection reset. Loopback drains as fast as it is written, so this shows
// only against a machine. A stream that ends this way is closed when the guest
// has closed its side too.
func (c *hvConn) CloseWrite() error {
	return windows.Shutdown(c.sock, windows.SHUT_WR)
}

// Close closes both handles, because the socket lives until the last one goes.
// The read half first, so that a peer waiting on this link learns it has
// closed at the same moment either way.
func (c *hvConn) Close() error {
	err := c.r.Close()
	if werr := c.w.Close(); err == nil {
		err = werr
	}
	return err
}

func (c *hvConn) LocalAddr() net.Addr  { return hvAddr(c.name) }
func (c *hvConn) RemoteAddr() net.Addr { return hvAddr(c.name) }

func (c *hvConn) SetDeadline(t time.Time) error {
	err := c.r.SetReadDeadline(t)
	if werr := c.w.SetWriteDeadline(t); err == nil {
		err = werr
	}
	return err
}

func (c *hvConn) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *hvConn) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }

type hvAddr string

func (a hvAddr) Network() string { return "hvsock" }
func (a hvAddr) String() string  { return string(a) }
