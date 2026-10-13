//go:build linux

package sandbox

import (
	"fmt"
	"net"
	"strconv"

	"golang.org/x/sys/unix"
)

// vsockHostCID is the well-known context id of the host, seen from inside a
// virtual machine. It is the same number under every hypervisor john uses;
// what differs is the socket family, and Hyper-V's hv_sock is handled where
// that machine is.
const vsockHostCID = 2

// dialVsockHost opens a stream to the client on the developer's machine. The
// sandbox has no network route to it — that is the point — so the socket
// device is the whole of the link between the two.
func dialVsockHost(port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("sandbox: vsock: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: vsockHostCID, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("sandbox: vsock connect to host port %d: %w", port, err)
	}
	return newSocketConn(fd, "vsock:"+strconv.FormatUint(uint64(port), 10))
}
