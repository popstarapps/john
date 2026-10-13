//go:build windows

package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: Hyper-V.
//
// Hyper-V provides the machine, so this configures one rather than
// implementing anything — the same shape as the macOS machine next door, and
// the same four jobs: start a Linux virtual machine with the caller's
// resources, boot the guest image with john.cfg on the kernel command line,
// give the guest two streams, and return once both are connected here.
//
// It is reached through the Host Compute System API in vmcompute.dll rather
// than through WMI, which is the decision this port turns on and is written
// down in hcs_windows.go. Two consequences are worth knowing before reading
// any further:
//
// The machine is a JSON document. HCS takes a configuration document and hands
// back a compute system; hcsDocument below is that document, and the whole of
// what john asks Windows for.
//
// There is direct kernel boot after all. The Hyper-V manager cannot do it —
// a Generation 2 virtual machine is UEFI and takes a bootable disk — which is
// why this platform was expected to need the guest image rebuilt as an EFI
// application on an EFI system partition. HCS can:
// Chipset.LinuxKernelDirect takes a kernel, an initrd and a command line, and
// is what WSL 2 boots with. So the guest image is untouched by this port and
// every machine still boots the same two files.
//
// The guest reaches the host over hv_sock, which is a different socket family
// with the same shape as vsock; the mapping is in hvsock_windows.go, and the
// agent inside needed no Windows-specific code at all.
type hyperv struct {
	// Kernel, Initramfs and Console override what the boot config carries.
	// They exist for the same reason the other machines have them: a test
	// needs to point the machine at an image it built itself rather than one
	// extracted from the binary, and needs to know where the boot log went.
	// Nothing outside a test sets them.
	Kernel    string
	Initramfs string
	Console   string
}

func platformHypervisor() hypervisor { return &hyperv{} }

func (h *hyperv) Name() string { return "hyper-v" }

// The ports the guest dials back on. They are the machine's own choice and
// travel to the guest in its configuration, so nothing outside this file has
// to agree with them — they are the same numbers the macOS machine uses only
// because a trace with the same ports on both platforms is easier to read.
const (
	hvControlPort = 2201
	hvLowerPort   = 2202
	// The third stream, and the only one the guest opens more than once: each
	// connection a program inside makes to the proxy is its own.
	hvProxyPort = 2203
)

// Available says which of the reasons it is, because they ask different things
// of the person reading it.
//
// Three of them are ordinary on a developer's machine and none of them is a
// failure: the Virtual Machine Platform feature has never been turned on, the
// firmware has virtualization disabled, or Windows is refusing this process
// the right to create a virtual machine. That last one is the one john will
// meet most, because creating a compute system needs an elevated token or
// membership of Hyper-V Administrators, and neither is a thing a developer
// gets by accident — so it is the one john offers to fix rather than only
// report. See admin_windows.go; the other two stay messages, because turning a
// Windows feature on and rebooting is a larger change to somebody's computer
// than a yes/no question should carry.
func (h *hyperv) Available() error {
	if err := computeCore.Load(); err != nil {
		return fmt.Errorf("sandbox: this Windows has no Hyper-V compute service (%w). "+
			"Turn on the Virtual Machine Platform feature and restart:  "+
			"dism /online /enable-feature /featurename:VirtualMachinePlatform /all", err)
	}
	err := hcsProbe()
	if err == nil {
		return nil
	}
	switch hcsCode(err) {
	case hcsErrAccessDenied:
		if inHyperVAdmins() {
			// The step is already taken; only the sign-in that makes it count
			// is missing, and offering the step again would not supply it.
			return fmt.Errorf("sandbox: %w", errSignInForHyperVAdmin)
		}
		// Wrapped rather than described, because this is the one reason john
		// can do something about rather than only report: AdminSetupFor picks
		// it out and the client offers to take the step. See admin_windows.go.
		return fmt.Errorf("sandbox: Windows will not let this process create a virtual machine — %w",
			errNeedsHyperVAdmin)
	case hcsErrHypervNotInstalled, hcsErrServiceNotAvailable, rpcServerUnavailable:
		return fmt.Errorf("sandbox: Hyper-V is not available on this machine (%w). "+
			"Turn on the Virtual Machine Platform feature and restart:  "+
			"dism /online /enable-feature /featurename:VirtualMachinePlatform /all — "+
			"and if that is already on, hardware virtualization is disabled in the firmware", err)
	}
	return fmt.Errorf("sandbox: Hyper-V: %w", err)
}

