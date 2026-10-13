//go:build !windows

package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/oliverkofoed/gokit/agentkit/llm"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"filippo.io/age"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/backend"
	"github.com/popstarapps/john/proxy"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/terminalui"
	"github.com/popstarapps/john/vt"
)

// These are end-to-end transcripts: a real terminal (pty), the real client,
// a real WebSocket, a real backend running the real agentkit loop — with only
// the model itself scripted (llmtest) and the sandbox in-memory. What the
// developer sees is asserted the way they see it: by reading the screen.

type e2eSession struct {
	t       *testing.T
	srv     *backend.Server
	fake    *llmtest.Fake
	workdir string
	// traceDir is the org's trace store, and orgKey the identity an
	// administrator would hold outside the backend. Every session here runs
	// encrypted, because every real one does.
	traceDir string
	orgKey   *age.X25519Identity
	sb       *sandbox.Fake
	app      *app
	term     *vt.Terminal
	ptmx     *os.File
	done     chan error
	mu       sync.Mutex

	// What it takes to start this backend again on the same address and the
	// same database, which is what a restart is.
	dbPath    string
	addr      string
	srvCancel context.CancelFunc
	srvDone   chan struct{}
	token     string
}

const testModelName = "internal/codegen-l"

func newE2E(t *testing.T, replies ...llmtest.Reply) *e2eSession {
	t.Helper()
	return newE2EWith(t, nil, replies...)
}

