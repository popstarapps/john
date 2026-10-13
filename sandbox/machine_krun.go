//go:build linux && (amd64 || arm64) && !john_guest

package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Linux: KVM.
//
// KVM is not the same kind of interface as the two hypervisors on the other
// platforms. Virtualization.framework and Hyper-V each hand you a whole
// virtual machine with a device model already attached; /dev/kvm hands you
// virtual CPUs and memory and expects the device model — block, vsock, the
// boot protocol, the interrupt controller — to be yours. Writing one is a
// project in itself and not the project this is.
//
// So the Linux machine uses libkrun, which is a virtual machine monitor in
// library form: it provides the microVM, virtio-blk and vsock, and leaves the
// lifecycle to the caller. That keeps the shape identical to the other two —
// configure a machine, boot it, take the guest's two streams — and keeps the
// whole device model out of this repository. How the library is reached, and
// why it is reached that way, is libkrun_linux.go.
//
// Two things about it shape this file, and neither is true of the other two
// machines.
//
// The machine runs in a process of its own. libkrun's one entry point is
// krun_start_enter, and it does not return: the library takes the process
// over, runs the virtual machine on it, and calls exit() with the guest's
// status when the machine stops. A john that called it would be a john that
// had turned into a virtual machine. So john starts a second copy of itself —
// `john __machine`, the same binary, the way `john __guest` is the same binary
// inside the sandbox — and that copy is the machine. Everything above it here
// talks to the guest through sockets and watches the child, which is the same
// relationship the qemu harness next door has with its subprocess.
//
// And the guest link runs the other way round. Under Virtualization.framework
// and Hyper-V the host installs listeners and the guest dials them; libkrun
// maps a guest vsock port to a Unix socket path on the host and dials that
// path when the guest opens the port. So this machine listens on Unix sockets
// and libkrun brings the connections to them — the guest agent is unchanged
// and still just dials vsock, because which end listens is a property of the
// machine and not of the agent.
type krun struct {
	// Kernel, Initramfs and Console override what the boot config carries.
	// They exist for the same reason the other machines have them: a test
	// needs to point the machine at an image it built itself rather than one
	// extracted from the binary, and needs to know where the boot log went.
	// Nothing outside a test sets them.
	Kernel    string
	Initramfs string
	Console   string
}

func platformHypervisor() hypervisor { return &krun{} }

func (k *krun) Name() string { return "kvm" }

// The ports the guest dials back on. They are the machine's own choice and
// travel to the guest in its configuration, so nothing outside this file has
// to agree with them — they are the same numbers the other two machines use
// only because a trace with the same ports on every platform is easier to
// read.
const (
	krunControlPort = 2201
	krunLowerPort   = 2202
	// The third stream, and the only one the guest opens more than once: each
	// connection a program inside makes to the proxy is its own.
	krunProxyPort = 2203
)

// Available says which of the reasons it is, because they ask different things
// of the person reading it: a machine with no /dev/kvm cannot virtualize at
// all and is a firmware setting or a different computer, a /dev/kvm that will
// not open is one group membership away, and a missing libkrun is a package to
// install.
//
// The order is the order of the questions. Whether this machine can run a
// virtual machine at all is asked before whether the monitor for one is
// installed, because a developer who fixes the second and then meets the first
// has been sent round twice.
func (k *krun) Available() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return errors.New("sandbox: /dev/kvm is not present — hardware virtualization is " +
				"disabled in this machine's firmware, or this is already a virtual machine " +
				"without nested virtualization")
		case errors.Is(err, os.ErrPermission):
			return fmt.Errorf("sandbox: cannot open /dev/kvm: %w — this user is usually "+
				"missing from the kvm group:  sudo usermod -aG kvm $USER  "+
				"(and then sign in again, because a process's groups are fixed when its "+
				"session is made)", err)
		}
		return fmt.Errorf("sandbox: /dev/kvm: %w", err)
	}
	f.Close()

	if _, err := loadLibkrun(); err != nil {
		return err
	}
	return nil
}

