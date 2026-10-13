package sandbox

import (
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// fileConn is a net.Conn over a character device. A virtual machine's serial
// port is a stream of bytes with no addresses and no accept, so the guest end
// of the link is a file the agent opens rather than a socket it dials — and
// everything above it wants a net.Conn.
//
// End-of-file on such a port does not mean what it means on a socket. A serial
// port with nobody attached on the other side reads EOF immediately, and reads
// again once somebody attaches — so EOF here is "not yet", not "never". Taking
// it at face value would have the agent decide the host had gone before the
// host had arrived, which is exactly what happens on a machine that boots
// faster than the thing waiting for it.
//
// So a read waits for the peer instead of ending. The machine is the host's to
// stop; an agent inside one does not get to decide the session is over.
//
// Deadlines are supported when the device is pollable, which a virtio-serial
// port is; nothing in the link sets them, and a device that cannot poll says
// so rather than pretending.
type fileConn struct {
	f      *os.File
	closed atomic.Bool
}

func (c *fileConn) Read(p []byte) (int, error) {
	for {
		n, err := c.f.Read(p)
		if n > 0 || !errors.Is(err, io.EOF) {
			return n, err
		}
		if c.closed.Load() {
			return 0, io.EOF
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *fileConn) Write(p []byte) (int, error) { return c.f.Write(p) }
func (c *fileConn) Close() error {
	c.closed.Store(true)
	return c.f.Close()
}

func (c *fileConn) LocalAddr() net.Addr  { return deviceAddr(c.f.Name()) }
func (c *fileConn) RemoteAddr() net.Addr { return deviceAddr(c.f.Name()) }

func (c *fileConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *fileConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *fileConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

type deviceAddr string

func (a deviceAddr) Network() string { return "device" }
func (a deviceAddr) String() string  { return string(a) }

// openDevice opens a character device as one end of the guest link.
func openDevice(path string) (net.Conn, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &fileConn{f: f}, nil
}

var errNotAStream = errors.New("sandbox: guest link address names nothing that can carry a stream")
