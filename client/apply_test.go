//go:build !windows

package client

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
	"github.com/popstarapps/john/vt"
)

// TestApplyFlow drives the standalone `john apply` review over a real pty: a
// fake sandbox holds an unapplied edit, the developer reviews it, applies it,
// and the host file changes — with the review being the only path to the host.
func TestApplyFlow(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	pty.Setsize(tty, &pty.Winsize{Rows: 40, Cols: 110})

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	sb := sandbox.NewFake(dir)
	ctx := context.Background()
	sb.Build(ctx, nil)
	sb.Write(ctx, "main.go", []byte("package main // edited\n"))

	diffs, _ := sb.Diff(ctx)
	ui := terminalui.New(terminalui.Options{Input: tty, Output: tty, Mode: terminalui.ModeInteractive})

	term := vt.New(110)
	var mu sync.Mutex
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				term.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	screen := func() string { mu.Lock(); defer mu.Unlock(); return term.String() }
	wait := func(s string) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if contains(screen(), s) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %q:\n%s", s, screen())
	}
	typ := func(k string) { ptmx.WriteString(k); time.Sleep(60 * time.Millisecond) }

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go runApplyFlow(runCtx, ui, sb, dir, diffs)
	go func() { done <- ui.Run(runCtx) }()
	t.Cleanup(func() { ui.Quit(); cancel(); ptmx.Close() })

	wait("main.go")
	typ("a") // apply selected
	wait("APPLY TO WORKING DIRECTORY")
	typ("y") // confirm
	wait("1 file →")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}

	host, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(host) != "package main // edited\n" {
		t.Fatalf("apply did not reach the host: %q", host)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
