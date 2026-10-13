//go:build darwin && cgo

package sandbox

/*
#cgo CFLAGS: -fobjc-arc
#cgo LDFLAGS: -framework Virtualization -framework Foundation -framework Security

#include <stdlib.h>
#include "machine_vz_darwin.h"
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

// macOS: Virtualization.framework.
//
// The framework provides the whole machine — an ARM64 or x86-64 Linux guest,
// virtio-blk, virtio-vsock and a boot loader — so this machine configures and
// starts one rather than implementing anything. It is an Objective-C API and
// therefore cgo, which is why the client is no longer built with
// CGO_ENABLED=0 on macOS; the Objective-C is all in machine_vz_darwin.m, and this file
// is the same shape as machine_qemu.go next door.
//
// The guest link is vsock, in the direction the sandbox needs it: the guest
// dials the host, so the host installs two listeners rather than connecting to
// two ports.
type vz struct {
	// Kernel, Initramfs and Console override what the boot config carries.
	// They exist for the same reason the qemu harness has them: a test needs
	// to point the machine at an image it built itself rather than one
	// extracted from the binary, and needs to know where the boot log went.
	// Nothing outside a test sets them.
	Kernel    string
	Initramfs string
	Console   string
}

func platformHypervisor() hypervisor { return &vz{} }

func (v *vz) Name() string { return "virtualization.framework" }

// The ports the guest dials back on. They are the machine's own choice and
// travel to the guest in its configuration, so nothing outside this file has
// to agree with them; they are above 1024 only so that they look like what
// they are next to a vsock trace.
const (
	vzControlPort = 2201
	vzLowerPort   = 2202
	// The third stream, and the only one the guest opens more than once: each
	// connection a program inside makes to the proxy is its own.
	vzProxyPort = 2203
)

// bootLinkWait is how long a booting guest has to open its two links before
// the machine is given up on.
const bootLinkWait = 2 * time.Minute

// Available says which of the reasons it is, because they call for different
// things from the person reading it: an old OS is an upgrade, a Mac without
// virtualization is a different machine, and a missing entitlement is a build
// that was signed wrong — and that last one is by far the most likely, since
// macOS will not start a virtual machine for a binary that does not carry it.
func (v *vz) Available() error {
	msg := make([]byte, 512)
	code := C.vz_supported((*C.char)(unsafe.Pointer(&msg[0])), C.size_t(len(msg)))
	if code == 0 {
		return nil
	}
	reason := goString(msg)
	if code == 3 {
		reason += " — a development build can be signed ad hoc: " +
			"codesign --entitlements sandbox/machine_vz.plist -f -s - <binary>"
	}
	return fmt.Errorf("sandbox: %s", reason)
}

func (v *vz) Boot(ctx context.Context, cfg bootConfig) (machine, error) {
	if err := v.Available(); err != nil {
		return nil, err
	}
	kernel, initramfs := v.Kernel, v.Initramfs
	if kernel == "" {
		kernel = cfg.Kernel
	}
	if initramfs == "" {
		initramfs = cfg.Initramfs
	}
	if kernel == "" || initramfs == "" {
		return nil, ErrNoGuestImage
	}

	guest := GuestConfig{
		Mount:          cfg.Mount,
		Upper:          cfg.Upper,
		Control:        "vsock:" + strconv.Itoa(vzControlPort),
		Lower:          "vsock:" + strconv.Itoa(vzLowerPort),
		MaxOutputBytes: cfg.MaxOutputBytes,
		MaxProcesses:   cfg.Resources.Processes,
	}
	raw, err := jsonMarshal(guest)
	if err != nil {
		return nil, err
	}

	disk, err := stateDisk(cfg)
	if err != nil {
		return nil, err
	}

	// The console log always exists rather than only when someone asked for
	// it: boot is where this goes wrong, it is written once per start, and a
	// developer whose sandbox did not come up should have something to read.
	console := v.Console
	if console == "" {
		console = filepath.Join(cfg.Dir, "console.log")
	}
	if f, err := createConsole(console); err == nil {
		f.Close()
	} else {
		console = ""
	}

	var conf C.vz_config
	free := func(p *C.char) { C.free(unsafe.Pointer(p)) }

	conf.kernel = C.CString(kernel)
	defer free(conf.kernel)
	conf.initramfs = C.CString(initramfs)
	defer free(conf.initramfs)
	conf.cmdline = C.CString("console=hvc0 panic=1 john.cfg=" + encodeGuestConfig(raw))
	defer free(conf.cmdline)
	conf.disk = C.CString(disk)
	defer free(conf.disk)
	if cfg.EnvDisk != "" {
		conf.envdisk = C.CString(cfg.EnvDisk)
		defer free(conf.envdisk)
		if cfg.EnvWritable {
			conf.envwritable = 1
		}
	}
	if console != "" {
		conf.console = C.CString(console)
		defer free(conf.console)
	}
	conf.cpus = C.uint(max(cfg.Resources.CPUs, 1))
	conf.memory = C.ulonglong(max(cfg.Resources.MemoryGB, 1)) << 30
	conf.ports[0] = C.uint(vzControlPort)
	conf.ports[1] = C.uint(vzLowerPort)
	conf.ports[2] = C.uint(vzProxyPort)
	conf.nports = 3

	msg := make([]byte, 512)
	handle := C.vz_boot(&conf, (*C.char)(unsafe.Pointer(&msg[0])), C.size_t(len(msg)))
	if handle == nil {
		return nil, fmt.Errorf("sandbox: virtualization.framework: %s", goString(msg))
	}
	m := &vzMachine{handle: handle, console: console, proxySocket: cfg.ProxySocket}

	// The lower layer first, in the order the agent opens them, and both
	// before returning — VM.Build serves the lower layer the moment it has
	// it. Waiting for the agent to be ready is not done here: every machine
	// needs that wait, so VM.Build owns it.
	if m.lower, err = m.accept(ctx, vzLowerPort, bootLinkWait); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: vz lower channel: %w", err)
	}
	if m.control, err = m.accept(ctx, vzControlPort, bootLinkWait); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: vz control channel: %w", err)
	}
	m.serveProxy()
	return m, nil
}

// vzMachine is one running virtual machine. Everything Objective-C about it is
// behind the handle.
type vzMachine struct {
	handle      unsafe.Pointer
	console     string
	proxySocket string
	control     net.Conn
	lower       net.Conn
	once        sync.Once
	// relays is the proxy accept loop and every relay it started, waited for
	// before the handle is freed — an accept still running, or a connection
	// closing, when the machine is released would be reading memory that has
	// gone.
	relays sync.WaitGroup

	// mu guards the two below. carrying is the guest's open proxy
	// connections, closed by Stop so the relays on them end.
	mu       sync.Mutex
	carrying map[net.Conn]struct{}
	released bool
}

// vzConn is a stream from the guest. Closing it lets the framework go of its
// own half too; closing only the duplicate Go holds would leave the stream
// open, the guest's end waiting on it for ever.
type vzConn struct {
	net.Conn
	m    *vzMachine
	fd   int
	once sync.Once
}

func (c *vzConn) Close() error {
	c.once.Do(func() {
		// Before the descriptor closes, because once it has its number can
		// be handed to the next connection.
		c.m.mu.Lock()
		if !c.m.released {
			C.vz_forget(c.m.handle, C.int(c.fd))
		}
		c.m.mu.Unlock()
	})
	return c.Conn.Close()
}

func (m *vzMachine) Control() net.Conn { return m.control }
func (m *vzMachine) Lower() net.Conn   { return m.lower }
func (m *vzMachine) Root() string      { return "" }

func (m *vzMachine) ProxyAddr() string {
	if m.proxySocket == "" {
		return ""
	}
	return "vsock:" + strconv.Itoa(vzProxyPort)
}

// serveProxy relays the guest's connections to the john proxy on the host.
//
// This is the sandbox's only route out, and it is a route to one Unix socket
// rather than to a network: the guest has no interface, so nothing inside can
// reach anything this loop does not carry, and everything it carries arrives
// at the proxy to be decided. Unlike the other two streams it accepts many
// times — a program inside opens a connection per request.
func (m *vzMachine) serveProxy() {
	if m.proxySocket == "" {
		return
	}
	m.relays.Add(1)
	go func() {
		defer m.relays.Done()
		for {
			// No deadline: a sandbox can go minutes without a request — a
			// long build, a developer reading — and the next one still has
			// to be carried. Giving up here left every later connection
			// queued with nobody to take it, and the program inside waiting
			// on a proxy that never answered.
			conn, err := m.accept(context.Background(), vzProxyPort, 0)
			if err != nil {
				// The machine has stopped, which is the only way out of this
				// loop; Stop waits for it before freeing the handle.
				return
			}
			m.mu.Lock()
			if m.carrying == nil {
				m.carrying = map[net.Conn]struct{}{}
			}
			m.carrying[conn] = struct{}{}
			m.mu.Unlock()
			m.relays.Add(1)
			go func() {
				defer m.relays.Done()
				m.relay(conn)
				m.mu.Lock()
				delete(m.carrying, conn)
				m.mu.Unlock()
			}()
		}
	}()
}

// relay joins one connection from inside the machine to the proxy's socket.
func (m *vzMachine) relay(guest net.Conn) {
	host, err := net.Dial("unix", m.proxySocket)
	if err != nil {
		guest.Close()
		return
	}
	done := make(chan struct{})
	go func() {
		io.Copy(host, guest)
		// Half-close, so a request whose body has ended is answered rather
		// than left waiting for a peer that has said everything it has to.
		if c, ok := host.(*net.UnixConn); ok {
			c.CloseWrite()
		}
		close(done)
	}()
	io.Copy(guest, host)
	guest.Close()
	<-done
	host.Close()
}

func (m *vzMachine) Stop(ctx context.Context) error {
	m.once.Do(func() {
		if m.control != nil {
			m.control.Close()
		}
		if m.lower != nil {
			m.lower.Close()
		}
		// The links closing is the guest's cue to go; it gets the chance to
		// put its disks away before the power is pulled. Stopping is also
		// what ends the accept loop: an accept on a stopped machine fails
		// rather than waiting, so the goroutine is gone before the handle it
		// reads is freed.
		if C.vz_wait_stopped(m.handle, C.int(shutdownGrace.Milliseconds())) == 0 {
			C.vz_stop(m.handle)
		}
		// Closed outside the lock: closing one takes it.
		m.mu.Lock()
		carrying := make([]net.Conn, 0, len(m.carrying))
		for conn := range m.carrying {
			carrying = append(carrying, conn)
		}
		m.mu.Unlock()
		for _, conn := range carrying {
			conn.Close()
		}
		m.relays.Wait()
		m.mu.Lock()
		m.released = true
		m.mu.Unlock()
		C.vz_release(m.handle)
	})
	return nil
}

// accept waits for the guest to open one of the two streams. The framework
// hands the connection to a listener whenever the guest gets round to it, so
// this waits in short steps rather than one long one: a cancelled context is
// then noticed while the machine is still booting, instead of at the end of a
// timeout nobody is still waiting for. A wait of zero waits until the machine
// stops.
func (m *vzMachine) accept(ctx context.Context, port int, wait time.Duration) (net.Conn, error) {
	var deadline time.Time
	if wait > 0 {
		deadline = time.Now().Add(wait)
	}
	if d, ok := ctx.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
		deadline = d
	}
	for {
		msg := make([]byte, 512)
		fd := C.vz_accept(m.handle, C.uint(port), 200,
			(*C.char)(unsafe.Pointer(&msg[0])), C.size_t(len(msg)))
		switch {
		case fd >= 0:
			conn, err := newSocketConn(int(fd), "vsock:"+strconv.Itoa(port))
			if err != nil {
				return nil, err
			}
			return &vzConn{Conn: conn, m: m, fd: int(fd)}, nil
		case fd == -1:
			return nil, errors.New(goString(msg))
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, fmt.Errorf("the guest never opened the link on vsock port %d "+
				"(see %s)", port, m.console)
		}
	}
}

// goString reads the NUL-terminated message the C side wrote into a buffer.
func goString(buf []byte) string {
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf)
}
