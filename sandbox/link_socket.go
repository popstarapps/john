//go:build unix

package sandbox

import (
	"net"
	"os"
	"syscall"
	"time"
)

// A stream socket the standard library will not wrap.
//
// net.FileConn is what this would normally be, and it cannot be used on either
// end of the vsock link. It asks the kernel what family the socket is and
// refuses anything it has no address type for: AF_VSOCK on the Linux side, and
// whatever Virtualization.framework hands back on the macOS one. Neither is a
// family Go's net package knows, and neither needs it to — nothing here dials,
// listens or resolves an address; the descriptor arrives already connected and
// is only ever read and written.
//
// So the descriptor is put in non-blocking mode and handed to the runtime's
// poller directly. That is what net.FileConn would have done, and it is what
// makes Close interrupt a blocked read and makes deadlines work.
func newSocketConn(fd int, name string) (net.Conn, error) {
	if err := syscall.SetNonblock(fd, true); err != nil {
		syscall.Close(fd)
		return nil, os.NewSyscallError("setnonblock", err)
	}
	return &socketConn{f: os.NewFile(uintptr(fd), name), name: name}, nil
}

// socketConn is a connected stream socket as a net.Conn. It behaves like the
// socket it is — end of file means the peer has gone, which on this link means
// the machine has stopped — so it is a thin wrapper rather than the
// waiting-for-the-peer one a serial port needs.
type socketConn struct {
	f    *os.File
	name string
}

func (c *socketConn) Read(p []byte) (int, error)  { return c.f.Read(p) }
func (c *socketConn) Write(p []byte) (int, error) { return c.f.Write(p) }
func (c *socketConn) Close() error                { return c.f.Close() }

// CloseWrite shuts down the sending side only, so the peer reads the end of
// what was sent and can still answer.
func (c *socketConn) CloseWrite() error {
	rc, err := c.f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.Shutdown(int(fd), syscall.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}

func (c *socketConn) LocalAddr() net.Addr  { return socketAddr(c.name) }
func (c *socketConn) RemoteAddr() net.Addr { return socketAddr(c.name) }

func (c *socketConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *socketConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *socketConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

type socketAddr string

func (a socketAddr) Network() string { return "vsock" }
func (a socketAddr) String() string  { return string(a) }
