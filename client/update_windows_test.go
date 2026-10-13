//go:build windows

package client

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Windows half of the update, which is a different mechanism rather than
// a different spelling of the Unix one: no execve, and a running .exe cannot
// be overwritten. These run the real thing — a copy of this test binary as
// "john.exe", actually running while it is replaced — because an update that
// only works on a file nobody is executing is not an update.

// copyOfThisBinary puts a runnable copy of the test binary at path.
func copyOfThisBinary(t *testing.T, path string) {
	t.Helper()
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// updateChild is what the copies do when started: print a marker, then
// either exit with a code or hang around being a running image.
func updateChild(t *testing.T) bool {
	switch os.Getenv("JOHN_UPDATE_CHILD") {
	case "":
		return false
	case "linger":
		os.Stdout.WriteString("lingering\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	case "new":
		os.Stdout.WriteString("i-am-the-new-build " + strings.Join(os.Args[1:], " ") + "\n")
		os.Exit(7)
	case "relaunch":
		// relaunch hands on this environment, so this is how the build it
		// starts knows to be the new one.
		os.Setenv("JOHN_UPDATE_CHILD", "new")
		code, err := relaunch(os.Getenv("JOHN_UPDATE_TARGET"), os.Args)
		if err != nil {
			os.Stdout.WriteString("relaunch: " + err.Error() + "\n")
			os.Exit(99)
		}
		os.Exit(code)
	}
	t.Fatalf("unknown child mode %q", os.Getenv("JOHN_UPDATE_CHILD"))
	return true
}

func TestReplacingARunningBinaryInstallsTheNewBuild(t *testing.T) {
	if updateChild(t) {
		return
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "john.exe")
	copyOfThisBinary(t, exe)

	// john.exe is running, the way it is when it updates itself.
	running := exec.Command(exe, "-test.run=^TestReplacingARunningBinaryInstallsTheNewBuild$")
	running.Env = append(os.Environ(), "JOHN_UPDATE_CHILD=linger")
	out, err := running.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := running.Start(); err != nil {
		t.Fatalf("starting a copy of the test binary: %v", err)
	}
	t.Cleanup(func() { running.Process.Kill(); running.Wait() })
	buf := make([]byte, 64)
	if n, _ := out.Read(buf); !strings.Contains(string(buf[:n]), "lingering") {
		t.Fatalf("the running copy said %q", buf[:n])
	}

	// Which is precisely what makes the naive approach fail. If this ever
	// stops being true, the rename dance below is no longer needed.
	if err := os.WriteFile(exe, []byte("x"), 0o755); err == nil {
		t.Fatal("Windows let a running executable be overwritten")
	}

	downloaded := filepath.Join(dir, ".john-update-xyz")
	if err := os.WriteFile(downloaded, []byte("the new build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(exe, downloaded); err != nil {
		t.Fatalf("replaceBinary with john.exe running: %v", err)
	}

	if got, err := os.ReadFile(exe); err != nil || string(got) != "the new build" {
		t.Fatalf("john.exe is now %q, %v", got, err)
	}
	if _, err := os.Stat(downloaded); !os.IsNotExist(err) {
		t.Fatalf("the download is still beside it: %v", err)
	}
	// The displaced build is still running and so still on disk.
	if _, err := os.Stat(exe + ".old"); err != nil {
		t.Fatalf("the running build was not moved aside: %v", err)
	}
	// Until it stops, it cannot be swept — and trying is not an error.
	sweepOldBinary(exe)
	if _, err := os.Stat(exe + ".old"); err != nil {
		t.Fatalf("the sweep removed a running binary?: %v", err)
	}
	running.Process.Kill()
	running.Wait()
	sweepOldBinary(exe)
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatalf("the sweep left the stopped build behind: %v", err)
	}
}

// A second update finds the first one's leftover in the way. It must not
// turn into a failed update.
func TestReplacingOverALeftoverFromAnEarlierUpdate(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "john.exe")
	for name, content := range map[string]string{
		"john.exe":         "the current build",
		"john.exe.old":     "the one before",
		".john-update-xyz": "the new build",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := replaceBinary(exe, filepath.Join(dir, ".john-update-xyz")); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "the new build" {
		t.Fatalf("john.exe is now %q", got)
	}
	if got, _ := os.ReadFile(exe + ".old"); string(got) != "the current build" {
		t.Fatalf("john.exe.old is now %q", got)
	}
}

// If the new build cannot be put in place, the old one must be put back:
// nothing runnable at the path is the one outcome worse than no update.
func TestAFailedReplaceLeavesTheOldBuildWhereItWas(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "john.exe")
	if err := os.WriteFile(exe, []byte("the current build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBinary(exe, filepath.Join(dir, "never-downloaded")); err == nil {
		t.Fatal("replacing with a file that does not exist reported success")
	}
	if got, err := os.ReadFile(exe); err != nil || string(got) != "the current build" {
		t.Fatalf("john.exe is now %q, %v", got, err)
	}
}

// relaunch cannot replace the process, so it has to be the next best thing:
// the new build runs with the same arguments on the same console, and its
// exit code is this process's exit code.
func TestRelaunchRunsTheNewBuildAndPassesItsExitCodeUp(t *testing.T) {
	if updateChild(t) {
		return
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "old-john.exe")
	target := filepath.Join(dir, "john.exe")
	copyOfThisBinary(t, parent)
	copyOfThisBinary(t, target)

	cmd := exec.Command(parent, "-test.run=^TestRelaunchRunsTheNewBuildAndPassesItsExitCodeUp$", "an-argument")
	cmd.Env = append(os.Environ(), "JOHN_UPDATE_CHILD=relaunch", "JOHN_UPDATE_TARGET="+target)
	out, err := cmd.CombinedOutput()
	got := string(out)
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 {
		t.Fatalf("exit = %v, want the new build's 7\n%s", err, got)
	}
	if !strings.Contains(got, "i-am-the-new-build") || !strings.Contains(got, "an-argument") {
		t.Fatalf("the new build did not run with the same arguments:\n%s", got)
	}
}

// An exec that cannot happen comes back as an error, as on Unix.
func TestRelaunchReportsABuildThatCannotStart(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-a-binary.exe")
	if _, err := relaunch(missing, []string{missing}); err == nil {
		t.Fatal("relaunching something that does not exist reported success")
	}
}
