//go:build windows

package client

import (
	"os"
	"os/exec"
	"path/filepath"
)

// replaceBinary installs the downloaded build. Windows will not let a running
// executable be overwritten, but it will let it be renamed — so the running
// binary is moved aside first and the new one takes its name. The displaced
// file cannot be deleted while it is still running; it is removed on the next
// run instead, which is what the sweep below is for.
func replaceBinary(exe, downloaded string) error {
	old := exe + ".old"
	os.Remove(old) // a leftover from a previous update, no longer running
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(downloaded, exe); err != nil {
		os.Rename(old, exe) // put it back rather than leave nothing runnable
		return err
	}
	return nil
}

// relaunch starts the new binary and waits for it, passing its exit code up.
// Windows has no execve, so this process stays alive as a thin parent rather
// than being replaced; sharing the console means the developer sees one
// continuous session.
func relaunch(exe string, args []string) (int, error) {
	cmd := exec.Command(exe, args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// sweepOldBinary clears the displaced binary an earlier update left behind.
// It is best effort: right after an update the displaced build is the parent
// still waiting in relaunch, so it cannot go until the run after that — and
// replaceBinary clears it anyway before it needs the name.
func sweepOldBinary(exe string) {
	os.Remove(filepath.Clean(exe) + ".old")
}

// sweepAfterUpdate is what makes "removed on the next run" true: called as
// john starts, it clears what the last update displaced.
func sweepAfterUpdate() {
	if exe, err := os.Executable(); err == nil {
		sweepOldBinary(exe)
	}
}