func (h *hyperv) Boot(ctx context.Context, cfg bootConfig) (machine, error) {
	if err := h.Available(); err != nil {
		return nil, err
	}
	kernel, initramfs := h.Kernel, h.Initramfs
	if kernel == "" {
		kernel = cfg.Kernel
	}
	if initramfs == "" {
		initramfs = cfg.Initramfs
	}
	if kernel == "" || initramfs == "" {
		return nil, ErrNoGuestImage
	}
	// vmcompute is a service, and the paths in the document are opened by it
	// rather than by this process. A relative path would be resolved against
	// its working directory, which is not ours.
	kernel, err := filepath.Abs(kernel)
	if err != nil {
		return nil, err
	}
	if initramfs, err = filepath.Abs(initramfs); err != nil {
		return nil, err
	}

	// The machine's identity, which is also the address of its half of every
	// hv_sock stream. A fresh one per boot: an id that outlived a machine
	// would be a listener a later sandbox could arrive at.
	vmID, err := windows.GenerateGUID()
	if err != nil {
		return nil, err
	}
	id := strings.Trim(vmID.String(), "{}")

	guest := GuestConfig{
		Mount:          cfg.Mount,
		Upper:          cfg.Upper,
		Control:        "vsock:" + strconv.Itoa(hvControlPort),
		Lower:          "vsock:" + strconv.Itoa(hvLowerPort),
		MaxOutputBytes: cfg.MaxOutputBytes,
		MaxProcesses:   cfg.Resources.Processes,
	}
	raw, err := jsonMarshal(guest)
	if err != nil {
		return nil, err
	}

	m := &hypervMachine{id: id, proxySocket: cfg.ProxySocket}

	// The sandbox's own disk. It is a VHDX here rather than the sparse raw
	// file the other machines take — see disk_windows.go and vhd_windows.go —
	// but the name and the making of it are the package's, not this
	// machine's, so that the rebuild and the reclaim can find it too.
	state, err := stateDisk(cfg)
	if err != nil {
		return nil, err
	}

	// The console log always exists rather than only when somebody asked for
	// it: boot is where this goes wrong, it is written once per start, and a
	// developer whose sandbox did not come up should have something to read.
	console := h.Console
	if console == "" {
		console = filepath.Join(cfg.Dir, "console.log")
	}
	pipe, err := m.openConsole(console)
	if err != nil {
		// A machine with no console still runs. Losing the log is worth
		// saying and is not worth refusing to boot over.
		fmt.Fprintf(os.Stderr, "john: the sandbox's console could not be opened: %v\n", err)
		console, pipe = "", ""
	}
	m.console = console

	doc, err := hcsDoc(hcsBoot{
		id:        id,
		kernel:    kernel,
		initramfs: initramfs,
		cmdline:   "console=ttyS0,115200 panic=1 john.cfg=" + encodeGuestConfig(raw),
		state:     state,
		envDisk:   cfg.EnvDisk,
		envRO:     !cfg.EnvWritable,
		console:   pipe,
		resources: cfg.Resources,
	})
	if err != nil {
		m.Stop(context.Background())
		return nil, err
	}

	// Everything the document names is opened by the virtual machine's own
	// worker process, which is a security principal with no rights to
	// anything of the developer's until it is given them.
	for _, path := range []string{kernel, initramfs, state, cfg.EnvDisk} {
		if path == "" {
			continue
		}
		if err := grantVMAccess(id, path); err != nil {
			m.Stop(context.Background())
			return nil, fmt.Errorf("sandbox: hyper-v: %w", err)
		}
	}

	if m.sys, err = hcsCreate(ctx, id, doc); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: hyper-v: %w", err)
	}

	// The listeners go up before the machine starts, not after. The agent
	// dials each stream once and does not retry, and a guest that boots in a
	// third of a second is quite capable of arriving before a listener that
	// was opened afterwards.
	for _, l := range []struct {
		port int
		into **hvListener
	}{{hvLowerPort, &m.lowerLn}, {hvControlPort, &m.controlLn}, {hvProxyPort, &m.proxyLn}} {
		if l.port == hvProxyPort && cfg.ProxySocket == "" {
			continue
		}
		ln, err := listenHvSock(vmID, uint32(l.port))
		if err != nil {
			m.Stop(context.Background())
			return nil, fmt.Errorf("sandbox: hyper-v: %w", err)
		}
		*l.into = ln
	}

	if err := m.sys.start(ctx); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: hyper-v: starting the machine: %w", err)
	}

	// The lower layer first, in the order the agent opens them, and both
	// before returning — VM.Build serves the lower layer the moment it has
	// one. Waiting for the agent to be ready is not done here: every machine
	// needs that wait, so VM.Build owns it.
	deadline := time.Now().Add(2 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if m.lower, err = m.lowerLn.accept(ctx, deadline); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: hyper-v lower channel: %w%s", err, m.whereToLook())
	}
	if m.control, err = m.controlLn.accept(ctx, deadline); err != nil {
		m.Stop(context.Background())
		return nil, fmt.Errorf("sandbox: hyper-v control channel: %w%s", err, m.whereToLook())
	}
	m.serveProxy()
	return m, nil
}