// newE2EWith is newE2E with a chance to write policy into the backend before
// the client starts, because a client pulls its snapshot on the way up: a
// policy written afterwards would be a different test.
func newE2EWith(t *testing.T, setup func(*store.Store), replies ...llmtest.Reply) *e2eSession {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 40, Cols: 110}); err != nil {
		t.Fatalf("setsize: %v", err)
	}

	// The developer's own trace key lands here, not in a real home directory.
	t.Setenv("JOHN_HOME", t.TempDir())
	orgKey, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	traceDir := filepath.Join(t.TempDir(), "traces")

	fake := llmtest.New(replies...)
	dbPath := filepath.Join(t.TempDir(), "john.db")
	srv, err := backend.New(backend.Config{
		DBPath:   dbPath,
		Listen:   "127.0.0.1:0",
		Version:  "test",
		DevIdP:   &devidp.Identity{Subject: "dev-1", Email: "oliver@example.com", Name: "Oliver K"},
		Streamer: fake,
	})
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	srvCtx, srvCancel := context.WithCancel(context.Background())
	srvDone := make(chan struct{})
	go func() { defer close(srvDone); srv.Serve(srvCtx) }()

	st := srv.Store()
	// A real org gets its snapshot signing key from setup. Without one here the
	// client cannot pin a key, so it would hold no policy snapshot and the test
	// would quietly exercise the deny-all path instead of the real one.
	_, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveOrg(&store.Org{
		Name: "Test Org", OIDCIssuer: "http://" + srv.Addr() + "/devidp",
		OIDCClientID: "john", AdminEmail: "oliver@example.com", SetupComplete: true,
		SigningKey: base64.StdEncoding.EncodeToString(signing),
		TraceKey:   orgKey.Recipient().String(), TracePath: traceDir,
	}); err != nil {
		t.Fatal(err)
	}
	lm, _ := json.Marshal(llm.Model{ID: "codegen-l", Name: testModelName,
		API: llm.OpenAIChat, Provider: "selfhosted", BaseURL: "https://llm.internal/v1",
		Cost: llm.Cost{Input: 1, Output: 2}})
	if err := st.CreateModel(&store.Model{
		ID: "m_test", Name: testModelName, Provider: "selfhosted", Hosting: "internal",
		ModelID: "codegen-l", LLMModel: string(lm),
	}); err != nil {
		t.Fatal(err)
	}
	user, err := st.UpsertSSOUser("dev-1", "oliver@example.com", "Oliver K", "oliver@example.com")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.CreateMachineToken(user.ID, "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(st)
	}

	workdir := t.TempDir()
	writeHost := func(p, content string) {
		full := filepath.Join(workdir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	writeHost("billing/client.go", "package billing\n")
	writeHost("billing/client_test.go", "package billing // tests\n")
	sb := sandbox.NewFake(workdir)

	ui := terminalui.New(terminalui.Options{
		Input: tty, Output: tty, Mode: terminalui.ModeInteractive,
		Placeholder:    "ask john to do something",
		FileCompletion: true,
		Commands: []terminalui.Command{
			{Name: "apply"}, {Name: "reset"}, {Name: "policies"}, {Name: "resume"},
			{Name: "help"}, {Name: "model"}, {Name: "resources"}, {Name: "quit"},
		},
	})
	a := newApp(ui, sb, Options{
		BackendURL: "http://" + srv.Addr(),
		Token:      token,
		Email:      "oliver@example.com",
		Workdir:    workdir,
		Version:    "test",
	})

	s := &e2eSession{t: t, srv: srv, fake: fake, workdir: workdir, sb: sb,
		traceDir: traceDir, orgKey: orgKey,
		app: a, term: vt.New(110), ptmx: ptmx, done: make(chan error, 1),
		dbPath: dbPath, addr: srv.Addr(), srvCancel: srvCancel, srvDone: srvDone, token: token}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.term.Write(buf[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	a.boot()
	go a.loop(ctx)
	go a.start(ctx)
	go func() { s.done <- ui.Run(ctx) }()

	t.Cleanup(func() {
		ui.Quit()
		select {
		case <-s.done:
		case <-time.After(3 * time.Second * timeScale):
		}
		a.wire.close()
		cancel()
		ptmx.Close()
		s.stopBackend()
	})
	s.wait("ask john to do something")
	s.wait(testModelName) // the session is open: the backend named the model
	return s
}

// stopBackend takes the backend away without touching the database: a deploy,
// a crash, a machine rebooting.
func (s *e2eSession) stopBackend() {
	if s.srv == nil {
		return
	}
	s.srvCancel()
	<-s.srvDone
	s.srv.Close()
	s.srv = nil
}

// restartBackend brings it back on the same address and the same database,
// with a new script for the model. The client is not told and does not know:
// it finds out the way it would in life, by its wire dropping.
func (s *e2eSession) restartBackend(replies ...llmtest.Reply) {
	s.t.Helper()
	s.stopBackend()
	fake := llmtest.New(replies...)
	srv, err := backend.New(backend.Config{
		DBPath:   s.dbPath,
		Listen:   s.addr,
		Version:  "test",
		DevIdP:   &devidp.Identity{Subject: "dev-1", Email: "oliver@example.com", Name: "Oliver K"},
		Streamer: fake,
	})
	if err != nil {
		s.t.Fatalf("restart: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(ctx) }()
	s.srv, s.fake, s.srvCancel, s.srvDone = srv, fake, cancel, done
}

func (s *e2eSession) screen() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.String()
}

func (s *e2eSession) wait(text string) {
	s.t.Helper()
	deadline := time.Now().Add(8 * time.Second * timeScale)
	for time.Now().Before(deadline) {
		if strings.Contains(s.screen(), text) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for %q:\n%s", text, s.screen())
}

func (s *e2eSession) type_(keys string) {
	s.t.Helper()
	if _, err := s.ptmx.WriteString(keys); err != nil {
		s.t.Fatalf("write: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
}

func TestSessionBootsAgainstRealBackend(t *testing.T) {
	s := newE2E(t)
	s.wait("signed in as oliver@example.com")
	s.wait(testModelName) // the backend assigned it; the client only renders it
}

func TestSessionStreamsAReply(t *testing.T) {
	s := newE2E(t, llmtest.Text("The retry now honours Retry-After headers."))
	s.type_("fix the retry in billing\r")
	s.wait("The retry now honours Retry-After headers.")

	// The prompt actually crossed the wire into the loop.
	reqs := s.fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	found := false
	for _, m := range reqs[0].Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "fix the retry in billing") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the prompt never reached the model request")
	}
}

func TestSessionToolRoundTrip(t *testing.T) {
	s := newE2E(t,
		llmtest.ToolCall("run", map[string]any{"cmd": "go test ./billing/..."}),
		llmtest.Text("Tests pass."),
	)
	s.sb.ExecFn = func(cmd, cwd string) sandbox.ExecResult {
		return sandbox.ExecResult{ExitCode: 0, Stdout: []byte("ok  \tbilling\t0.01s\n")}
	}

	s.type_("run the billing tests\r")
	s.wait("go test ./billing/...") // the tool call rendered
	s.wait("Tests pass.")           // and the loop continued with its result

	// The command's output went back up the wire into the model's context.
	reqs := s.fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(reqs))
	}
	second := reqs[1]
	found := false
	for _, m := range second.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "exit 0") && strings.Contains(b.Text, "ok") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tool result never reached the second model call: %+v", second.Messages)
	}
}

func TestSessionEditReviewApply(t *testing.T) {
	s := newE2E(t,
		llmtest.ToolCall("edit", map[string]any{
			"path": "billing/client.go", "content": "package billing // edited\n"}),
		llmtest.Text("Edited the client."),
	)
	s.type_("edit the client\r")
	s.wait("Edited the client.")
	s.wait("unapplied")

	// The host has not moved: the edit lives in the sandbox.
	host, _ := os.ReadFile(filepath.Join(s.workdir, "billing/client.go"))
	if string(host) != "package billing\n" {
		t.Fatalf("host modified before apply: %q", host)
	}

	s.type_("/apply\r")
	s.wait("billing/client.go")
	s.type_("a")
	s.wait("APPLY TO WORKING DIRECTORY")
	s.type_("n")
	s.wait("not applied")
	host, _ = os.ReadFile(filepath.Join(s.workdir, "billing/client.go"))
	if string(host) != "package billing\n" {
		t.Fatal("declining the confirm must not write the host")
	}

	s.type_("/apply\r")
	s.wait("billing/client.go")
	s.type_("a")
	s.wait("APPLY TO WORKING DIRECTORY")
	s.type_("y")
	s.wait("1 file →")
	host, _ = os.ReadFile(filepath.Join(s.workdir, "billing/client.go"))
	if string(host) != "package billing // edited\n" {
		t.Fatalf("applied content = %q", host)
	}
}

func TestSessionInterrupt(t *testing.T) {
	blocker := make(chan struct{})
	s := newE2E(t, llmtest.Reply{
		Blocks:  []llm.Block{llm.TextBlock("working on it")},
		Blocker: blocker,
	})
	defer close(blocker)

	s.type_("do something slow\r")
	s.wait("working on it")
	s.type_("\x1b") // esc
	s.wait("interrupted")
}

func TestSessionFailsClosedWhenBackendDies(t *testing.T) {
	s := newE2E(t, llmtest.Text("hello"))
	s.type_("say hello\r")
	s.wait("hello")
	s.wait("ask john to do something") // the turn ended, so the next line is a prompt

	// The backend goes away: the status line alerts and prompts are refused,
	// because the loop that would answer them is not on this machine.
	s.srv.Close()
	s.wait("backend unreachable")
	s.type_("try anyway\r")
	s.wait("blocked: the agent loop runs in the backend")
	// Nothing new reached the model.
	if n := len(s.fake.Requests()); n != 1 {
		t.Fatalf("model calls after outage = %d, want 1", n)
	}
}

// TestToolTableHasExactlyThreeEntries pins the closed tool set: adding a
// fourth tool means deleting this assertion, which means explaining it in
// protocol/readme.md and the threat model first.
func TestToolTableHasExactlyThreeEntries(t *testing.T) {
	handled := map[string]bool{"read": true, "edit": true, "run": true}
	if len(handled) != 3 {
		t.Fatal("the tool set is closed at three")
	}
	// And an unknown tool is blocked, never guessed at.
	s := newE2E(t,
		llmtest.ToolCall("fetch_url", map[string]any{"url": "https://example.com"}),
		llmtest.Text("Understood, no fetching."),
	)
	s.type_("fetch something\r")
	s.wait("Understood, no fetching.")

	reqs := s.fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d", len(reqs))
	}
	blockedSeen := false
	for _, m := range reqs[1].Messages {
		if !m.IsError {
			continue
		}
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "unknown tool") {
				blockedSeen = true
			}
		}
	}
	if !blockedSeen {
		t.Fatal("the unknown tool must come back blocked, as an error the model can see")
	}
}

