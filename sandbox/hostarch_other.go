//go:build !linux && !darwin

package sandbox

import (
	"os"
	"runtime"
	"strings"
)

// hostArch is the processor this machine actually has.
//
// Windows sets PROCESSOR_ARCHITEW6432 for a process running under emulation,
// leaving PROCESSOR_ARCHITECTURE describing what the process thinks it is —
// so the first of the two that is set is the machine's own answer.
func hostArch() (string, error) {
	for _, key := range []string{"PROCESSOR_ARCHITEW6432", "PROCESSOR_ARCHITECTURE"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return normalizeArch(v), nil
		}
	}
	return runtime.GOARCH, nil
}
