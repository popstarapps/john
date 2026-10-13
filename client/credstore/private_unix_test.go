//go:build !windows

package credstore

import (
	"os"
	"testing"
)

// onlyThisUserCanRead: on Unix that is the mode bits, and 0600 exactly.
func onlyThisUserCanRead(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s is mode %v, want 0600", path, info.Mode().Perm())
	}
}
