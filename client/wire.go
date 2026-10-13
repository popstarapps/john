package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"github.com/coder/websocket"

	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
	"github.com/popstarapps/john/trace"
)

// wire is the client's half of the session protocol: it dials the backend,
// holds the resumable Conn, translates messages into terminalui events, and
// executes the tool calls the loop sends down — in the sandbox, and nowhere
// else. There is no code path here that could reach a model, which is the
// property the whole design stands on.

// clientLimits is what this client states on open: how much of a tool result
// it will send, how many tool calls it will run at once, and how much of a
// prompt's attachments it will carry. All three are its own numbers to state —
// it is the side that reads the output, the side that owns the sandbox the
// work runs in, and the side holding the developer's files.
//
// The attachment number is the whole of one prompt, not one file, and it is
// well under the transport's 16 MB frame: base64 costs a third on top, and a
// prompt is entitled to say something as well as carry something.
var clientLimits = protocol.Limits{
	MaxResultBytes: 1 << 20, MaxConcurrentTools: 4, MaxAttachmentBytes: 4 << 20,
}

// gate caps how many tool calls run at once.
//
// The backend may have several outstanding — a turn that reads four files
// sends four calls — and without this every one of them runs the moment it
// arrives. That is the client deciding to start as much work in the sandbox as
// the other end asks for, which is the shape of thing this client exists not
// to do: a number both sides agreed on and neither kept is not a limit.
//
// It is a channel of permits rather than a counter, so a call waiting for one
// waits on its own context too and a cancel while queued is answered rather
// than run late.
type gate struct {
	mu sync.Mutex
	ch chan struct{}
}

func newGate(n int) *gate {
	g := &gate{}
	g.resize(n)
	return g
}

// resize sets how many may run at once, for everything that starts from now
// on. A call already running holds a permit from the gate as it was and gives
// it back there, so a reconnect that negotiates a different number never has
// to interrupt work that is already in the sandbox.
func (g *gate) resize(n int) {
	if n < 1 {
		n = 1
	}
	ch := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		ch <- struct{}{}
	}
	g.mu.Lock()
	g.ch = ch
	g.mu.Unlock()
}

// enter waits for a permit. It reports false when the call was cancelled
// before it ever got one, which is a call that must be answered and must not
// run.
func (g *gate) enter(ctx context.Context) (release func(), ok bool) {
	g.mu.Lock()
	ch := g.ch
	g.mu.Unlock()
	select {
	case <-ch:
		return func() { ch <- struct{}{} }, true
	case <-ctx.Done():
		return func() {}, false
	}
}

type wire struct {
	app     *app
	url     string // backend base URL, http(s)://…
	workdir string
	wsKey   string

	// identity is this machine's trace key. Its recipient goes up on open so
	// the backend can wrap the session key for it; its private half never
	// leaves here, and answers the backend's request to unlock a session it
	// wrote earlier and can no longer read.
	identity *credstore.Identity

	mu       sync.Mutex
	conn     *protocol.Conn // swapped by the reconnect paths while the UI sends on it
	token    string         // renewed underneath by the refresher; read on every dial
	session  string         // backend session id, once opened
	resume   string         // a past session to carry on with, set by /resume
	fresh    bool           // the next open starts a new conversation, set by /new
	tools    map[string]context.CancelFunc
	results  map[string]*protocol.ToolResult // this turn's cache, for re-issued calls
	gate     *gate                           // caps how many of those run at once
	stopping bool
	commands int // commands running, for what the busy line says
}

// commandStarted and commandEnded keep the busy line saying "running" while
// a command runs and "thinking" otherwise. It said "thinking" through a
// two-minute sleep, while the model was waiting on the sandbox.
func (w *wire) commandStarted() {
	w.mu.Lock()
	w.commands++
	first := w.commands == 1
	w.mu.Unlock()
	if first {
		w.app.ui.Send(terminalui.BusyLabel{Label: "running"})
	}
}

func (w *wire) commandEnded() {
	w.mu.Lock()
	w.commands--
	last := w.commands == 0
	w.mu.Unlock()
	if last {
		w.app.ui.Send(terminalui.BusyLabel{Label: "thinking"})
	}
}

