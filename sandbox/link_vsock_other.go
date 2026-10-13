//go:build !linux

package sandbox

import (
	"errors"
	"net"
)

// Only the guest dials over vsock, and the guest is Linux on every platform.
func dialVsockHost(uint32) (net.Conn, error) {
	return nil, errors.New("sandbox: the guest agent only runs inside the Linux sandbox")
}
