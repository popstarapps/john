//go:build !windows

package sandbox

import "errors"

// Nowhere but Windows is a sandbox one administrative step away from working.
// macOS needs an entitlement, which is a property of the build rather than of
// the machine; Linux needs /dev/kvm and group membership that a developer's
// distribution has usually already arranged, and the reasons it can fail are
// varied enough that john would be guessing at which one it is.
func adminSetupFor(error) *AdminSetup { return nil }

// GrantAdminAccess is the elevated half of an AdminSetup, and there is none
// here. It exists on every platform so that `john __sandbox-admin` is one
// command with one behaviour rather than a build tag in main.
func GrantAdminAccess() error {
	return errors.New("sandbox: there is nothing for an administrator to grant on this platform")
}