func (k *krun) Boot(ctx context.Context, cfg bootConfig) (machine, error) {
	if err := k.Available(); err != nil {
		return nil, err
	}
	kernel, initramfs := k.Kernel, k.Initramfs
	if kernel == "" {
		kernel = cfg.Kernel
	}
	if initramfs == "" {
		initramfs = cfg.Initramfs
	}
	if kernel == "" || initramfs == "" {
		return nil, ErrNoGuestImage
	}
	// The child opens these, and it is started from whatever directory john
	// was started in; a relative path would be resolved twice.
	kernel, err := filepath.Abs(kernel)
	if err != nil {
		return nil, err
	}
	if initramfs, err = filepath.Abs(initramfs); err != nil {
		return nil, err
	}
	format, err := kernelFormat(kernel)
	if err != nil {
		return nil, err
	}

	guest := GuestConfig{
		Mount:          cfg.Mount,
		Upper:          cfg.Upper,
		Control:        "vsock:" + strconv.Itoa(krunControlPort),
		Lower:          "vsock:" + strconv.Itoa(krunLowerPort),
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
	// And the environment's, which nothing else is about to restore.
	//
	// libkrun shrinks a raw disk image out from under its own guest. Its block
	// device answers a discard that reaches the end of the device by
	// truncating the file to where the discard began, which for a raw image is
	// not a discard at all — it is the disk getting smaller. Making a
	// filesystem is what triggers it: mke2fs zeroes the last 64K of a device
	// before it writes anything, so the first boot of every sandbox ends with
	// its disk 64K shorter than the filesystem that was just made on it, and
	// the next boot cannot mount it — "bad geometry: block count N exceeds
	// size of device".
	//
	// Growing it back is the whole repair, and it is a repair rather than a
	// workaround: the bytes that were lost are the ones mke2fs asked to read
	// as zeroes, and the hole a grown sparse file leaves reads as zeroes. The
	// sandbox's own disk is already restored by stateDisk above, because
	// makeDisk grows what it finds; the environment's is made once in
	// VM.Build, before the machine that builds it runs, so by the time a
	// session attaches it the shrinking has already happened.
	if cfg.EnvDisk != "" {
		if err := makeDisk(cfg.EnvDisk, envDiskBytes); err != nil {
			return nil, err
		}
	}

	// The sockets the guest's streams arrive on. They live in a short
	// directory of their own because AF_UNIX paths are capped near 108 bytes
	// and a sandbox state directory is nowhere near short enough — the same
	// reason the qemu harness does this and the same reason the proxy puts its
	// socket in the temp dir.
	sockDir, err := os.MkdirTemp("", "jk")
	if err != nil {
		return nil, err
	}
	m := &krunMachine{sockDir: sockDir, exited: make(chan struct{})}

	// The console log always exists rather than only when somebody asked for
	// it: boot is where this goes wrong, it is written once per start, and a
	// developer whose sandbox did not come up should have something to read.
	// It carries two things — the guest's kernel log, on the machine's serial
	// port, and whatever libkrun itself has to say — because when a boot fails
	// it is not yet known which of them is talking.
	console := k.Console
	if console == "" {
		console = filepath.Join(cfg.Dir, "console.log")
	}
	log, err := createConsole(console)
	if err != nil {
		// A machine with no console still runs. Losing the log is worth
		// saying and is not worth refusing to boot over.
		fmt.Fprintf(os.Stderr, "john: the sandbox's console could not be opened: %v\n", err)
		console = ""
	}
	m.console, m.log = console, log

	boot := krunBoot{
		Kernel:    kernel,
		Format:    format,
		Initramfs: initramfs,
		Cmdline: "console=" + krunConsoleDevice() + " panic=1 john.cfg=" +
			encodeGuestConfig(raw),
		StateDisk:   disk,
		EnvDisk:     cfg.EnvDisk,
		EnvReadOnly: !cfg.EnvWritable,
		CPUs:        max(cfg.Resources.CPUs, 1),
		MemoryMiB:   max(cfg.Resources.MemoryGB, 1) * 1024,
	}

	// The two streams the agent opens once each. They are listening before
	// the machine starts, not after: the agent dials each of them once and
	// does not retry, and a guest that boots in a third of a second is quite
	// capable of arriving before a listener that was opened afterwards.
	for _, l := range []struct {
		port int
		name string
		into **net.UnixListener
	}{
		{krunLowerPort, "l", &m.lowerLn},
		{krunControlPort, "c", &m.controlLn},
	} {
		path := filepath.Join(sockDir, l.name)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			m.Stop(context.Background())
			return nil, fmt.Errorf("sandbox: kvm: %w", err)
		}
		*l.into = ln
		boot.Ports = append(boot.Ports, krunPort{Port: uint32(l.port), Path: path})
	}
	// The third is the route out, and it is relayed here rather than pointed
	// straight at the proxy's own socket — which libkrun would have allowed,
	// and which loses responses. See relay: the machine has to be the one that
	// closes this connection last, and it can only be that if it holds both
	// ends.
	if cfg.ProxySocket != "" {
		m.proxySocket = cfg.ProxySocket
		path := filepath.Join(sockDir, "p")
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			m.Stop(context.Background())
			return nil, fmt.Errorf("sandbox: kvm: %w", err)
		}
		m.proxyLn = ln
		boot.Ports = append(boot.Ports, krunPort{Port: krunProxyPort, Path: path})
	}

	encoded, err := json.Marshal(boot)
	if err != nil {
		m.Stop(context.Background())
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		m.Stop(context.Background())
		return nil, err
	}
	cmd := exec.Command(exe, "__machine", base64.RawURLEncoding.EncodeToString(encoded))
	// Both halves of the log, into the one file. The guest's serial port is
	// the child's stdout and libkrun's own complaints are its stderr, so a
	// machine that never got as far as a kernel still leaves a reason.
	if log != nil {
		cmd.Stdout, cmd.Stderr = log, log
	} else {
		cmd.Stdout, cmd.Stderr = nil, os.Stderr
	}
	// The machine dies with john, whether or not john gets to say so.
	//
	// A sandbox is one machine over one disk, and the instance lock is what
	// enforces that — but a lock held by a process that was killed is a lock
	// that is gone, while the machine it was holding for is still running and
	// still writing. The next john would take the lock, boot a second kernel
	// onto the same filesystem, and both would be right about owning it.
	// Pdeathsig closes that: the kernel kills the machine when the thread that
	// started it goes, which includes john being killed outright.
	//
	// It is the thread and not the process, which is why the thread is pinned
	// across the Start. A Go goroutine moves between threads freely, and a
	// locked thread whose goroutine has exited is destroyed — either would
	// deliver the signal to a machine whose john is alive and well.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	runtime.LockOSThread()
	err = cmd.Start()
	runtime.UnlockOSThread()
	if err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: kvm: starting the machine: %w", err)
	}
	m.proc = cmd
	go func() {
		cmd.Wait()
		close(m.exited)
	}()

	// The lower layer first, in the order the agent opens them, and both
	// before returning — VM.Build serves the lower layer the moment it has
	// one. Waiting for the agent to be ready is not done here: every machine
	// needs that wait, so VM.Build owns it.
	deadline := time.Now().Add(2 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if m.lower, err = m.accept(ctx, m.lowerLn, "lower", deadline); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: kvm lower channel: %w%s", err, m.whereToLook())
	}
	if m.control, err = m.accept(ctx, m.controlLn, "control", deadline); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: kvm control channel: %w%s", err, m.whereToLook())
	}
	m.serveProxy()
	return m, nil
}