func newWire(a *app, backendURL, token, workdir string) *wire {
	w := &wire{
		app: a, url: strings.TrimSuffix(backendURL, "/"), token: token,
		workdir: workdir, wsKey: sandbox.WorkspaceKey(workdir),
		tools: map[string]context.CancelFunc{}, results: map[string]*protocol.ToolResult{},
		gate: newGate(clientLimits.MaxConcurrentTools),
	}
	// Best effort: a machine that cannot write an identity still gets a
	// session, it just cannot read its own traces back afterwards.
	w.identity, _ = credstore.EnsureIdentity()
	return w
}

// recipient is the public half to register, or "" when this machine has no
// identity.
func (w *wire) recipient() string {
	if w.identity == nil {
		return ""
	}
	return w.identity.Recipient
}

// openMessage is the client's session.open, in one place because three paths
// send it: the first connect, a reopen after a refused resume, and /resume.
func (w *wire) openMessage() *protocol.SessionOpen {
	w.mu.Lock()
	resume, fresh := w.resume, w.fresh
	w.mu.Unlock()
	return &protocol.SessionOpen{
		New:          fresh,
		Workspace:    w.workdir,
		WorkspaceKey: w.wsKey,
		Client:       w.clientInfo(),
		Protocol:     protocol.Version,
		Limits:       clientLimits,
		Sandbox:      protocol.SandboxInfo{State: string(sandbox.StateReady)},
		Session:      resume,
		TraceKey:     w.recipient(),
	}
}

func (w *wire) wsURL() string {
	u := w.url + "/api/v1/session"
	u = strings.Replace(u, "http://", "ws://", 1)
	u = strings.Replace(u, "https://", "wss://", 1)
	return u
}

// currentToken reads the token the wire is holding.
func (w *wire) currentToken() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.token
}

// setToken takes a freshly refreshed access token. The next dial uses it;
// a connection already open is unaffected, having been authenticated once at
// its own dial.
func (w *wire) setToken(t string) {
	w.mu.Lock()
	w.token = t
	w.mu.Unlock()
}

// currentConn is the Conn this wire is attached to, or nil before the first
// connect. Every reader takes a copy: the reconnect paths swap the field while
// the UI loop is sending, so reading it twice is reading two different Conns.
func (w *wire) currentConn() *protocol.Conn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn
}

// connected reports whether the session is open — whether there is a Conn to
// put a message on at all. send() before that has nowhere to go and drops the
// message, so the callers that must not lose one ask first.
func (w *wire) connected() bool { return w.currentConn() != nil }

// newConn builds a Conn that knows itself, so the detach it reports can be
// told apart from a detach on a Conn the wire has already moved on from.
func (w *wire) newConn() *protocol.Conn {
	var c *protocol.Conn
	c = protocol.NewConn(protocol.Options{OnDetach: func(error) { w.onDetach(c) }})
	return c
}

// swap installs c as the current Conn, records the session it belongs to, and
// returns the Conn it replaced for the caller to close outside the lock. The
// new one is in place before the old one is closed, so a send in between is
// buffered by a live Conn rather than dropped on a dead one.
func (w *wire) swap(c *protocol.Conn, session string) *protocol.Conn {
	w.mu.Lock()
	old := w.conn
	w.conn, w.session, w.resume = c, session, ""
	w.mu.Unlock()
	return old
}

