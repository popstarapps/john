//go:build !windows

package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// /rebuild builds from the working directory's .john/, so what the sandbox
// changed there and nobody applied is shown first — and only that: the rest
// of the sandbox's work is not what a rebuild is about.
func TestARebuildOffersTheUnappliedDockerfileFirst(t *testing.T) {
	s := newE2E(t)
	ctx := context.Background()
	dockerfile := "FROM debian:stable-slim\nRUN apt-get update && apt-get install -y nodejs npm\n"
	if err := s.sb.Write(ctx, ".john/Dockerfile", []byte(dockerfile)); err != nil {
		t.Fatal(err)
	}
	if err := s.sb.Write(ctx, "src/App.jsx", []byte("export default function App() {}\n")); err != nil {
		t.Fatal(err)
	}

	left := make(chan bool, 1)
	go func() { left <- s.app.applyEnvironmentFirst(ctx) }()
	s.wait("before rebuilding")
	s.wait(".john/Dockerfile")
	if strings.Contains(s.screen(), "App.jsx") {
		t.Fatalf("the review before a rebuild offered work that is not the environment:\n%s", s.screen())
	}
	s.type_("a")
	s.wait("APPLY TO WORKING DIRECTORY")
	s.type_("y")

	select {
	case leftOut := <-left:
		if leftOut {
			t.Fatal("the Dockerfile was applied, and still reported as left out")
		}
	case <-time.After(10 * time.Second * timeScale):
		t.Fatalf("the review did not finish:\n%s", s.screen())
	}
	host, err := os.ReadFile(filepath.Join(s.workdir, ".john", "Dockerfile"))
	if err != nil || string(host) != dockerfile {
		t.Fatalf("the working directory's Dockerfile = %q, %v", host, err)
	}
	if _, err := os.Stat(filepath.Join(s.workdir, "src", "App.jsx")); err == nil {
		t.Fatal("the rebuild's review applied work that was not the environment")
	}

	// With nothing left under .john/, there is nothing to ask.
	if s.app.applyEnvironmentFirst(ctx) {
		t.Fatal("nothing was pending, and something was reported left out")
	}
}

// Leaving the change in the sandbox is allowed, and the rebuild is told so.
func TestARebuildCanLeaveTheDockerfileInTheSandbox(t *testing.T) {
	s := newE2E(t)
	ctx := context.Background()
	if err := s.sb.Write(ctx, ".john/Dockerfile", []byte("FROM debian:stable-slim\n")); err != nil {
		t.Fatal(err)
	}
	left := make(chan bool, 1)
	go func() { left <- s.app.applyEnvironmentFirst(ctx) }()
	s.wait("before rebuilding")
	s.type_("q")
	select {
	case leftOut := <-left:
		if !leftOut {
			t.Fatal("the Dockerfile was left in the sandbox, and the rebuild was not told")
		}
	case <-time.After(10 * time.Second * timeScale):
		t.Fatalf("the review did not finish:\n%s", s.screen())
	}
}
