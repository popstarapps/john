package sandbox

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: the one administrative step, and how it is taken.
//
// Creating a compute system is refused to an ordinary token — see Available in
// machine_hyperv.go — and there are exactly two ways to have one that is not
// refused. Run john elevated, every time, forever: which would put an agent
// that executes code it was sent by a model in a process with administrator
// rights, and is the wrong answer to a question about isolation. Or be a
// member of Hyper-V Administrators, which is the group Windows provides for
// precisely this and which UAC does not filter out — so an ordinary,
// unelevated john has the privilege and nothing else does.
//
// So the step is the second one, it is taken once, and the only thing that
// ever runs elevated is `john __sandbox-admin`: a few lines that add one SID
// to one group and exit.

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskFlagNoUI       = 0x00000400
	swHide                = 0
	// The developer said no at the consent prompt.
	errorCancelled = 1223
	// The SID is already a member of the group, which is not a failure: the
	// step has been taken, and what is left is the part it cannot do.
	errorMemberInAlias = 1378
)

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteEx = shell32.NewProc("ShellExecuteExW")

	netapi32                   = windows.NewLazySystemDLL("netapi32.dll")
	procNetLocalGroupAddMember = netapi32.NewProc("NetLocalGroupAddMembers")
	procNetLocalGroupGetMember = netapi32.NewProc("NetLocalGroupGetMembers")
)

// shellExecuteInfo is SHELLEXECUTEINFOW. Every field is here because the
// structure is sized by cbSize and a short one is rejected.
type shellExecuteInfo struct {
	cbSize        uint32
	fMask         uint32
	hwnd          windows.Handle
	verb          *uint16
	file          *uint16
	parameters    *uint16
	directory     *uint16
	show          int32
	instApp       windows.Handle
	idList        uintptr
	class         *uint16
	keyClass      windows.Handle
	hotKey        uint32
	icon          windows.Handle
	processHandle windows.Handle
}

// localGroupMembers0 is LOCALGROUP_MEMBERS_INFO_0: a member named by SID
// rather than by name, which is the whole reason this is not `net localgroup`.
type localGroupMembers0 struct {
	sid *windows.SID
}

// adminSetupFor answers only for the refusal it can actually fix.
//
// Available() reports several things — the feature is off, the firmware has
// virtualization disabled, this Windows has no compute service — and none of
// those is a group membership. Offering to elevate for one of them would be
// asking for administrator rights to do nothing.
func adminSetupFor(err error) *AdminSetup {
	if !errors.Is(err, errNeedsHyperVAdmin) {
		return nil
	}
	return &AdminSetup{
		Reason:   "Windows will not let this process create a virtual machine",
		Question: "Let john ask Windows for permission now?",
		Detail: []string{
			"Windows will show its consent prompt, once.",
			"john adds you to the Hyper-V Administrators group — the one permission it is",
			"missing, and the group Windows provides for it. Nothing stays running with",
			"administrator rights: john itself does not need them and does not take them.",
			"It takes effect the next time you sign in to Windows.",
		},
		Done: "Done: you are in the Hyper-V Administrators group. Windows applies a new group\n" +
			"when you sign in, so this is the one step left:\n\n" +
			"  Sign out of Windows and back in (or restart), then run john again.",
		Take: elevateForAdminSetup,
	}
}

// errSignInForHyperVAdmin is the refusal a developer meets between taking the
// step and signing in again: the group has them, this process's token does
// not, and a token's groups are fixed when the logon session is made. Nothing
// is left to grant, so it is not offered again — it says the one thing to do.
var errSignInForHyperVAdmin = errors.New("you are in the Hyper-V Administrators group, but Windows " +
	"applies a new group when you sign in, and you have not signed in since joining it.\n" +
	"  Nothing else is needed: sign out of Windows and back in (or restart), then run john again")