// The loop a developer waits on: a request is filed, an administrator approves
// it into a group, and the session that is already running is told, pulls the
// new policies, and its proxy allows the host — with no restart.
func TestAnApprovedRequestReachesTheRunningSession(t *testing.T) {
	s := newE2E(t)
	s.wait(testModelName)
	api := func(path string, body any, out any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", "http://"+s.srv.Addr()+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+s.app.wire.currentToken())
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST %s = %d: %s", path, resp.StatusCode, msg)
		}
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
	}

	spec, err := proxy.ProposedPolicy(proxy.Outcome{Method: "GET", URL: "http://deb.debian.org/debian/dists/stable/InRelease"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var filed struct{ ID string }
	api("/api/v1/access-requests", map[string]any{"title": "Access to deb.debian.org", "spec": spec}, &filed)
	api("/api/v1/access-requests/"+filed.ID+"/decision", map[string]any{
		"status": "approved", "newGroup": map[string]any{"name": "Debian mirrors", "baseline": true},
	}, nil)

	s.waitAllowed("http://deb.debian.org/debian/pool/main/c/curl/curl.deb")
}

// Ending a session stops its sandbox, whichever way it ends. For a virtual
// machine that is what puts the disk in order — the workspace unmounted and
// synced — before the power goes. john used to just exit, the machine died
// with it, and a file the agent wrote seconds before /quit came back empty
// the next time.
func TestEndingASessionStopsTheSandbox(t *testing.T) {
	backend := newE2E(t)

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer ptmx.Close()
	pty.Setsize(tty, &pty.Winsize{Rows: 40, Cols: 110})
	go io.Copy(io.Discard, ptmx)

	sb := sandbox.NewFake(t.TempDir())
	done := make(chan int, 1)
	go func() {
		done <- Session(Options{
			BackendURL: "http://" + backend.addr, Token: backend.token, Email: "oliver@example.com",
			Workdir: t.TempDir(), Version: "test", Sandbox: sb,
			UI: &terminalui.Options{Input: tty, Output: tty, Mode: terminalui.ModeInteractive,
				Commands: []terminalui.Command{{Name: "quit"}}},
		})
	}()
	time.Sleep(time.Second * timeScale)
	ptmx.WriteString("/quit\r")
	select {
	case <-done:
	case <-time.After(10 * time.Second * timeScale):
		t.Fatal("the session did not end on /quit")
	}
	if !sb.Stopped() {
		t.Fatal("the session ended without stopping its sandbox")
	}
}

// A failed command says why on screen, not only to the model: the first lines
// of its error output, less its stack frames, go under its line. "exit 1"
// alone was all a developer saw of a build that failed with "EIO: i/o error,
// rmdir 'dist/assets'".
func TestAFailedCommandShowsWhyOnScreen(t *testing.T) {
	s := newE2E(t,
		llmtest.ToolCall("run", map[string]any{"cmd": "npm run build"}),
		llmtest.Text("The build failed."),
	)
	s.sb.ExecFn = func(cmd, cwd string) sandbox.ExecResult {
		return sandbox.ExecResult{ExitCode: 1, Stdout: []byte("vite v5.4.21 building for production...\n"),
			Stderr: []byte("x Build failed in 7.07s\nerror during build:\nEIO: i/o error, rmdir 'dist/assets'\n    at rmdirSync (node:fs:1214:11)\n")}
	}
	s.type_("build it\r")
	s.wait("The build failed.")
	scr := s.screen()
	for _, want := range []string{"exit 1", "error during build:", "EIO: i/o error, rmdir 'dist/assets'"} {
		if !strings.Contains(scr, want) {
			t.Fatalf("the screen does not show %q:\n%s", want, scr)
		}
	}
	if strings.Contains(scr, "at rmdirSync") {
		t.Fatalf("a stack frame is on screen:\n%s", scr)
	}
}

// The unapplied count is what the sandbox holds, not what the edit tool wrote:
// a command that writes files counts too, and what apply would leave unticked
// — node_modules — does not. It was "7 unapplied" while package-lock.json sat
// uncounted, and nothing at all after a restart.
func TestTheUnappliedCountIsWhatTheSandboxHolds(t *testing.T) {
	s := newE2E(t,
		llmtest.ToolCall("run", map[string]any{"cmd": "npm install"}),
		llmtest.Text("Installed."),
	)
	s.sb.ExecFn = func(cmd, cwd string) sandbox.ExecResult {
		ctx := context.Background()
		s.sb.Write(ctx, "package-lock.json", []byte("{}"))
		s.sb.Write(ctx, "node_modules/react/index.js", []byte("x"))
		return sandbox.ExecResult{ExitCode: 0}
	}
	s.type_("install it\r")
	s.wait("Installed.")
	s.wait("1 unapplied")
}

// A sandbox that stops answering is said to, once, and started again — not
// reported to the model as "context deadline exceeded" call after call. The
// call that found out tells the model what happened and to run it again, and
// the next one runs in the restarted sandbox.
func TestASandboxThatStopsAnsweringIsRestarted(t *testing.T) {
	s := newE2E(t,
		llmtest.ToolCall("run", map[string]any{"cmd": "npm install"}),
		llmtest.ToolCall("run", map[string]any{"cmd": "npm install"}),
		llmtest.Text("Installed on the second try."),
	)
	s.sb.Wedged = true
	s.type_("install it\r")
	s.wait("Installed on the second try.")
	s.wait("the sandbox stopped answering · restarting it")
	if n := s.sb.Restarts(); n != 1 {
		t.Fatalf("restarted %d times, want once", n)
	}

	// What the model was told about the call that never got an answer.
	reqs := s.fake.Requests()
	var told string
	for _, m := range reqs[1].Messages {
		for _, b := range m.Blocks {
			told += b.Text
		}
	}
	if !strings.Contains(told, "john is restarting it") || !strings.Contains(told, "Run it again") {
		t.Fatalf("the model was told: %q", told)
	}
}
