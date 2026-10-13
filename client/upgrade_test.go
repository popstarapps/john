package client

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// upgradedBackend is a backend that was upgraded under a running session: it
// answers every handshake, open or resume, with version.mismatch.
func upgradedBackend(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var handshakes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		tr := protocol.NewWSTransport(c)
		defer tr.Close()
		if _, err := tr.ReadFrame(r.Context()); err != nil {
			return
		}
		handshakes.Add(1)
		data, _ := protocol.EncodeRaw(&protocol.VersionMismatch{
			Required: "0.5.2", Client: "0.4.1", Download: "/releases/john-darwin-arm64",
		})
		tr.WriteFrame(r.Context(), data)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &handshakes
}

// A session whose backend was upgraded underneath it stops and asks about the
// update, rather than retrying a refusal that will never change behind a
// status line that says the backend is unreachable.
func TestASessionStopsToUpdateWhenTheBackendIsUpgradedUnderIt(t *testing.T) {
	for _, path := range []string{"reopen", "resume"} {
		t.Run(path, func(t *testing.T) {
			url, handshakes := upgradedBackend(t)
			a := newApp(terminalui.New(terminalui.Options{Mode: terminalui.ModePlain}),
				sandbox.NewFake(t.TempDir()), Options{BackendURL: url, Token: "t", Version: "0.4.1"})

			done := make(chan struct{})
			go func() {
				defer close(done)
				if path == "reopen" {
					a.wire.reopenFresh()
					return
				}
				c := a.wire.newConn()
				a.wire.swap(c, "s_before_the_upgrade")
				a.wire.reconnectLoop(c)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second * timeScale):
				t.Fatalf("still retrying after %d refusals", handshakes.Load())
			}

			u := a.pendingUpdate()
			if u == nil || u.Required != "0.5.2" || u.Current != "0.4.1" {
				t.Fatalf("pending update = %+v, want 0.4.1 -> 0.5.2", u)
			}
			if n := handshakes.Load(); n != 1 {
				t.Fatalf("%d handshakes, want the one refusal and no retries", n)
			}
		})
	}
}

// stopRecorder is a sandbox that remembers being stopped, the way a virtual
// machine releases its lock when it is.
type stopRecorder struct {
	*sandbox.Fake
	stopped atomic.Bool
}

func (s *stopRecorder) Stop(context.Context) error {
	s.stopped.Store(true)
	return nil
}

// The update is asked about only once the session has let go of its
// sandbox. On Windows the new john runs as a child of the old one, and found
// the sandbox still held by its own parent: "in use by another john".
func TestTheSandboxIsStoppedBeforeTheUpdateIsAskedAbout(t *testing.T) {
	url, _ := upgradedBackend(t)
	sb := &stopRecorder{Fake: sandbox.NewFake(t.TempDir())}
	var out bytes.Buffer
	opts := Options{BackendURL: url, Token: "t", Version: "0.4.1", Sandbox: sb,
		UI: &terminalui.Options{Mode: terminalui.ModePlain, Output: &out, Input: strings.NewReader("")}}

	type result struct {
		code int
		u    *update
	}
	done := make(chan result, 1)
	go func() {
		code, u := session(opts)
		done <- result{code, u}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(20 * time.Second * timeScale):
		t.Fatalf("the session did not end on the refusal:\n%s", out.String())
	}
	if r.u == nil || r.u.Required != "0.5.2" {
		t.Fatalf("session returned update %+v, want 0.5.2 to ask about", r.u)
	}
	if !sb.stopped.Load() {
		t.Fatal("the update was handed back with the sandbox still running")
	}
}