// krunConsoleDevice is what the machine's serial port is called inside the
// guest. It is not the same name on both architectures — an x86 guest's first
// serial port is ttyS0 and an ARM guest's is ttyAMA0 — and a kernel told the
// wrong one boots silently, which looks exactly like a kernel that did not
// boot.
//
// It is this binary's architecture rather than a configured one because the
// machine virtualizes rather than emulates: the guest's processor is the
// host's, and image.go has already refused a build whose architecture is not
// the machine's.
func krunConsoleDevice() string {
	if runtime.GOARCH == "arm64" {
		return "ttyAMA0"
	}
	return "ttyS0"
}

// kernelFormat works out how libkrun should read the guest kernel.
//
// The guest image is not built for any one machine — every machine boots the
// same two files, and the image build is not this port's to change — so what
// arrives here is whatever Alpine ships for this architecture, and libkrun
// takes an enum saying which of several shapes that is. It is read out of the
// file rather than assumed from the architecture, because an assumption that
// stops being true produces a machine that does not boot and says nothing.
//
// Two shapes turn up. On arm64 the image is a bare Linux Image, already
// bootable, which is loaded as it is. On x86-64 it is a bzImage: a setup
// header, a decompressor, and a compressed ELF vmlinux behind them. libkrun
// has no bzImage loader, but its "compressed image" formats do exactly what is
// needed — find the compressed payload, decompress it, load the ELF — so the
// job here is to say which compression, and to check the one assumption those
// formats make.
func kernelFormat(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Enough for the boot protocol's header, which is where everything this
	// needs to know is written down.
	head := make([]byte, 1024)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, fmt.Errorf("sandbox: reading the guest kernel: %w", err)
	}
	head = head[:n]

	switch {
	case len(head) >= 4 && bytes.Equal(head[:4], []byte{0x7f, 'E', 'L', 'F'}):
		return krunKernelELF, nil
	case len(head) >= 0x3c && string(head[0x38:0x3c]) == "ARM\x64":
		// An arm64 Image. Its magic is at 0x38 and it is loaded whole.
		return krunKernelRaw, nil
	case len(head) >= 0x250 && string(head[0x202:0x206]) == "HdrS":
		return bzImageFormat(f, head)
	}
	return 0, fmt.Errorf("sandbox: %s is not a kernel any machine john ships can boot", path)
}