func (w *wire) dial(ctx context.Context) (*protocol.WSTransport, error) {
	w.mu.Lock()
	token := w.token
	w.mu.Unlock()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	c, _, err := websocket.Dial(ctx, w.wsURL(), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	return protocol.NewWSTransport(c), nil
}

// connect opens the session for the first time.
func (w *wire) connect(ctx context.Context) error {
	tr, err := w.dial(ctx)
	if err != nil {
		return err
	}
	ready, err := w.handshake(ctx, tr, w.openMessage())
	if err != nil {
		tr.Close()
		return err
	}

	c := w.newConn()
	w.swap(c, ready.Session) // nothing to close: this is the first Conn
	w.app.applyReady(ready)
	w.app.carryingOn(ready)
	if err := c.Attach(tr, 0); err != nil {
		return err
	}
	go w.pump(c)
	go w.heartbeat(tr)
	return nil
}

// heartbeat pings the transport on an interval; two misses close it, which
// surfaces as a detach. Overridden shorter in tests.
var heartbeatInterval = 20 * time.Second

func (w *wire) heartbeat(tr *protocol.WSTransport) {
	misses := 0
	for {
		time.Sleep(heartbeatInterval)
		w.mu.Lock()
		stopping := w.stopping
		w.mu.Unlock()
		if stopping {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), heartbeatInterval/2)
		err := tr.Ping(ctx)
		cancel()
		if err == nil {
			misses = 0
			continue
		}
		misses++
		if misses >= 2 {
			tr.Close() // the reader sees the close and the Conn detaches
			return
		}
	}
}

// handshake sends one raw frame and reads the raw answer.
func (w *wire) handshake(ctx context.Context, tr protocol.Transport, open protocol.Message) (*protocol.SessionReady, error) {
	data, err := protocol.EncodeRaw(open)
	if err != nil {
		return nil, err
	}
	if err := tr.WriteFrame(ctx, data); err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		frame, err := tr.ReadFrame(hctx)
		if err != nil {
			return nil, err
		}
		msg, err := protocol.DecodeRaw(frame)
		if err != nil {
			return nil, err
		}
		switch m := msg.(type) {
		case *protocol.SessionReady:
			// Both sides have now stated their limits, so this is where the
			// smaller of the two starts governing — on a resume as well as a
			// first open, because a session can come back to a backend that
			// answers differently than the one it left.
			w.gate.resize(clientLimits.Negotiate(m.Limits).MaxConcurrentTools)
			return m, nil
		case *protocol.SessionKeyRequest:
			// The backend wrote this conversation and can no longer read it.
			// Only this machine can open the key, so it does, and hands back
			// the key — never the conversation, which stays the backend's to
			// author.
			answer := &protocol.SessionKey{Session: m.Session}
			if key, err := w.unwrap(m.Wrapped); err == nil {
				answer.Key = base64.StdEncoding.EncodeToString(key)
			}
			data, err := protocol.EncodeRaw(answer)
			if err != nil {
				return nil, err
			}
			if err := tr.WriteFrame(hctx, data); err != nil {
				return nil, err
			}
		case *protocol.SessionStale:
			return nil, staleError{reason: m.Reason}
		case *protocol.VersionMismatch:
			// The backend will not serve this build. Nothing about the
			// session is recoverable here — the answer is a different
			// binary — so this travels up as its own error and the session
			// ends rather than retrying against a door that stays shut.
			return nil, versionError{u: &update{
				Required: m.Required,
				Current:  w.app.version,
				URL:      w.url + m.Download,
				Token:    w.currentToken(),
			}}
		case *protocol.Error:
			return nil, refusedError{code: m.Code, message: m.Message}
		default:
			return nil, fmt.Errorf("unexpected handshake reply %s", m.T())
		}
	}
}

// refusedError is the backend answering the handshake with an error of its
// own: it is there, and it said no — with no model assigned, say. Reported as
// "backend unreachable", it sent the developer looking for a network problem.
type refusedError struct {
	code, message string
}

func (e refusedError) Error() string { return e.code + ": " + e.message }

// unwrap opens a session key with this machine's private identity.
func (w *wire) unwrap(wrapped string) ([]byte, error) {
	if w.identity == nil {
		return nil, credstore.ErrNoIdentity
	}
	id, err := age.ParseX25519Identity(w.identity.Private)
	if err != nil {
		return nil, err
	}
	return trace.UnwrapKey(wrapped, id)
}

type staleError struct{ reason string }

func (e staleError) Error() string { return "session stale: " + e.reason }

// versionError is the backend refusing this build. It carries what is needed
// to fix it, because the fix is the only useful thing to say about it.
type versionError struct{ u *update }

func (e versionError) Error() string {
	return "this backend runs john " + e.u.Required + ", you have " + e.u.Current
}

