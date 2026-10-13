package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Host Compute System API, which is how Windows is asked for a virtual
// machine.
//
// Hyper-V is drivable two ways and they are not equivalent here. WMI's
// Msvm_VirtualSystemManagementService is the older surface and the one every
// PowerShell example uses, and it needs the full Hyper-V role installed — the
// management stack, vmms.exe, the lot. The Host Compute System API in
// vmcompute.dll is the newer one, it is what WSL and Windows containers are
// built on, and it needs only the Virtual Machine Platform feature — the one a
// developer is overwhelmingly likely to already have, because enabling WSL 2
// turns it on. It is also a flat C API of eight functions rather than COM, so
// Go reaches it through x/sys/windows and the Windows client stays out of cgo,
// which is what keeps `make clients` able to cross-compile it from a Mac.
//
// So: HCS. The cost is that the machine is described by a JSON document whose
// schema is versioned by Windows release rather than documented; hcsDocument
// in machine_hyperv.go is the subset john uses and nothing more.
//
// Everything in this file is mechanism. The machine is next door.

var (
	computeCore    = windows.NewLazySystemDLL("computecore.dll")
	computeStorage = windows.NewLazySystemDLL("computestorage.dll")

	hcsCreateOperation         = computeCore.NewProc("HcsCreateOperation")
	hcsCloseOperation          = computeCore.NewProc("HcsCloseOperation")
	hcsWaitForOperationResult  = computeCore.NewProc("HcsWaitForOperationResult")
	hcsEnumerateComputeSystems = computeCore.NewProc("HcsEnumerateComputeSystems")
	hcsCreateComputeSystem     = computeCore.NewProc("HcsCreateComputeSystem")
	hcsStartComputeSystem      = computeCore.NewProc("HcsStartComputeSystem")
	hcsTerminateComputeSystem  = computeCore.NewProc("HcsTerminateComputeSystem")
	hcsCloseComputeSystem      = computeCore.NewProc("HcsCloseComputeSystem")
)

// hcsProcs are the entry points this machine cannot run without, checked
// together so that a Windows that has vmcompute but not this API says so
// rather than panicking on the first call: x/sys's LazyProc.Call panics when
// the procedure is missing, which is the wrong end of the boot to find out.
var hcsProcs = []*windows.LazyProc{
	hcsCreateOperation, hcsCloseOperation, hcsWaitForOperationResult,
	hcsEnumerateComputeSystems, hcsCreateComputeSystem, hcsStartComputeSystem,
	hcsTerminateComputeSystem, hcsCloseComputeSystem,
}

// HcsGrantVmAccess has moved. It is exported from computecore.dll on current
// Windows and from computestorage.dll on the builds where that library was
// split out, and a machine that looked in only one of them fails at the first
// file it tries to grant — which reads as a virtual machine that cannot open
// its own kernel. So it is resolved across both, once.
var grantProc = sync.OnceValues(func() (*windows.LazyProc, error) {
	for _, dll := range []*windows.LazyDLL{computeCore, computeStorage} {
		if dll.Load() != nil {
			continue
		}
		p := dll.NewProc("HcsGrantVmAccess")
		if p.Find() == nil {
			return p, nil
		}
	}
	return nil, errors.New("HcsGrantVmAccess is in neither computecore.dll nor computestorage.dll")
})

// The HRESULTs vmcompute answers with. They are the difference between "turn a
// Windows feature on", "run this once as an administrator" and "this machine
// cannot do it at all", so they are named rather than printed as hex.
const (
	hcsErrHypervNotInstalled   = 0x80370102
	hcsErrInvalidState         = 0x80370105
	hcsErrUnexpectedExit       = 0x80370106
	hcsErrTerminated           = 0x80370107
	hcsErrInvalidJSON          = 0x8037010D
	hcsErrSystemNotFound       = 0x8037010E
	hcsErrSystemAlreadyExists  = 0x8037010F
	hcsErrSystemAlreadyStopped = 0x80370110
	hcsErrServiceNotAvailable  = 0x80370114
	hcsErrOperationTimeout     = 0x80370118
	hcsErrAccessDenied         = 0x8037011B
	// Not an HCS code: the RPC layer's answer when vmcompute is not there to
	// be reached at all, which is what an unenabled feature looks like before
	// the service has ever been started.
	rpcServerUnavailable = 0x800706BA
)

