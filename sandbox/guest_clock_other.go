//go:build !linux

package sandbox

import "time"

// setClock is the guest's clock being set, where the guest is a Linux kernel.
// Off Linux the agent is the loopback machine's, a process on the developer's
// own computer, and that clock is already the host's.
func setClock(time.Time) error { return nil }
