package sandbox

import (
	"time"

	"golang.org/x/sys/unix"
)

// setClock sets the guest's wall clock. The agent is the machine's init and
// runs as root, which is all CAP_SYS_TIME asks for.
func setClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}