// inHyperVAdmins reports whether this user is a direct member of Hyper-V
// Administrators on this computer, as the computer has it now rather than as
// this process's token has it. Membership through another group reads as not
// a member, which only means the step is offered and finds nothing to add.
func inHyperVAdmins() bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	group, err := windows.CreateWellKnownSid(windows.WinBuiltinHyperVAdminsSid)
	if err != nil {
		return false
	}
	name, err := groupName(group)
	if err != nil {
		return false
	}
	wname, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	var buf *byte
	var read, total uint32
	status, _, _ := procNetLocalGroupGetMember.Call(
		0, // this computer
		uintptr(unsafe.Pointer(wname)),
		0, // LOCALGROUP_MEMBERS_INFO_0
		uintptr(unsafe.Pointer(&buf)),
		0xFFFFFFFF, // MAX_PREFERRED_LENGTH
		uintptr(unsafe.Pointer(&read)),
		uintptr(unsafe.Pointer(&total)),
		0,
	)
	if buf != nil {
		defer windows.NetApiBufferFree(buf)
	}
	if status != 0 || buf == nil {
		return false
	}
	for _, m := range unsafe.Slice((*localGroupMembers0)(unsafe.Pointer(buf)), read) {
		if m.sid != nil && m.sid.Equals(user.User.Sid) {
			return true
		}
	}
	return false
}

// errNeedsHyperVAdmin marks the one Available() failure that a person sitting
// at this terminal can fix in about ten seconds.
var errNeedsHyperVAdmin = errors.New("creating a virtual machine needs an administrator, once")

// elevateForAdminSetup runs this same binary again, elevated, to do the one
// thing that needs it.
//
// Re-running john rather than shelling out to `net localgroup` is what makes
// this safe to offer: what runs with administrator rights is code in this
// repository, chosen here, doing one named thing — not a command line built
// out of a group name and a user name, on a machine where both are whatever
// the operating system's language made them.
func elevateForAdminSetup() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	args, err := windows.UTF16PtrFromString("__sandbox-admin")
	if err != nil {
		return err
	}

	info := shellExecuteInfo{
		fMask:      seeMaskNoCloseProcess | seeMaskFlagNoUI,
		verb:       verb,
		file:       file,
		parameters: args,
		show:       swHide,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if r, _, e := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errno, ok := e.(windows.Errno); ok && errno == errorCancelled {
			return ErrAdminDeclined
		}
		return fmt.Errorf("sandbox: asking Windows for administrator rights: %w", e)
	}
	if info.processHandle == 0 {
		return errors.New("sandbox: Windows started nothing for the elevated step")
	}
	defer windows.CloseHandle(info.processHandle)

	if _, err := windows.WaitForSingleObject(info.processHandle, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.processHandle, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("sandbox: the elevated step failed (exit %d)", code)
	}
	return nil
}

// GrantAdminAccess is what runs elevated: it puts this user in the group that
// may create virtual machines, and does nothing else at all.
//
// Both halves are named by SID rather than by name. The group's name is
// whatever this Windows is localized to — "Hyper-V-Administratoren" on a
// German machine — so `net localgroup "Hyper-V Administrators"` is a command
// that works in one language, and a well-known SID is the same everywhere.
func GrantAdminAccess() error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("sandbox: reading this user's identity: %w", err)
	}
	group, err := windows.CreateWellKnownSid(windows.WinBuiltinHyperVAdminsSid)
	if err != nil {
		return fmt.Errorf("sandbox: the Hyper-V Administrators group: %w", err)
	}
	name, err := groupName(group)
	if err != nil {
		return err
	}
	wname, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}

	member := localGroupMembers0{sid: user.User.Sid}
	status, _, _ := procNetLocalGroupAddMember.Call(
		0, // this computer
		uintptr(unsafe.Pointer(wname)),
		0, // LOCALGROUP_MEMBERS_INFO_0
		uintptr(unsafe.Pointer(&member)),
		1,
	)
	switch uint32(status) {
	case 0, errorMemberInAlias:
		// Already a member is the same outcome, and it is the state a
		// developer is in when they took this step and have not signed in
		// again yet.
		return nil
	default:
		return fmt.Errorf("sandbox: adding this user to %s: %w", name, windows.Errno(status))
	}
}

// groupName is what this Windows calls a group, given its SID.
func groupName(sid *windows.SID) (string, error) {
	var nameLen, domainLen uint32
	var use uint32
	// The first call is asking how much room the answer needs; it always
	// fails, with the lengths filled in.
	windows.LookupAccountSid(nil, sid, nil, &nameLen, nil, &domainLen, &use)
	if nameLen == 0 {
		return "", errors.New("sandbox: this Windows has no Hyper-V Administrators group")
	}
	name := make([]uint16, nameLen)
	domain := make([]uint16, max(domainLen, 1))
	if err := windows.LookupAccountSid(nil, sid, &name[0], &nameLen, &domain[0], &domainLen, &use); err != nil {
		return "", fmt.Errorf("sandbox: naming the Hyper-V Administrators group: %w", err)
	}
	return windows.UTF16ToString(name), nil
}
