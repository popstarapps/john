package backend

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer the menu's goroutine and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestSetupOnLoopbackSaysHowToTunnel(t *testing.T) {
	srv := testServer(t)
	srv.SetupURL = "http://" + srv.Addr() + "/setup?token=x"
	_, port, _ := net.SplitHostPort(srv.Addr())

	var out bytes.Buffer
	srv.OfferRemoteSetup(strings.NewReader(""), &out, false, []HostAddress{{net.ParseIP("192.0.2.7"), "eth0"}})
	got := out.String()
	if !strings.Contains(got, "ssh -L "+port+":127.0.0.1:"+port) {
		t.Errorf("no tunnel command in:\n%s", got)
	}
	if !strings.Contains(got, "--listen <address>:"+port) {
		t.Errorf("off a terminal, --listen should be named instead of a menu:\n%s", got)
	}
	if strings.Contains(got, "1) 192.0.2.7") {
		t.Errorf("a menu was offered off a terminal:\n%s", got)
	}
}

func TestSetupListensOnTheAddressPicked(t *testing.T) {
	// The server is on 127.0.0.1, so the other address has to be one this
	// machine can bind without clashing: IPv6 loopback, where there is one.
	if ln, err := net.Listen("tcp", "[::1]:0"); err != nil {
		t.Skip("no IPv6 loopback here")
	} else {
		ln.Close()
	}
	srv := testServer(t)
	srv.SetupURL = "http://" + srv.Addr() + "/setup?token=x"
	_, port, _ := net.SplitHostPort(srv.Addr())

	in, typed := io.Pipe()
	defer typed.Close()
	var out syncBuffer
	srv.OfferRemoteSetup(in, &out, true, []HostAddress{{net.ParseIP("192.0.2.7"), "eth0"}, {net.ParseIP("::1"), "lo0"}})
	if !strings.Contains(out.String(), "2) ::1        lo0") {
		t.Fatalf("addresses not offered:\n%s", out.String())
	}

	typed.Write([]byte("nine\n2\n"))
	want := "http://[::1]:" + port + "/setup?token=x"
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("no URL on the picked address:\n%s", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "Type a number from 1 to 2.") {
		t.Errorf("a bad answer was not refused:\n%s", out.String())
	}

	resp, err := http.Get("http://[::1]:" + port + "/api/v1/org/public")
	if err != nil {
		t.Fatalf("the picked address does not answer: %v", err)
	}
	resp.Body.Close()
}

func TestAWildcardSetupURLIsOneABrowserCanOpen(t *testing.T) {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := loopbackFor(ln.Addr()); !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("loopbackFor(%s) = %s", ln.Addr(), got)
	}
}