// hcsError is a failed HCS call, carrying the HRESULT so that Available can
// tell the reasons apart, and whatever vmcompute said about it.
type hcsError struct {
	call   string
	code   uint32
	detail string
}

func (e *hcsError) Error() string {
	msg := fmt.Sprintf("%s: 0x%08x", e.call, e.code)
	if name := hcsCodeName(e.code); name != "" {
		msg += " (" + name + ")"
	}
	if e.detail != "" {
		msg += ": " + e.detail
	}
	return msg
}

func hcsCodeName(code uint32) string {
	switch code {
	case hcsErrHypervNotInstalled:
		return "Hyper-V is not installed"
	case hcsErrInvalidState:
		return "the compute system is in the wrong state"
	case hcsErrUnexpectedExit:
		return "the virtual machine exited unexpectedly"
	case hcsErrTerminated:
		return "the virtual machine was terminated"
	case hcsErrInvalidJSON:
		return "vmcompute rejected the configuration document"
	case hcsErrSystemNotFound:
		return "no such compute system"
	case hcsErrSystemAlreadyExists:
		return "a compute system with that name already exists"
	case hcsErrSystemAlreadyStopped:
		return "already stopped"
	case hcsErrServiceNotAvailable:
		return "the Hyper-V compute service is not available"
	case hcsErrOperationTimeout:
		return "the operation timed out"
	case hcsErrAccessDenied:
		return "access denied"
	case rpcServerUnavailable:
		return "the Hyper-V compute service could not be reached"
	default:
		return ""
	}
}

func hcsCode(err error) uint32 {
	var e *hcsError
	if errors.As(err, &e) {
		return e.code
	}
	return 0
}

// hcsCall runs one HCS operation to completion.
//
// Every call in this API has the same shape: an operation handle is made, the
// call is started against it, and its result is waited for. Wrapping the three
// keeps the machine readable and keeps the operation handle from outliving the
// call it belongs to — a leaked one is a handle vmcompute holds until the
// process exits.
func hcsCall(call string, timeout time.Duration, fn func(op uintptr) uintptr) (string, error) {
	op, _, _ := hcsCreateOperation.Call(0, 0)
	if op == 0 {
		return "", fmt.Errorf("%s: HcsCreateOperation returned nothing", call)
	}
	defer hcsCloseOperation.Call(op)

	// The synchronous half. A non-zero HRESULT here means the call was never
	// started, so there is no result to wait for.
	if hr := uint32(fn(op)); hr != 0 {
		return "", &hcsError{call: call, code: hr}
	}

	ms := uintptr(timeout / time.Millisecond)
	if timeout <= 0 {
		ms = windows.INFINITE
	}
	var result *uint16
	hr, _, _ := hcsWaitForOperationResult.Call(op, ms, uintptr(unsafe.Pointer(&result)))
	// The result document is written on failure as well as on success, and on
	// failure it is where the reason is. Either way it is ours to free.
	doc := ""
	if result != nil {
		doc = windows.UTF16PtrToString(result)
		windows.LocalFree(windows.Handle(unsafe.Pointer(result)))
	}
	if uint32(hr) != 0 {
		return doc, &hcsError{call: call, code: uint32(hr), detail: hcsResultMessage(doc)}
	}
	return doc, nil
}

// hcsResultMessage pulls the human half out of a result document. vmcompute
// answers a failure with {"Error":...,"ErrorMessage":"..."}, and the message is
// the only part worth putting in front of a person.
func hcsResultMessage(doc string) string {
	if doc == "" {
		return ""
	}
	var r struct {
		ErrorMessage string `json:"ErrorMessage"`
	}
	if err := json.Unmarshal([]byte(doc), &r); err == nil && r.ErrorMessage != "" {
		return r.ErrorMessage
	}
	return ""
}

