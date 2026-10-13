package client

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// An edit to .john/Dockerfile under a running sandbox is said once, marked on
// the status line until it is dealt with, and taken back when the file is
// changed back. A definition that would not build is reported instead, since
// /rebuild could not do anything with it.
func TestAnEditedEnvironmentIsSaidOnceAndClearedWhenUndone(t *testing.T) {
	workdir := t.TempDir()
	file := filepath.Join(workdir, filepath.FromSlash(sandbox.EnvFile))
	write := func(s string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		_, id, _, err := sandbox.Environment(workdir)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	var out bytes.Buffer
	ui := terminalui.New(terminalui.Options{Mode: terminalui.ModePlain, Output: &out, Input: strings.NewReader("")})
	a := newApp(ui, sandbox.NewFake(workdir), Options{BackendURL: "http://127.0.0.1:1", Token: "t", Workdir: workdir})
	done := make(chan struct{})
	go func() { ui.Run(context.Background()); close(done) }()

	built := write("FROM debian:stable-slim\n")
	edited := write("FROM debian:stable-slim\nRUN apt-get install -y git\n")
	a.environmentIs(edited, built)
	a.environmentIs(edited, built) // the next look at the same change
	if !a.envChanged {
		t.Fatal("an edit was not marked")
	}
	a.environmentIs(write("FROM debian:stable-slim\n"), built)
	if a.envChanged {
		t.Fatal("an edit that was undone stayed marked")
	}
	a.environmentIs(write("FROM debian:stable-slim\nENTRYPOINT [\"sh\"]\n"), built)
	if a.envChanged {
		t.Fatal("a definition that would not build was offered for a rebuild")
	}

	ui.Quit()
	<-done
	got := out.String()
	if n := strings.Count(got, "/rebuild applies it now"); n != 1 {
		t.Fatalf("the change was said %d times:\n%s", n, got)
	}
	if !strings.Contains(got, "ENTRYPOINT") {
		t.Fatalf("the broken definition was not reported:\n%s", got)
	}
}

func TestAnApplyTouchesTheEnvironmentOnlyThroughJohnDir(t *testing.T) {
	for paths, want := range map[string]bool{
		".john/Dockerfile":           true,
		"src/main.go .john/setup.sh": true,
		"Dockerfile":                 false,
		"src/.john/Dockerfile":       false,
		".johnny/x":                  false,
	} {
		if got := touchesEnvironment(strings.Fields(paths)); got != want {
			t.Errorf("%q: %v, want %v", paths, got, want)
		}
	}
}