// bzImageFormat names the compression an x86 bzImage carries its vmlinux in,
// having first checked that libkrun will find that payload and not something
// else.
//
// libkrun locates the payload by searching the whole file for the first
// occurrence of the compression's magic bytes. That is a guess, and it is
// right for the image john pins — but it is a guess that would fail silently
// into a kernel that is not a kernel, so this works out where the payload
// actually is, from the header, and refuses if anything earlier in the file
// looks like it.
func bzImageFormat(f *os.File, head []byte) (uint32, error) {
	setupSects := int(head[0x1f1])
	if setupSects == 0 {
		setupSects = 4 // what the boot protocol means by zero
	}
	start := int64(setupSects+1)*512 + int64(binary.LittleEndian.Uint32(head[0x248:0x24c]))

	magic := make([]byte, 4)
	if _, err := f.ReadAt(magic, start); err != nil {
		return 0, fmt.Errorf("sandbox: the guest kernel's payload is not where its header says: %w", err)
	}
	var format uint32
	var want []byte
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b, 0x08}):
		format, want = krunKernelImageGz, magic[:3]
	case bytes.HasPrefix(magic, []byte("BZh")):
		format, want = krunKernelImageBz2, magic[:3]
	case bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		format, want = krunKernelImageZstd, magic[:4]
	default:
		return 0, fmt.Errorf("sandbox: the guest kernel is compressed with something "+
			"libkrun cannot read (payload begins %x)", magic)
	}

	// Nothing before the payload may look like the payload, because the first
	// match is what libkrun will take.
	prefix := make([]byte, start)
	if _, err := f.ReadAt(prefix, 0); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	if i := bytes.Index(prefix, want); i >= 0 {
		return 0, fmt.Errorf("sandbox: this guest kernel cannot be booted by libkrun: its "+
			"compressed payload is at 0x%x, but the same magic appears at 0x%x and libkrun "+
			"takes the first one", start, i)
	}
	return format, nil
}

// krunMachine is one running virtual machine: a child process with libkrun in
// it, and the sockets its guest arrived on.
type krunMachine struct {
	sockDir string
	console string
	log     *os.File
	proc    *exec.Cmd
	// exited is closed when the child is gone, so a wait for the guest ends
	// when the machine dies rather than at the end of a timeout.
	exited      chan struct{}
	proxySocket string

	controlLn *net.UnixListener
	lowerLn   *net.UnixListener
	proxyLn   *net.UnixListener
	control   net.Conn
	lower     net.Conn
	once      sync.Once
	// relays is the proxy's accept loop, waited for before the listener it
	// reads from is let go of.
	relays sync.WaitGroup
}

func (m *krunMachine) Control() net.Conn { return m.control }
func (m *krunMachine) Lower() net.Conn   { return m.lower }
func (m *krunMachine) Root() string      { return "" }

func (m *krunMachine) ProxyAddr() string {
	if m.proxyLn == nil {
		return ""
	}
	return "vsock:" + strconv.Itoa(krunProxyPort)
}

// serveProxy relays the guest's connections to the john proxy on the host.
//
// This is the sandbox's only route out, and it is a route to one socket rather
// than to a network: the machine has no interface, so nothing inside can reach
// anything this loop does not carry, and everything it carries arrives at the
// proxy to be decided. Unlike the other two streams it accepts many times — a
// program inside opens a connection per request.
func (m *krunMachine) serveProxy() {
	if m.proxyLn == nil {
		return
	}
	m.relays.Add(1)
	go func() {
		defer m.relays.Done()
		for {
			conn, err := m.proxyLn.Accept()
			if err != nil {
				// The listener has closed, which is the only way out of this
				// loop; Stop closes it and then waits here.
				return
			}
			// The connections themselves are not waited for: they end when
			// the machine does, and Stop kills it after this loop has gone.
			go m.relay(conn.(*net.UnixConn))
		}
	}()
}