// hcsProbe asks vmcompute a question that changes nothing, so that Available
// can find out which of the several reasons it is. Enumerating is the cheapest
// call in the API and it goes through the same access check as creating a
// machine, which is the check that matters.
func hcsProbe() error {
	if err := computeCore.Load(); err != nil {
		return err
	}
	for _, p := range hcsProcs {
		if err := p.Find(); err != nil {
			return fmt.Errorf("vmcompute here has no %s: %w", p.Name, err)
		}
	}
	if _, err := grantProc(); err != nil {
		return err
	}
	query, err := windows.UTF16PtrFromString("{}")
	if err != nil {
		return err
	}
	_, err = hcsCall("HcsEnumerateComputeSystems", 30*time.Second, func(op uintptr) uintptr {
		hr, _, _ := hcsEnumerateComputeSystems.Call(uintptr(unsafe.Pointer(query)), op)
		return hr
	})
	return err
}

// hcsSystem is one compute system: a virtual machine vmcompute is holding open
// for us. The handle is the machine's life — the document sets
// ShouldTerminateOnLastHandleClosed, so a john that crashes does not leave a
// virtual machine running with a developer's workspace mounted into it.
type hcsSystem struct {
	handle uintptr
	id     string
}

func hcsCreate(ctx context.Context, id string, doc []byte) (*hcsSystem, error) {
	wid, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, err
	}
	wdoc, err := windows.UTF16PtrFromString(string(doc))
	if err != nil {
		return nil, err
	}
	var handle uintptr
	if _, err := hcsCall("HcsCreateComputeSystem", hcsTimeout(ctx, 2*time.Minute), func(op uintptr) uintptr {
		hr, _, _ := hcsCreateComputeSystem.Call(uintptr(unsafe.Pointer(wid)),
			uintptr(unsafe.Pointer(wdoc)), op, 0, uintptr(unsafe.Pointer(&handle)))
		return hr
	}); err != nil {
		// The handle is written before the operation completes, so a create
		// that failed can still have left one behind.
		if handle != 0 {
			hcsCloseComputeSystem.Call(handle)
		}
		return nil, err
	}
	return &hcsSystem{handle: handle, id: id}, nil
}

func (s *hcsSystem) start(ctx context.Context) error {
	_, err := hcsCall("HcsStartComputeSystem", hcsTimeout(ctx, 2*time.Minute), func(op uintptr) uintptr {
		hr, _, _ := hcsStartComputeSystem.Call(s.handle, op, 0)
		return hr
	})
	return err
}

// close terminates the machine and lets go of it.
//
// Terminate rather than shut down: there is no guest integration service in
// this machine to ask politely, the agent's own exit already powers the guest
// off from inside, and this path is the one that runs when that did not
// happen. A machine that has already stopped says so, which is not a failure.
func (s *hcsSystem) close(ctx context.Context) error {
	if s.handle == 0 {
		return nil
	}
	_, err := hcsCall("HcsTerminateComputeSystem", hcsTimeout(ctx, 30*time.Second), func(op uintptr) uintptr {
		hr, _, _ := hcsTerminateComputeSystem.Call(s.handle, op, 0)
		return hr
	})
	switch hcsCode(err) {
	case hcsErrSystemAlreadyStopped, hcsErrSystemNotFound, hcsErrInvalidState:
		err = nil
	}
	hcsCloseComputeSystem.Call(s.handle)
	s.handle = 0
	return err
}

func hcsTimeout(ctx context.Context, def time.Duration) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		if left := time.Until(d); left > 0 && left < def {
			return left
		}
	}
	return def
}

// grantVMAccess lets the virtual machine's worker process open one of our
// files.
//
// A compute system runs as a security principal of its own — NT VIRTUAL
// MACHINE\<id> — and that principal has no rights to anything in the
// developer's profile. Every path named in the configuration document has to
// be granted one at a time: the kernel, the initramfs and each disk. A
// forgotten one is a boot that fails inside vmwp with an access denied nobody
// ever sees.
func grantVMAccess(id, path string) error {
	grant, err := grantProc()
	if err != nil {
		return err
	}
	wid, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return err
	}
	wpath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	hr, _, _ := grant.Call(uintptr(unsafe.Pointer(wid)), uintptr(unsafe.Pointer(wpath)))
	if uint32(hr) != 0 {
		return &hcsError{call: "HcsGrantVmAccess(" + path + ")", code: uint32(hr)}
	}
	return nil
}