func (m *hypervMachine) whereToLook() string {
	if m.console == "" {
		return ""
	}
	return " (see " + m.console + ")"
}

// hypervMachine is one running virtual machine.
type hypervMachine struct {
	id          string
	sys         *hcsSystem
	console     string
	consoleSrv  *consolePipe
	proxySocket string

	controlLn *hvListener
	lowerLn   *hvListener
	proxyLn   *hvListener

	control net.Conn
	lower   net.Conn
	once    sync.Once
	// relays is the proxy accept loop, waited for before the machine is let
	// go of, so nothing is still accepting on a listener that has closed.
	relays sync.WaitGroup
}

func (m *hypervMachine) Control() net.Conn { return m.control }
func (m *hypervMachine) Lower() net.Conn   { return m.lower }
func (m *hypervMachine) Root() string      { return "" }

func (m *hypervMachine) ProxyAddr() string {
	if m.proxySocket == "" || m.proxyLn == nil {
		return ""
	}
	return "vsock:" + strconv.Itoa(hvProxyPort)
}

// serveProxy relays the guest's connections to the john proxy on the host.
//
// This is the sandbox's only route out, and it is a route to one socket rather
// than to a network: the machine has no network adapter, so nothing inside can
// reach anything this loop does not carry, and everything it carries arrives
// at the proxy to be decided. Unlike the other two streams it accepts many
// times — a program inside opens a connection per request.
func (m *hypervMachine) serveProxy() {
	if m.proxyLn == nil {
		return
	}
	m.relays.Add(1)
	go func() {
		defer m.relays.Done()
		for {
			conn, err := m.proxyLn.accept(context.Background(), time.Time{})
			if err != nil {
				// The listener has closed, which is the only way out of this
				// loop; Stop closes it and then waits here.
				return
			}
			go m.relay(conn)
		}
	}()
}

// relay joins one connection from inside the machine to the proxy's socket.
func (m *hypervMachine) relay(guest net.Conn) {
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
	// The proxy has said everything. Closing now would drop whatever the
	// guest has not read yet, so the stream is ended instead, and closed once
	// the guest has read to the end and closed its side — which is what ends
	// the copy above.
	if c, ok := guest.(interface{ CloseWrite() error }); !ok || c.CloseWrite() != nil {
		guest.Close()
	}
	<-done
	guest.Close()
	host.Close()
}

