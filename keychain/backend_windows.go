//go:build windows

package keychain

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows Credential Manager, through advapi32.
//
// The API rather than a program: cmdkey is for domain credentials and takes
// the password as an argument, which would put a machine token in the process
// list. CredWrite takes a blob, so the secret is never anywhere but this
// process's memory and the credential store.
//
// Credentials written CRED_PERSIST_LOCAL_MACHINE belong to the signed-in user
// and are encrypted with their login. That is the same protection the rest of
// what a developer's machine holds gets.
type credentialManager struct{}

func platformBackend() Backend { return credentialManager{} }

func (credentialManager) Name() string { return "Windows Credential Manager" }

// The store is part of Windows, so the only question is whether the calls are
// there, which they are on everything john runs on.
func (credentialManager) Available() bool { return realStoreWanted() && advapi32.Load() == nil }

var (
	advapi32     = windows.NewLazySystemDLL("advapi32.dll")
	procCredRead = advapi32.NewProc("CredReadW")
	procCredWrit = advapi32.NewProc("CredWriteW")
	procCredDel  = advapi32.NewProc("CredDeleteW")
	procCredFree = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	errNotFound             = syscall.Errno(1168) // ERROR_NOT_FOUND
)

// credential is CREDENTIALW. Only the fields john sets or reads are named
// meaningfully; the rest keep the layout right, which is the whole job.
type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// target is the name the credential is filed under. The prefix keeps john's
// items together in a store shared with everything else on the machine.
func target(name string) (*uint16, error) {
	return windows.UTF16PtrFromString("john:" + name)
}

func (credentialManager) Get(name string) (string, error) {
	t, err := target(name)
	if err != nil {
		return "", err
	}
	var cred *credential
	r, _, callErr := procCredRead.Call(uintptr(unsafe.Pointer(t)), credTypeGeneric, 0,
		uintptr(unsafe.Pointer(&cred)))
	if r == 0 {
		if errors.Is(callErr, errNotFound) {
			return "", ErrNotFound
		}
		return "", callErr
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	if cred.CredentialBlobSize == 0 {
		return "", nil
	}
	blob := unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize)
	return string(append([]byte(nil), blob...)), nil
}

func (credentialManager) Set(name, secret string) error {
	t, err := target(name)
	if err != nil {
		return err
	}
	user, err := windows.UTF16PtrFromString("john")
	if err != nil {
		return err
	}
	blob := []byte(secret)
	cred := credential{
		Type:               credTypeGeneric,
		TargetName:         t,
		Persist:            credPersistLocalMachine,
		UserName:           user,
		CredentialBlobSize: uint32(len(blob)),
	}
	if len(blob) > 0 {
		cred.CredentialBlob = &blob[0]
	}
	r, _, callErr := procCredWrit.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if r == 0 {
		return callErr
	}
	// The blob is read by the call above, so it must outlive it.
	runtime.KeepAlive(blob)
	return nil
}

func (credentialManager) Delete(name string) error {
	t, err := target(name)
	if err != nil {
		return err
	}
	r, _, callErr := procCredDel.Call(uintptr(unsafe.Pointer(t)), credTypeGeneric, 0)
	if r == 0 {
		if errors.Is(callErr, errNotFound) {
			return ErrNotFound
		}
		return callErr
	}
	return nil
}

// missingBackendAdvice: the credential store is part of Windows, so this is
// only reached when advapi32 itself will not load.
func missingBackendAdvice() string {
	return "advapi32.dll would not load"
}