// relay joins one connection from inside the machine to the proxy's socket.
//
// The other machines' relays are shorter than this, and the difference is the
// last two lines. libkrun carries a guest's vsock connection on a Unix socket
// here, and it throws away whatever it has not yet handed the guest the moment
// that socket hangs up: its proxy answers the poll's HANG_UP by resetting the
// guest's connection without draining what is still buffered. The john proxy
// closes as soon as it has written a response, so the end of nearly every
// reply was in that buffer — and whether the guest had read it first was a
// race. Roughly a third of requests came back as "connection closed
// prematurely" for a response the proxy had sent in full.
//
// Half-closing is what avoids it, and it is not a trick: a shutdown is not a
// hang-up, so libkrun reads the socket to the end, hands every byte to the
// guest and only then says the peer is done — which is what the guest needed
// to be told. So this end is shut for writing when the proxy has finished, and
// closed only once the guest has closed too, which is also why the machine
// relays this at all rather than giving libkrun the proxy's own socket: it has
// to be the one that closes last, and it can only be that if it holds the end.
func (m *krunMachine) relay(guest *net.UnixConn) {
	defer guest.Close()
	host, err := net.Dial("unix", m.proxySocket)
	if err != nil {
		return
	}
	defer host.Close()

	fromGuest := make(chan struct{})
	go func() {
		io.Copy(host, guest)
		// Half-close, so a request whose body has ended is answered rather
		// than left waiting for a peer that has said everything it has to.
		if c, ok := host.(*net.UnixConn); ok {
			c.CloseWrite()
		}
		close(fromGuest)
	}()
	io.Copy(guest, host)
	guest.CloseWrite()
	<-fromGuest
}

func (m *krunMachine) whereToLook() string {
	if m.console == "" {
		return ""
	}
	return " (see " + m.console + ")"
}

// accept waits for the guest to open one of the streams, in short steps rather
// than one long one — so a cancelled context or a machine that died on the way
// up is noticed while it is happening, rather than at the end of a timeout
// nobody is still waiting for.
func (m *krunMachine) accept(ctx context.Context, ln *net.UnixListener, what string, deadline time.Time) (net.Conn, error) {
	for {
		ln.SetDeadline(time.Now().Add(200 * time.Millisecond))
		conn, err := ln.Accept()
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, err
		}
		select {
		case <-m.exited:
			return nil, fmt.Errorf("the machine stopped before the guest opened the %s link", what)
		default:
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the guest never opened the %s link", what)
		}
	}
}

func (m *krunMachine) Stop(ctx context.Context) error {
	m.once.Do(func() {
		if m.control != nil {
			m.control.Close()
		}
		if m.lower != nil {
			m.lower.Close()
		}
		// Closing the listener is what ends the proxy's accept loop, so it
		// happens before the wait rather than after it.
		if m.proxyLn != nil {
			m.proxyLn.Close()
		}
		m.relays.Wait()
		if m.controlLn != nil {
			m.controlLn.Close()
		}
		if m.lowerLn != nil {
			m.lowerLn.Close()
		}
		// The machine is a process, and stopping it is killing it. There is
		// no gentler call to make: libkrun's shutdown device is macOS-only,
		// and the guest has already been asked to go — the agent exits when
		// its link closes and the guest's init unmounts its disks and powers
		// off after it. The kill is for a guest that has not managed that in
		// time.
		if m.proc != nil && m.proc.Process != nil {
			select {
			case <-m.exited:
			case <-time.After(shutdownGrace):
				m.proc.Process.Kill()
				<-m.exited
			}
		}
		if m.log != nil {
			m.log.Close()
		}
		os.RemoveAll(m.sockDir)
	})
	return nil
}

// The machine's other half
//
// Everything below here runs in the child process, which is john started as
// `john __machine`. It is the same binary, so there is no second thing to
// build, ship or keep in step — the same arrangement `john __guest` has inside
// the sandbox.