// clientInfo is what this client says it is, in session.open and
// session.resume alike: the backend refuses either from a different build.
func (w *wire) clientInfo() protocol.ClientInfo {
	return protocol.ClientInfo{Version: w.app.version, OS: runtime.GOOS + "/" + runtime.GOARCH}
}

// onDetach is the fail-closed moment: the wire dropped, the loop that answers
// prompts is unreachable, and the status line says so before anything else.
func (w *wire) onDetach(c *protocol.Conn) {
	w.mu.Lock()
	stopping, current := w.stopping, w.conn
	w.mu.Unlock()
	if stopping || c != current {
		// Either we are quitting, or a reopen has already replaced this Conn
		// and nobody is waiting on it. Reconnecting it would dial the backend
		// forever on behalf of a session that has been left behind.
		return
	}
	w.app.setOffline(true)
	go w.reconnectLoop(c)
}

// reconnectLoop resumes the numbered stream c was carrying. It gives up the
// moment c stops being the wire's Conn, because from then on the reconnect
// belongs to whoever replaced it.
func (w *wire) reconnectLoop(c *protocol.Conn) {
	backoff := time.Second
	for {
		w.mu.Lock()
		stopping, session, current := w.stopping, w.session, w.conn
		w.mu.Unlock()
		if stopping || c != current {
			return
		}

		time.Sleep(backoff)
		if backoff < 15*time.Second {
			backoff *= 2
		}

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		tr, err := w.dial(ctx)
		if err != nil {
			cancel()
			continue
		}
		ready, err := w.handshake(ctx, tr, &protocol.SessionResume{
			Session: session, WorkspaceKey: w.wsKey, Ack: c.RecvSeq(),
			Client: w.clientInfo(),
		})
		cancel()
		if err != nil {
			tr.Close()
			// The backend was upgraded while this client was away. Only an
			// update gets past that, so retrying would be refused forever.
			var ve versionError
			if errors.As(err, &ve) {
				w.app.updateRequired(ve, true)
				return
			}
			var stale staleError
			if errors.As(err, &stale) {
				// The parked turn is gone; the conversation survives on the
				// backend. Open fresh with a new numbered stream.
				w.app.notice(terminalui.NoticeWarn, "session resumed too late — reopening ("+stale.reason+")")
				w.reopenFresh()
				return
			}
			continue
		}
		if err := c.Attach(tr, ready.Ack); err != nil {
			tr.Close()
			continue
		}
		go w.heartbeat(tr)
		w.app.applyReady(ready)
		w.app.setOffline(false)
		return
	}
}

// currentSession is the backend session id this wire is attached to.
func (w *wire) currentSession() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.session
}

// resumeSession carries on an earlier conversation: it names the session on
// the next session.open and reconnects. The backend answers with that
// session's key, wrapped for this machine, and only after this machine hands
// the opened key back can the backend read its own record of the
// conversation.
func (w *wire) resumeSession(id string) error {
	return w.switchSession(id, false)
}

// newSession starts a new conversation in this workspace, as /new does: the
// backend ends the active one, which /resume still lists, and opens another.
func (w *wire) newSession() error {
	return w.switchSession("", true)
}

// switchSession moves this client to another conversation: a named past one,
// or a new one. Either way it is a fresh session.open on a new connection.
func (w *wire) switchSession(id string, fresh bool) error {
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return errors.New("session is closing")
	}
	prevResume := w.resume
	w.resume, w.fresh = id, fresh
	w.mu.Unlock()
	// A new conversation is asked for once. A reconnect later carries on the
	// one it became, rather than ending it to start yet another.
	defer func() {
		w.mu.Lock()
		w.fresh = false
		w.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tr, err := w.dial(ctx)
	if err != nil {
		return err
	}
	ready, err := w.handshake(ctx, tr, w.openMessage())
	if err != nil {
		tr.Close()
		w.mu.Lock()
		w.resume = prevResume
		w.mu.Unlock()
		return err
	}
	if id != "" && ready.Session != id {
		// The backend could not open that conversation and started a new one
		// rather than continuing an empty one wearing its name.
		w.app.notice(terminalui.NoticeWarn,
			"that session could not be unlocked with this machine's key — started a new one")
	}

	c := w.newConn()
	if old := w.swap(c, ready.Session); old != nil {
		old.Close()
	}
	w.app.applyReady(ready)
	w.app.turnLeftBehind()
	if err := c.Attach(tr, 0); err != nil {
		tr.Close()
		return err
	}
	go w.pump(c)
	go w.heartbeat(tr)
	w.app.setOffline(false)
	return nil
}

