//go:build windows

package credstore

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// onlyThisUserCanRead: on Windows the mode bits mean nothing — os.WriteFile's
// 0600 sets at most the read-only attribute — and what keeps another user out
// is the file's DACL, inherited from the directory it is written into. So the
// assertion is on that: no allow entry grants read to one of the groups every
// account on the machine is in. The owner, SYSTEM and Administrators are
// expected; an administrator can read anything on the machine regardless.
func onlyThisUserCanRead(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil {
		t.Fatalf("%s has a null DACL, which grants everyone everything", path)
	}

	var everyone []*windows.SID
	for _, s := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinWorldSid,             // Everyone
		windows.WinAuthenticatedUserSid, // Authenticated Users
		windows.WinBuiltinUsersSid,      // Users
		windows.WinAnonymousSid,         // Anonymous
		windows.WinBuiltinGuestsSid,     // Guests
		windows.WinInteractiveSid,       // Interactive
	} {
		sid, err := windows.CreateWellKnownSid(s)
		if err != nil {
			t.Fatal(err)
		}
		everyone = append(everyone, sid)
	}

	const read = windows.FILE_READ_DATA | windows.GENERIC_READ | windows.GENERIC_ALL
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Mask&read == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		for _, e := range everyone {
			if sid.Equals(e) {
				t.Fatalf("%s can be read by %s (mask %#x)", path, sid, ace.Mask)
			}
		}
	}
}