// krunBoot is what the machine is told to start. It is the whole of the
// interface between the two processes, and it is a JSON document on the
// command line for the same reason the guest's configuration is one on the
// kernel command line: at the moment it is needed there is no channel yet.
type krunBoot struct {
	Kernel      string     `json:"kernel"`
	Format      uint32     `json:"format"`
	Initramfs   string     `json:"initramfs"`
	Cmdline     string     `json:"cmdline"`
	StateDisk   string     `json:"state_disk"`
	EnvDisk     string     `json:"env_disk,omitempty"`
	EnvReadOnly bool       `json:"env_read_only,omitempty"`
	CPUs        int        `json:"cpus"`
	MemoryMiB   int        `json:"memory_mib"`
	Ports       []krunPort `json:"ports"`
}

// krunPort is one guest vsock port and the host socket libkrun joins it to.
type krunPort struct {
	Port uint32 `json:"port"`
	Path string `json:"path"`
}

// runMachineMonitor is the child. It configures libkrun from what it was told
// and enters the virtual machine, which it does not come back from: on success
// libkrun runs the machine on this process and exits it when the guest stops.
func runMachineMonitor(args []string) error {
	if len(args) != 1 {
		return errors.New("sandbox: __machine is started by the sandbox, not by hand")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(args[0])
	if err != nil {
		return fmt.Errorf("sandbox: unreadable machine configuration: %w", err)
	}
	var b krunBoot
	if err := json.Unmarshal(encoded, &b); err != nil {
		return fmt.Errorf("sandbox: unreadable machine configuration: %w", err)
	}

	lib, err := loadLibkrun()
	if err != nil {
		return err
	}

	// The parent pointed both of this process's outputs at the sandbox's
	// console log, so the guest's serial port and libkrun's own log go there
	// by writing to them.
	lib.initLog(int32(os.Stderr.Fd()), krunLogWarn, krunLogStyleNever, 0)

	ctx := lib.createCtx()
	if err := krunError("creating a context", ctx); err != nil {
		return err
	}
	id := uint32(ctx)

	// libkrun's own devices go before john adds any; see disableVsock.
	if err := krunError("turning off the implicit socket device", lib.disableVsock(id)); err != nil {
		return err
	}
	if err := krunError("turning off the implicit console", lib.disableConsole(id)); err != nil {
		return err
	}

	if err := krunError("setting the machine's size",
		lib.setVMConfig(id, uint8(min(b.CPUs, 255)), uint32(b.MemoryMiB))); err != nil {
		return err
	}
	if err := krunError("setting the kernel",
		lib.setKernel(id, b.Kernel, b.Format, b.Initramfs, b.Cmdline)); err != nil {
		return err
	}
	// The sandbox's own disk first and the workspace's environment second,
	// because that order is the only thing that identifies them to the guest:
	// virtio names disks for the order they were attached in, so these are
	// /dev/vda and /dev/vdb inside. See sandbox/guest/init.
	if err := krunError("attaching the sandbox's disk",
		lib.addDisk(id, "state", b.StateDisk, krunDiskRaw, false)); err != nil {
		return err
	}
	if b.EnvDisk != "" {
		if err := krunError("attaching the environment disk",
			lib.addDisk(id, "env", b.EnvDisk, krunDiskRaw, b.EnvReadOnly)); err != nil {
			return err
		}
	}

	// The socket device, with none of libkrun's transparent socket
	// forwarding: the guest gets a vsock device and the ports named below and
	// nothing else. TSI would give programs inside a way out to the host's
	// network that no policy of john's had seen.
	if err := krunError("adding the socket device", lib.addVsock(id, 0)); err != nil {
		return err
	}
	for _, p := range b.Ports {
		// listen=false: the guest opens these and libkrun dials the host path
		// for it, which is the direction every one of john's streams runs in.
		if err := krunError("mapping vsock port "+strconv.Itoa(int(p.Port)),
			lib.addVsockPort(id, p.Port, p.Path, false)); err != nil {
			return err
		}
	}

	// The guest's console. Output goes to this process's stdout, which is the
	// sandbox's console log. Input is the read end of a pipe nobody writes to
	// — there is nobody at the guest's console to type at it, and a serial
	// port reading an immediately-closed file is a device that reports end of
	// input forever.
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	if err := krunError("adding the console",
		lib.addSerial(id, int32(r.Fd()), int32(os.Stdout.Fd()))); err != nil {
		return err
	}

	// libkrun runs the machine on this thread and takes the process with it.
	runtime.LockOSThread()
	code := lib.startEnter(id)
	runtime.KeepAlive(r)
	runtime.KeepAlive(w)
	if err := krunError("starting the machine", code); err != nil {
		return err
	}
	// Not reached: krun_start_enter only returns on failure.
	return nil
}
