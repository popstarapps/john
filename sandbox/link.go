package sandbox

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The guest reaches the host. Which transport that is depends on the machine
// underneath, and the address the host passed says which:
//
//	vsock:<port>   the host over a virtual machine's socket device
//	dev:<path>     a character device, which is what a virtio serial port is
//	<path>         a Unix socket, which is what the loopback machine uses
//
// Spelling it as an address rather than a build tag keeps the agent one
// program that works under whichever machine started it, and keeps the choice
// of transport a property of the machine rather than of the agent.
func dialGuestLink(addr string) (net.Conn, error) {
	switch {
	case strings.HasPrefix(addr, "vsock:"):
		n, err := strconv.ParseUint(strings.TrimPrefix(addr, "vsock:"), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("sandbox: bad guest link address %q: %w", addr, err)
		}
		return dialVsockHost(uint32(n))
	case strings.HasPrefix(addr, "dev:"):
		return openDevice(strings.TrimPrefix(addr, "dev:"))
	case strings.HasPrefix(addr, "/"), filepath.IsAbs(addr):
		// Both spellings, because the two ends of this are not always the
		// same operating system. Inside a machine the agent is Linux and the
		// address is a Linux path; under the loopback machine the agent is a
		// process on the developer's own computer, and on Windows the socket
		// it is told to open is C:\... — which filepath.IsAbs recognises
		// there and the leading slash does not.
		return net.Dial("unix", addr)
	default:
		return nil, fmt.Errorf("%w: %q", errNotAStream, addr)
	}
}

// The guest's configuration reaches it on the kernel command line, because at
// the moment a machine boots there is nowhere else to put it: no filesystem
// the host has written, no network, and no link yet — the configuration is
// what says how to open the link.
//
// It is base64url so that it survives a command line intact. The kernel splits
// on whitespace and the bootloader has opinions about quoting; base64url has
// neither, and it is unpadded so there is no '=' to be read as a separator.

const guestConfigFlag = "john.cfg="

// errNoGuestConfig means the agent was started without being told how to reach
// the host, which is the one thing it cannot work out for itself.
var errNoGuestConfig = errors.New("sandbox: the guest was started with no configuration")

func encodeGuestConfig(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

// GuestConfigFromCmdline reads the configuration a machine booted the agent
// with. It is how `john __guest` starts inside a virtual machine; the loopback
// machine, which has a command line of its own to use, passes it as an
// argument instead.
func GuestConfigFromCmdline() (GuestConfig, error) {
	raw, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return GuestConfig{}, fmt.Errorf("%w: %v", errNoGuestConfig, err)
	}
	for _, field := range strings.Fields(string(raw)) {
		encoded, ok := strings.CutPrefix(field, guestConfigFlag)
		if !ok {
			continue
		}
		return decodeGuestConfig(encoded)
	}
	return GuestConfig{}, errNoGuestConfig
}

func decodeGuestConfig(encoded string) (GuestConfig, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return GuestConfig{}, fmt.Errorf("sandbox: unreadable guest configuration: %w", err)
	}
	var cfg GuestConfig
	if err := json.Unmarshal(decoded, &cfg); err != nil {
		return GuestConfig{}, err
	}
	return cfg, nil
}
