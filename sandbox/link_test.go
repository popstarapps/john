package sandbox

import (
	"errors"
	"testing"
)

// The transport is the machine's choice, not the agent's: the same agent runs
// under a hypervisor that gives it a socket device and under one that gives it
// a serial port, and the address it is handed is what says which.
func TestTheAgentTakesItsTransportFromTheAddress(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"vsock:1024", "vsock"},
		{"dev:/dev/vport0p1", "device"},
		{"/tmp/john/control.sock", "unix"},
	}
	for _, c := range cases {
		// Dialling is expected to fail here — there is no machine on the
		// other end. What is being checked is that it tried the right thing,
		// which the error names.
		_, err := dialGuestLink(c.addr)
		if err == nil {
			t.Errorf("%s: dialled successfully with nothing to dial", c.addr)
			continue
		}
		if errors.Is(err, errNotAStream) {
			t.Errorf("%s: not recognised as a transport at all: %v", c.addr, err)
		}
	}

	if _, err := dialGuestLink("http://example.com"); !errors.Is(err, errNotAStream) {
		t.Errorf("an address that names no transport was accepted: %v", err)
	}
	if _, err := dialGuestLink(""); !errors.Is(err, errNotAStream) {
		t.Errorf("an empty address was accepted: %v", err)
	}
}

// The configuration reaches the agent on the kernel command line, which the
// kernel splits on whitespace and which no bootloader is careful with. It has
// to survive that.
func TestTheGuestConfigurationSurvivesTheKernelCommandLine(t *testing.T) {
	cfg := GuestConfig{
		Mount:          "/Users/bob/a project/with spaces",
		Upper:          "/john/upper",
		Control:        "vsock:1024",
		Lower:          "vsock:1025",
		MaxOutputBytes: 1 << 20,
	}
	raw, err := jsonMarshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	encoded := encodeGuestConfig(raw)

	for _, r := range encoded {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			t.Fatalf("encoded configuration contains %q, which a command line may not carry intact", r)
		}
	}

	var back GuestConfig
	decoded, err := decodeGuestConfig(encoded)
	if err != nil {
		t.Fatal(err)
	}
	back = decoded
	if back != cfg {
		t.Fatalf("round trip gave %+v, want %+v", back, cfg)
	}
}