// reopenFresh abandons the old numbered stream and starts over: new Conn, new
// session.open. Used only after the backend refused a resume.
func (w *wire) reopenFresh() {
	var lastRefusal string
	for {
		w.mu.Lock()
		stopping := w.stopping
		w.mu.Unlock()
		if stopping {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		tr, err := w.dial(ctx)
		if err != nil {
			cancel()
			time.Sleep(2 * time.Second)
			continue
		}
		ready, err := w.handshake(ctx, tr, w.openMessage())
		cancel()
		if err != nil {
			tr.Close()
			// A backend that answered and said no will say it again. A
			// version refusal ends the session, because only an update gets
			// past it; any other is said once rather than retried in silence
			// behind a status line that only says "unreachable".
			var ve versionError
			if errors.As(err, &ve) {
				w.app.updateRequired(ve, true)
				return
			}
			var refused refusedError
			if errors.As(err, &refused) && refused.message != lastRefusal {
				lastRefusal = refused.message
				w.app.notice(terminalui.NoticeError, "the backend refused the session: "+refused.message)
			}
			time.Sleep(2 * time.Second)
			continue
		}
		c := w.newConn()
		if old := w.swap(c, ready.Session); old != nil {
			old.Close()
		}
		w.app.applyReady(ready)
		// The turn this client was showing belonged to the stream that was
		// refused, and nothing on this one has claimed it. Said before the
		// pump starts, so a backend that did pick the turn up is the next
		// thing on screen rather than a contradiction of it.
		w.app.turnLeftBehind()
		c.Attach(tr, 0)
		go w.pump(c)
		go w.heartbeat(tr)
		w.app.setOffline(false)
		return
	}
}

func (w *wire) close() {
	w.mu.Lock()
	w.stopping = true
	c := w.conn
	w.mu.Unlock()
	if c != nil {
		c.Send(context.Background(), &protocol.SessionClose{Reason: "quit"})
		time.Sleep(50 * time.Millisecond) // give the close a chance to flush
		c.Close()
	}
}

// send puts a message on the current Conn, which buffers it for replay if the
// transport is between attachments. Before the session is open there is no
// Conn and the message is dropped; a caller that cannot afford that asks
// connected() first.
func (w *wire) send(m protocol.Message) {
	c := w.currentConn()
	if c == nil {
		return
	}
	c.Send(context.Background(), m)
}

// pump translates the backend's narration into UI events. It reads the Conn it
// was started for rather than whichever one the wire holds now, so a reopen
// leaves the old pump to drain its own stream and end with it.
func (w *wire) pump(c *protocol.Conn) {
	a := w.app
	for msg := range c.In() {
		switch m := msg.(type) {
		case *protocol.TurnBegin:
			// The turn id matters here, not just the spinner: a turn the
			// backend picked up again after a restart was begun by a client
			// process that is gone, and Esc has to interrupt the turn that is
			// actually running rather than one this process remembers.
			a.beginTurn(m.Turn)
		case *protocol.ThinkingDelta:
			a.ui.Send(terminalui.Thinking{Text: m.Text})
		case *protocol.MessageStart:
			a.ui.Send(terminalui.AssistantStart{ID: m.ID})
		case *protocol.MessageDelta:
			a.ui.Send(terminalui.AssistantDelta{ID: m.ID, Text: m.Text})
		case *protocol.MessageEnd:
			a.ui.Send(terminalui.AssistantEnd{ID: m.ID})
		case *protocol.ToolCall:
			go w.execTool(m)
		case *protocol.ToolCancel:
			w.mu.Lock()
			cancel := w.tools[m.ID]
			w.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		case *protocol.ModelNarrow:
			go a.onNarrow(m)
		case *protocol.ModelSelected:
			a.onModelSelected(m)
		case *protocol.Usage:
			a.setContext(m.Context, m.ContextWindow)
		case *protocol.Notice:
			level := terminalui.NoticeInfo
			switch m.Level {
			case "warn":
				level = terminalui.NoticeWarn
			case "error":
				level = terminalui.NoticeError
			}
			a.ui.Send(terminalui.Notice{Level: level, Text: m.Text})
		case *protocol.TurnEnd:
			w.mu.Lock()
			w.results = map[string]*protocol.ToolResult{} // the turn's cache dies with it
			w.mu.Unlock()
			a.endTurn(m)
		case *protocol.Error:
			a.notice(terminalui.NoticeError, m.Message)
		case *protocol.SessionClosed:
			a.notice(terminalui.NoticeWarn, "session closed: "+m.Reason)
			return
		case *protocol.PolicyRefresh:
			// An administrator changed something. The message carries no
			// policy — the snapshot is signed and comes over the API — so
			// this is only the cue to go and pull it, off this loop because
			// it is a request over the network and the wire has messages to
			// keep reading.
			go a.refreshPolicies(m.What)
		case *protocol.AccessDecision:
			a.accessDecided(m)
		}
	}
}

// execTool runs one tool call in the sandbox and answers it. A re-issued call
// (same id, after a reconnect) answers from the cache instead of running side
// effects twice.
func (w *wire) execTool(call *protocol.ToolCall) {
	w.mu.Lock()
	if cached, ok := w.results[call.ID]; ok {
		w.mu.Unlock()
		w.send(cached)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.tools[call.ID] = cancel
	w.mu.Unlock()

	// Wait for one of the negotiated slots. The cancel above is registered
	// first on purpose: a call that is queued is a call that can be cancelled,
	// and one that was cancelled while it waited is answered as cancelled
	// without ever running. It is not cached either — it had no result, and a
	// re-issue after a reconnect should run it.
	release, ok := w.gate.enter(ctx)
	if !ok {
		w.mu.Lock()
		delete(w.tools, call.ID)
		w.mu.Unlock()
		w.send(&protocol.ToolResult{ID: call.ID, Status: "cancelled"})
		return
	}
	if call.Tool == "run" {
		w.commandStarted()
	}
	res := w.runTool(ctx, call)
	if call.Tool == "run" {
		w.commandEnded()
	}
	release()

	w.mu.Lock()
	delete(w.tools, call.ID)
	w.results[call.ID] = res
	w.mu.Unlock()
	w.send(res)
	if call.Tool == "run" || call.Tool == "edit" {
		// After the answer, not before it: the count is for the status
		// line and never holds the model up.
		w.app.recountUnapplied()
	}
}

func (w *wire) runTool(ctx context.Context, call *protocol.ToolCall) *protocol.ToolResult {
	a := w.app
	start := time.Now()
	res := &protocol.ToolResult{ID: call.ID}
	a.awaitSandbox(ctx)
	fail := func(msg string) *protocol.ToolResult {
		res.Status, res.Stderr = "failed", msg
		if call.Tool == "run" {
			// It never ran to an exit: -1, not the 0 an unset code reads as
			// beside "failed".
			res.Exit = -1
		}
		res.DurationMS = int(time.Since(start).Milliseconds())
		a.ui.Send(terminalui.ToolEnd{ID: call.ID, Status: terminalui.ToolFail, Meta: msg})
		return res
	}
	// A workspace-protection refusal is a decision, not a failure: it comes
	// back blocked, with the rule in `policy`, so the loop learns that the
	// path is not its to change instead of trying again another way.
	failErr := func(err error) *protocol.ToolResult {
		if errors.Is(err, sandbox.ErrStoppedAnswering) {
			a.sandboxStopped()
			return fail(stoppedAnswering)
		}
		var pe *policy.ProtectedError
		if !errors.As(err, &pe) {
			return fail(err.Error())
		}
		res.Status, res.Policy, res.Stderr = "blocked", "workspace protection", pe.Error()
		res.DurationMS = int(time.Since(start).Milliseconds())
		meta := "read-only by policy"
		switch pe.Mode {
		case policy.ModeExclude:
			meta = "excluded by policy"
		case policy.ModeNoDelete:
			meta = "no-delete by policy"
		}
		a.ui.Send(terminalui.ToolEnd{ID: call.ID, Status: terminalui.ToolBlocked, Meta: meta})
		return res
	}

	// badArgs answers a call whose arguments are not what the tool takes,
	// saying which: "bad arguments" alone left the model guessing, and with
	// no ToolStart before it the call never appeared on screen at all.
	badArgs := func(want string) *protocol.ToolResult {
		a.ui.Send(terminalui.ToolStart{ID: call.ID, Verb: call.Tool, Target: "(bad arguments)"})
		return fail(fmt.Sprintf("bad arguments: %s takes %s; got %s", call.Tool, want, call.Args))
	}

	switch call.Tool {
	case "read":
		var args struct {
			Path     string `json:"path"`
			MaxBytes int    `json:"max_bytes"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil || args.Path == "" {
			return badArgs(`{"path": "...", "max_bytes": n}`)
		}
		args.Path = a.inWorkspace(args.Path)
		a.ui.Send(terminalui.ToolStart{ID: call.ID, Verb: "read", Target: args.Path})
		max := args.MaxBytes
		if max <= 0 || max > 1<<20 {
			max = 256 << 10
		}
		content, err := a.sb.Read(ctx, args.Path, max)
		if err != nil {
			return failErr(err)
		}
		if utf8.Valid(content.Data) {
			res.Content = string(content.Data)
		} else {
			res.ContentB64 = base64.StdEncoding.EncodeToString(content.Data)
		}
		res.Truncated = content.Truncated
		res.Status = "ok"
		a.ui.Send(terminalui.ToolEnd{ID: call.ID, Status: terminalui.ToolOK,
			Meta: fmt.Sprintf("%d bytes", len(content.Data))})

	case "edit":
		var args struct {
			Path    string  `json:"path"`
			Find    string  `json:"find"`
			Replace string  `json:"replace"`
			All     bool    `json:"all"`
			Content *string `json:"content"`
			Delete  bool    `json:"delete"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil || args.Path == "" {
			return badArgs(`{"path": "...", and "content", or "find" and "replace", or "delete"}`)
		}
		args.Path = a.inWorkspace(args.Path)
		a.ui.Send(terminalui.ToolStart{ID: call.ID, Verb: "edit", Target: args.Path})
		switch {
		case args.Delete:
			if err := a.sb.Delete(ctx, args.Path); err != nil {
				return failErr(err)
			}
		case args.Content != nil:
			if err := a.sb.Write(ctx, args.Path, []byte(*args.Content)); err != nil {
				return failErr(err)
			}
		case args.Find != "":
			current, err := a.sb.Read(ctx, args.Path, 0)
			if err != nil {
				return failErr(err)
			}
			text := string(current.Data)
			count := strings.Count(text, args.Find)
			if count == 0 {
				return fail("find text not present in " + args.Path)
			}
			if count > 1 && !args.All {
				return fail(fmt.Sprintf("find text appears %d times in %s — make it unique or pass all", count, args.Path))
			}
			var next string
			if args.All {
				next = strings.ReplaceAll(text, args.Find, args.Replace)
			} else {
				next = strings.Replace(text, args.Find, args.Replace, 1)
			}
			if err := a.sb.Write(ctx, args.Path, []byte(next)); err != nil {
				return failErr(err)
			}
		default:
			return fail("edit needs find/replace, content, or delete")
		}
		res.Status = "ok"
		a.markDirty(args.Path)
		a.ui.Send(terminalui.ToolEnd{ID: call.ID, Status: terminalui.ToolOK})

	case "run":
		var args struct {
			Cmd       string `json:"cmd"`
			Cwd       string `json:"cwd"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil || args.Cmd == "" {
			return badArgs(`{"cmd": "...", "cwd": "...", "timeout_ms": n}`)
		}
		a.ui.Send(terminalui.ToolStart{ID: call.ID, Verb: "run", Target: args.Cmd})
		timeout := time.Duration(args.TimeoutMS) * time.Millisecond
		out, err := a.sb.Exec(ctx, args.Cmd, args.Cwd, timeout)
		if err != nil {
			if ctx.Err() != nil {
				res.Status = "cancelled"
				res.DurationMS = int(time.Since(start).Milliseconds())
				a.ui.Send(terminalui.ToolEnd{ID: call.ID, Status: terminalui.ToolSkipped, Meta: "cancelled"})
				return res
			}
			return failErr(err)
		}
		res.Exit = out.ExitCode
		res.Stdout = string(out.Stdout)
		res.Stderr = string(out.Stderr)
		res.Truncated = out.Truncated
		if out.ExitCode == 0 {
			res.Status = "ok"
			a.ui.Send(terminalui.ToolEnd{ID: call.ID, Status: terminalui.ToolOK,
				Meta: toolDuration(out.Duration)})
		} else {
			res.Status = "failed"
			for _, line := range failureLines(out.Stderr, out.Stdout, 3) {
				a.ui.Send(terminalui.ToolDetail{ID: call.ID, Text: line})
			}
			a.ui.Send(terminalui.ToolEnd{ID: call.ID, Status: terminalui.ToolFail,
				Meta: fmt.Sprintf("exit %d", out.ExitCode)})
		}

	default:
		// The tool list is closed. A capability the client guesses at is a
		// capability nobody governed.
		res.Status = "blocked"
		res.Policy = "closed tool set"
		res.Stderr = "unknown tool: " + call.Tool
		a.ui.Send(terminalui.ToolEnd{ID: call.ID, Status: terminalui.ToolBlocked, Meta: "unknown tool"})
	}
	res.DurationMS = int(time.Since(start).Milliseconds())
	return res
}

// inWorkspace turns a path the agent read off the sandbox — absolute, under
// the workspace's mount, which mirrors the host's own path — into the
// workspace-relative one the sandbox takes. `pwd`, compiler output and stack
// traces all print the absolute form, so it is the one the agent reaches for;
// refusing it as "escapes the workspace" sent it writing somewhere else.
// Anything outside the workspace is left as it is, for the sandbox to refuse.
func (a *app) inWorkspace(p string) string {
	mount := sandbox.GuestMount(a.workdir)
	if rest, ok := strings.CutPrefix(p, strings.TrimSuffix(mount, "/")+"/"); ok && rest != "" {
		return rest
	}
	return p
}

// failureLines is what a failed command says about why, for under its line on
// screen, from stderr, or from stdout for a program that reports there.
// "exit 1" alone left the developer to guess — the reason went to the model
// and nowhere else.
//
// It is the first few lines, once what says nothing about this failure is
// gone. Tools lead with the reason and follow it with advice and where the
// log went: npm's "403 Forbidden - GET …/@vitejs/plugin-react" is its second
// line, and the last three were "on a server you do not have access to" and a
// log path. apt's E: lines come first too. Stack frames say where rather than
// why, so they go; and a Python traceback, which ends with the exception
// rather than starting with it, is read from the end.
func failureLines(stderr, stdout []byte, n int) []string {
	from := stderr
	if len(strings.TrimSpace(string(from))) == 0 {
		from = stdout
	}
	var lines []string
	traceback, footer := false, false
	for _, l := range strings.Split(string(from), "\n") {
		l = strings.TrimSpace(l)
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(l, "npm ERR!"), "npm error"))
		switch {
		case footer:
			footer = false // the log's path, under npm's line about it
		case strings.HasPrefix(body, "A complete log of this run can be found in"):
			footer = true
		case body == "", stackFrame.MatchString(l):
		case strings.HasPrefix(l, "Traceback (most recent call last)"):
			traceback = true
		default:
			lines = append(lines, l)
		}
	}
	if traceback {
		return lines[max(0, len(lines)-n):]
	}
	return lines[:min(n, len(lines))]
}

// stackFrame is one frame of a JavaScript, Python or Java stack trace.
var stackFrame = regexp.MustCompile(`^(at \S|File ".*", line \d+)`)