func (m *hypervMachine) Stop(ctx context.Context) error {
	var err error
	m.once.Do(func() {
		if m.control != nil {
			m.control.Close()
		}
		if m.lower != nil {
			m.lower.Close()
		}
		// Closing the listener is what ends the proxy's accept loop, so it
		// happens before the wait rather than after it.
		m.proxyLn.close()
		m.relays.Wait()
		m.controlLn.close()
		m.lowerLn.close()
		if m.sys != nil {
			err = m.sys.close(ctx)
		}
		m.consoleSrv.close()
	})
	return err
}

// The configuration document
//
// This is the whole of what john asks Windows for, and it is deliberately
// small. There is no network adapter, no video, no keyboard, no guest
// integration service and no GCS connection: the machine has two disks, a
// socket device and a serial port, and everything else it might have had is a
// way in or a way out that would then need policing.

type hcsBoot struct {
	id        string
	kernel    string
	initramfs string
	cmdline   string
	state     string
	envDisk   string
	envRO     bool
	console   string
	resources Resources
}

func hcsDoc(b hcsBoot) ([]byte, error) {
	// Who may bind an hv_sock service for this machine, and who may connect
	// to one. The machine's own worker process needs it, and so does whoever
	// started john — which is not necessarily an administrator, since
	// membership of Hyper-V Administrators is the other way to get here.
	sd := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if sid, err := currentUserSID(); err == nil {
		sd += "(A;;FA;;;" + sid + ")"
	}

	scsi := map[string]hcsScsi{"0": {Attachments: map[string]hcsAttachment{
		// LUN 0 is the sandbox's own disk and LUN 1 is the workspace's
		// environment, which is the order the guest finds them in — /dev/sda
		// and /dev/sdb, the way the other machines' guests find /dev/vda and
		// /dev/vdb.
		"0": {Type: "VirtualDisk", Path: b.state},
	}}}
	if b.envDisk != "" {
		scsi["0"].Attachments["1"] = hcsAttachment{
			Type: "VirtualDisk", Path: b.envDisk, ReadOnly: b.envRO,
		}
	}

	devices := hcsDevices{
		Scsi: scsi,
		HvSocket: &hcsHvSocket{HvSocketConfig: hcsHvSocketConfig{
			DefaultBindSecurityDescriptor:    sd,
			DefaultConnectSecurityDescriptor: sd,
		}},
	}
	if b.console != "" {
		devices.ComPorts = map[string]hcsComPort{"0": {NamedPipe: b.console}}
	}

	return json.Marshal(hcsDocument{
		// 2.1 is the oldest schema that carries everything used here, and
		// naming the oldest is what keeps the machine working on a Windows
		// older than the one it was built on.
		SchemaVersion: hcsVersion{Major: 2, Minor: 1},
		Owner:         "john",
		// The machine's life is the handle john holds. A client that crashes
		// does not leave a virtual machine running with a developer's
		// workspace mounted into it.
		ShouldTerminateOnLastHandleClosed: true,
		VirtualMachine: hcsVirtualMachine{
			// The agent powers the guest off when it exits, and that is how
			// the machine is meant to end. Without this the guest's poweroff
			// would be a reset and the machine would boot again.
			StopOnReset: true,
			Chipset: hcsChipset{
				// The guest's real-time clock keeps UTC, which is what Linux
				// reads it as. Left out, Hyper-V keeps the host's local time
				// there, the way Windows wants it: on a host at UTC+2 the
				// guest booted saying "setting system clock to 14:20 UTC" at
				// 12:20Z, and every mtime it wrote was two hours ahead.
				UseUtc: true,
				LinuxKernelDirect: &hcsLinuxKernelDirect{
					KernelFilePath: b.kernel,
					InitRdPath:     b.initramfs,
					KernelCmdLine:  b.cmdline,
				},
			},
			ComputeTopology: hcsComputeTopology{
				Memory: hcsMemory{
					SizeInMB: uint64(max(b.resources.MemoryGB, 1)) * 1024,
					// The sandbox is given memory rather than made to own it:
					// an idle machine costs the host what it is using, which
					// is what makes several open sessions bearable.
					AllowOvercommit: true,
				},
				Processor: hcsProcessor{Count: int32(max(b.resources.CPUs, 1))},
			},
			Devices: devices,
		},
	})
}

