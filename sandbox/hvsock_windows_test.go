package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The guest link's streams, without a guest.
//
// Hyper-V sockets have a loopback: a service bound for HV_GUID_LOOPBACK is
// reachable from this computer itself, through the same AF_HYPERV socket
// family and the same hvConn wrapper the machine uses. That needs the Hyper-V
// socket service and nothing else — no virtual machine, no guest image and no
// administrator — so what is tested here is exactly the host half of every
// byte the sandbox's network access carries.

var hvLoopback = windows.GUID{
	Data1: 0xe0e16197, Data2: 0xdd56, Data3: 0x4a10,
	Data4: [8]byte{0x91, 0x95, 0x5e, 0xe7, 0xa1, 0x55, 0xa8, 0x38},
}

var procConnect = ws2_32.NewProc("connect")

// hvLoopbackPair listens on a loopback service and dials it, or skips where
// this computer has no Hyper-V sockets to give.
func hvLoopbackPair(t *testing.T, port uint32) (*hvListener, func() net.Conn) {
	t.Helper()
	ln, err := listenHvSock(hvLoopback, port)
	if err != nil {
		t.Skipf("no Hyper-V socket loopback here: %v", err)
	}
	t.Cleanup(func() { ln.close() })
	dial := func() net.Conn {
		t.Helper()
		sock, err := windows.WSASocket(afHyperV, windows.SOCK_STREAM, hvProtocolRaw,
			nil, 0, windows.WSA_FLAG_OVERLAPPED)
		if err != nil {
			t.Fatal(err)
		}
		sa := rawSockaddrHV{Family: afHyperV, VMID: hvLoopback, ServiceID: hvServiceID(port)}
		if r, _, e := procConnect.Call(uintptr(sock),
			uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); int32(r) != 0 {
			windows.Closesocket(sock)
			t.Skipf("cannot dial the Hyper-V socket loopback: %v", e)
		}
		c, err := newHvConn(sock, "loopback")
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return ln, dial
}

func hvAccept(t *testing.T, ln *hvListener) net.Conn {
	t.Helper()
	c, err := ln.accept(context.Background(), time.Now().Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// writeChunks writes data in pieces of uneven size, the way a relay's io.Copy
// does, and reports what it wrote.
func writeChunks(w io.Writer, data []byte, seed int64) error {
	r := rand.New(rand.NewSource(seed))
	for len(data) > 0 {
		n := 1 + r.Intn(64<<10)
		if n > len(data) {
			n = len(data)
		}
		if _, err := w.Write(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func randomBytes(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// Both directions at once, on many streams at once, is what a build pulling
// packages through the proxy does — and every byte must arrive as it was sent.
func TestHvConnCarriesEveryByteBothWays(t *testing.T) {
	ln, dial := hvLoopbackPair(t, 0x4a00)
	const streams, size = 8, 16 << 20

	var wg sync.WaitGroup
	errs := make(chan error, 4*streams)
	check := func(what string, got, want []byte) {
		if !bytes.Equal(got, want) {
			at := 0
			for at < len(got) && at < len(want) && got[at] == want[at] {
				at++
			}
			t.Errorf("%s: got %d bytes (sha %x), want %d (sha %x); first difference at %d",
				what, len(got), sha256.Sum256(got), len(want), sha256.Sum256(want), at)
		}
	}
	for i := 0; i < streams; i++ {
		up, down := randomBytes(size, int64(2*i)), randomBytes(size, int64(2*i+1))
		client := dial()
		server := hvAccept(t, ln)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer client.Close()
			defer server.Close()
			var got sync.WaitGroup
			var atServer, atClient []byte
			got.Add(2)
			go func() { defer got.Done(); atServer, _ = io.ReadAll(io.LimitReader(server, size)) }()
			go func() { defer got.Done(); atClient, _ = io.ReadAll(io.LimitReader(client, size)) }()
			go func() { errs <- writeChunks(client, up, int64(i)) }()
			go func() { errs <- writeChunks(server, down, int64(i+100)) }()
			got.Wait()
			check("guest to host", atServer, up)
			check("host to guest", atClient, down)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// The relay closes the stream to the guest the moment the proxy's side has
// ended. Everything written before that must still arrive: a response cut
// short is what a package manager reports as a connection reset.
func TestHvConnCloseDeliversWhatWasWritten(t *testing.T) {
	ln, dial := hvLoopbackPair(t, 0x4a01)
	const size = 8 << 20
	for i := 0; i < 8; i++ {
		data := randomBytes(size, int64(i))
		client := dial()
		server := hvAccept(t, ln)
		go func() {
			writeChunks(server, data, int64(i))
			server.Close()
		}()
		got, err := io.ReadAll(client)
		client.Close()
		if err != nil {
			t.Errorf("stream %d: read: %v after %d bytes", i, err, len(got))
		}
		if !bytes.Equal(got, data) {
			t.Errorf("stream %d: got %d of %d bytes", i, len(got), size)
		}
	}
}