type hcsDocument struct {
	SchemaVersion                     hcsVersion
	Owner                             string
	ShouldTerminateOnLastHandleClosed bool
	VirtualMachine                    hcsVirtualMachine
}

type hcsVersion struct{ Major, Minor int }

type hcsVirtualMachine struct {
	StopOnReset     bool
	Chipset         hcsChipset
	ComputeTopology hcsComputeTopology
	Devices         hcsDevices
}

type hcsChipset struct {
	UseUtc            bool
	LinuxKernelDirect *hcsLinuxKernelDirect `json:",omitempty"`
}

type hcsLinuxKernelDirect struct {
	KernelFilePath string
	InitRdPath     string
	KernelCmdLine  string
}

type hcsComputeTopology struct {
	Memory    hcsMemory
	Processor hcsProcessor
}

type hcsMemory struct {
	SizeInMB        uint64
	AllowOvercommit bool
}

type hcsProcessor struct{ Count int32 }

type hcsDevices struct {
	Scsi     map[string]hcsScsi    `json:",omitempty"`
	HvSocket *hcsHvSocket          `json:",omitempty"`
	ComPorts map[string]hcsComPort `json:",omitempty"`
}

type hcsScsi struct {
	Attachments map[string]hcsAttachment
}

type hcsAttachment struct {
	Type     string
	Path     string
	ReadOnly bool `json:",omitempty"`
}

type hcsHvSocket struct {
	HvSocketConfig hcsHvSocketConfig
}

type hcsHvSocketConfig struct {
	DefaultBindSecurityDescriptor    string
	DefaultConnectSecurityDescriptor string
}

type hcsComPort struct {
	NamedPipe string
}

// currentUserSID is who is running john, in the form a security descriptor
// spells it.
func currentUserSID() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

// The console
//
// Hyper-V's serial port is a named pipe that the virtual machine's worker
// process connects to, so the host is the server. Everything the guest kernel
// prints arrives here and is written to a file under the sandbox's state
// directory — which is the file every boot failure in this machine points at.

type consolePipe struct {
	handle windows.Handle
	file   *os.File
	closed sync.Once
}

func (m *hypervMachine) openConsole(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	log, err := createConsole(path)
	if err != nil {
		return "", err
	}
	name := `\\.\pipe\john-console-` + m.id
	wname, err := windows.UTF16PtrFromString(name)
	if err != nil {
		log.Close()
		return "", err
	}
	// The worker process is not this user and is not an administrator: it is
	// the machine's own principal, and it will not be able to connect to a
	// pipe with the default descriptor. This one is a log of a Linux kernel
	// booting, written for one connection and then never again.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;WD)")
	if err != nil {
		log.Close()
		return "", err
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	h, err := windows.CreateNamedPipe(wname,
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT,
		1, 4096, 4096, 0, &sa)
	if err != nil {
		log.Close()
		return "", err
	}
	m.consoleSrv = &consolePipe{handle: h, file: log}
	go m.consoleSrv.pump()
	return name, nil
}

// pump waits for the machine to attach and then copies what it says into the
// log. It is best effort throughout: a console that never connects is a
// machine with no boot log, which is worse than having one and is not a reason
// to fail a boot that is otherwise fine.
func (c *consolePipe) pump() {
	if err := windows.ConnectNamedPipe(c.handle, nil); err != nil &&
		err != windows.ERROR_PIPE_CONNECTED {
		return
	}
	buf := make([]byte, 4096)
	for {
		var n uint32
		if err := windows.ReadFile(c.handle, buf, &n, nil); err != nil || n == 0 {
			return
		}
		c.file.Write(buf[:n])
	}
}

func (c *consolePipe) close() {
	if c == nil {
		return
	}
	c.closed.Do(func() {
		windows.CloseHandle(c.handle)
		c.file.Close()
	})
}
