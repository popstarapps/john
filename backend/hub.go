package backend

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/oliverkofoed/gokit/agentkit"
	"github.com/oliverkofoed/gokit/agentkit/llm"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/trace"
)

// The agent loop runs here and nowhere else. The client executes the three
// tools and holds no model credential; every byte on its way to a provider
// passes through this file, which is what makes the controls of later stages
// (replacements, keyword narrowing) enforceable by construction.

// The sandbox is layered over the developer's working directory, not a copy
// of it, and the prompt says so. Called "a sandboxed copy", it had the model
// doubt a host edit it had just found with grep: "this reflects the copy of
// index.html in my sandbox".
const systemPrompt = `You are john, a careful coding agent working in a sandbox over the developer's project.

You have exactly three tools: read (read a file), edit (change, create or delete a file), and run (execute a shell command in the workspace). Listing a directory or searching the tree is a run command (ls, find, grep). There are no other tools and none can be added.

The workspace is the developer's working directory, live, with your changes layered on top. A file the developer changes while you work is what you read next, unless you have changed it yourself. Your edits stay in the sandbox and do not reach the working directory until the developer reviews and applies the accumulated diff when you are done. Work in small, verifiable steps: read before you edit, run the project's own tests or build to check yourself, and finish with a short summary of what changed and how you verified it.

The sandbox's tools come from the project's .john/Dockerfile. A project without one gets the starter: FROM debian:stable-slim, with ca-certificates, curl, git, build-essential, jq, ripgrep and a few other basics. When the work needs something the sandbox lacks — a language runtime, a package manager, a compiler — add it to .john/Dockerfile rather than installing it by hand: what apt-get installs in the running sandbox is gone at the next rebuild, and the image build usually reaches package mirrors without the network policies the sandbox needs. If the project has no .john/Dockerfile, do not write one from scratch: ask the developer to run /env init, which writes the starter into the project, and add to it once it is there. Your edit reaches the developer in the diff like any other file; once they apply it, /rebuild rebuilds the sandbox from it and keeps your unapplied changes. You cannot rebuild the sandbox yourself, so say what you added, ask the developer to apply and /rebuild, and end the turn there; carry on with the tools in place when they ask you to.

The sandbox has no direct network: no DNS and no raw sockets, so name lookups, ping and /dev/tcp fail by design. HTTP and HTTPS go through john's proxy, set in HTTP_PROXY and HTTPS_PROXY, which programs that honour those variables use on their own — curl, wget, apt-get, pip, npm, go and git over HTTPS among them. The proxy applies the organization's network policies. A request no policy covers is refused with a 403 from the john proxy, and the developer is offered to ask an administrator for access; when that happens, say which host was blocked and stop there rather than trying another way around it. To install a Debian package, run apt-get update before apt-get install.`

// resumableFor is how long a disconnected session stays parked before its
// turn is aborted and its memory released. The conversation itself survives
// in the database either way.
const resumableFor = 5 * time.Minute

type hub struct {
	srv *Server

	mu       sync.Mutex
	sessions map[string]*hubSession
}

func newHub(srv *Server) *hub {
	h := &hub{srv: srv, sessions: map[string]*hubSession{}}
	// An administrator's decision reaches the sessions that are open now.
	// Without this a developer whose access request was just approved waits
	// for their snapshot to age out before the request they asked about
	// starts working, and nothing on their screen says why.
	srv.store.OnPolicyChange(h.policyChanged)
	return h
}

// policyChanged tells every live session that what it holds is out of date.
// The message carries no policy content: the snapshot is signed and pulled
// over the API, and this is only the nudge to go and get it, with what the
// change was about so the developer can be told.
func (h *hub) policyChanged(epoch uint64, what []string) {
	h.mu.Lock()
	sessions := make([]*hubSession, 0, len(h.sessions))
	for _, hs := range h.sessions {
		sessions = append(sessions, hs)
	}
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, hs := range sessions {
		// What the backend enforces itself — routing keywords, replacements,
		// the models — changes here, before the client is told to refresh.
		hs.reloadControls()
		hs.conn.Send(ctx, &protocol.PolicyRefresh{Epoch: epoch, What: what})
	}
}

// tellUser sends a message to every session one developer has open.
func (h *hub) tellUser(userID string, m protocol.Message) {
	h.mu.Lock()
	var theirs []*hubSession
	for _, hs := range h.sessions {
		if hs.userID == userID {
			theirs = append(theirs, hs)
		}
	}
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, hs := range theirs {
		hs.conn.Send(ctx, m)
	}
}

// shutdownAll ends every live session. WebSockets are hijacked connections
// that http.Server.Close never touches, so the hub has to hang up itself —
// clients see the wire drop, fail closed, and reconnect when the backend
// returns.
func (h *hub) shutdownAll() {
	h.mu.Lock()
	live := make([]*hubSession, 0, len(h.sessions))
	for _, hs := range h.sessions {
		live = append(live, hs)
	}
	h.mu.Unlock()
	for _, hs := range live {
		// Said before the interrupt, because it changes what the interrupt
		// means: a turn stopped by the backend going away has not ended, and
		// the next connection is meant to pick it up.
		hs.mu.Lock()
		hs.leaving = true
		done := hs.turnDone
		hs.mu.Unlock()
		hs.agent.Interrupt()
		// The interrupt answers the outstanding tool calls as interrupted and
		// checkpoints the conversation, on the turn's own goroutine. Waiting
		// for it is what makes the turn resumable: shut down first and the
		// last checkpoint is the one still asking for tools — or the database
		// is closed before the new one lands — and the next backend can only
		// call the turn lost. Bounded, because a deploy must not hang on one
		// session.
		if done != nil {
			select {
			case <-done:
			case <-time.After(turnWindDown):
			}
		}
		hs.shutdown("backend shutting down")
	}
}

// turnWindDown is how long a stopping backend waits for an interrupted turn
// to write its last checkpoint.
const turnWindDown = 5 * time.Second

// handleSessionWS is GET /api/v1/session: authenticate, upgrade, handshake.
func (s *Server) handleSessionWS(w http.ResponseWriter, r *http.Request) {
	user := s.auth.UserFor(r)
	if user == nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The client is a CLI, not a browser; there is no origin to check.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	tr := protocol.NewWSTransport(c)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	frame, err := tr.ReadFrame(ctx)
	cancel()
	if err != nil {
		tr.Close()
		return
	}
	msg, err := protocol.DecodeRaw(frame)
	if err != nil {
		tr.Close()
		return
	}

	switch m := msg.(type) {
	case *protocol.SessionOpen:
		if mismatch := s.versionMismatch(m.Client); mismatch != nil {
			sendRaw(tr, mismatch)
			tr.Close()
			return
		}
		s.hub.open(user, m, tr)
	case *protocol.SessionResume:
		if mismatch := s.versionMismatch(m.Client); mismatch != nil {
			sendRaw(tr, mismatch)
			tr.Close()
			return
		}
		s.hub.resume(user, m, tr)
	default:
		sendRaw(tr, &protocol.Error{Code: protocol.ErrInternal, Message: "expected session.open or session.resume", Fatal: true})
		tr.Close()
	}
}

// versionMismatch decides whether to refuse this client, and returns the
// refusal to send. Nil means carry on.
//
// Three things have to be true to refuse. The versions have to actually
// differ. The client has to be a released build — a "dev" or unstamped
// binary is somebody working on john itself, and there is no release to send
// them to. And this backend has to be holding the build for their platform,
// because refusing a client we cannot also fix would strand it with no way
// back in; a `make build` backend bundles no clients at all, and says so on
// the download endpoints rather than locking anyone out here.
func (s *Server) versionMismatch(c protocol.ClientInfo) *protocol.VersionMismatch {
	client := c.Version
	if client == s.cfg.Version || isDevBuild(client) || isDevBuild(s.cfg.Version) {
		return nil
	}
	download := releaseFor(c.OS)
	if download == "" {
		return nil
	}
	return &protocol.VersionMismatch{
		Required: s.cfg.Version,
		Client:   client,
		Download: download,
	}
}

// isDevBuild reports a binary built without a version stamped into it.
func isDevBuild(v string) bool { return v == "" || v == "dev" }

func sendRaw(tr protocol.Transport, m protocol.Message) {
	if data, err := protocol.EncodeRaw(m); err == nil {
		tr.WriteFrame(context.Background(), data)
	}
}

// open starts (or reopens) the conversation for a workspace.
func (h *hub) open(user *store.User, m *protocol.SessionOpen, tr protocol.Transport) {
	st := h.srv.store

	models, err := h.availableModels(user.ID)
	if err != nil {
		sendRaw(tr, &protocol.Error{Code: protocol.ErrInternal, Message: err.Error(), Fatal: true})
		tr.Close()
		return
	}
	if len(models) == 0 {
		sendRaw(tr, &protocol.Error{Code: protocol.ErrNoModel,
			Message: "no model is assigned to you — an administrator assigns models through policy groups", Fatal: true})
		tr.Close()
		return
	}

	// The developer's public key, as their client last generated it. Recorded
	// before any session key is minted, so this session is wrapped for the key
	// the client is holding right now.
	if m.TraceKey != "" && m.TraceKey != user.TracePubKey {
		if err := st.SetUserTracePubKey(user.ID, m.TraceKey); err == nil {
			user.TracePubKey = m.TraceKey
		}
	}

	var row *store.Session
	if m.Session != "" {
		// `/resume`: a named past conversation, and only the developer's own.
		row, err = st.SessionByID(m.Session)
		if err == nil && row.UserID != user.ID {
			err = store.ErrNotFound
		}
		if err != nil {
			sendRaw(tr, &protocol.Error{Code: protocol.ErrSessionUnknown,
				Message: "no such session for this workspace", Fatal: true})
			tr.Close()
			return
		}
		if err := st.ReopenSession(row.ID, user.ID); err != nil {
			sendRaw(tr, &protocol.Error{Code: protocol.ErrInternal, Message: err.Error(), Fatal: true})
			tr.Close()
			return
		}
	} else {
		// `/new`: the workspace's active conversation is ended, and the new
		// one starts on the model it was on, if that is still allowed.
		startOn := models[0]
		if m.New {
			for {
				prev, err := st.FindActiveSession(user.ID, m.WorkspaceKey)
				if err != nil {
					break
				}
				for _, mo := range models {
					if mo.ID == prev.ModelID {
						startOn = mo
					}
				}
				if err := st.EndSession(prev.ID); err != nil {
					break
				}
			}
		}
		row, err = st.FindActiveSession(user.ID, m.WorkspaceKey)
		if errors.Is(err, store.ErrNotFound) {
			row, err = st.CreateSession(user.ID, m.WorkspaceKey, m.Workspace, "", startOn.ID)
			if err == nil {
				st.AddUsage(time.Now(), user.ID, startOn.ID, store.UsageDelta{Sessions: 1})
			}
		}
		if err != nil {
			sendRaw(tr, &protocol.Error{Code: protocol.ErrInternal, Message: err.Error(), Fatal: true})
			tr.Close()
			return
		}
	}

	// The session key. A conversation the backend has written before comes
	// back locked: the key is in the trace file's header, wrapped for this
	// developer, and only their client can open it. A client that cannot —
	// re-keyed machine, lost identity — gets a fresh conversation rather than
	// one that silently lost its history.
	key, locked, err := h.sessionKey(user, row, tr)
	if err == nil && locked {
		// Its history is unreachable, so this conversation cannot be carried
		// on and must not be quietly continued as an empty one.
		row, err = st.CreateSession(user.ID, m.WorkspaceKey, m.Workspace, "", models[0].ID)
		if err == nil {
			st.AddUsage(time.Now(), user.ID, models[0].ID, store.UsageDelta{Sessions: 1})
			key, _, err = h.sessionKey(user, row, tr)
		}
	}
	if err != nil {
		sendRaw(tr, &protocol.Error{Code: protocol.ErrInternal, Message: err.Error(), Fatal: true})
		tr.Close()
		return
	}

	// A second client for the same conversation replaces the first: the
	// session lives here, and there is exactly one live wire to it.
	h.mu.Lock()
	if old := h.sessions[row.ID]; old != nil {
		delete(h.sessions, row.ID)
		go old.shutdown("replaced by a new connection")
	}
	h.mu.Unlock()

	hs, err := h.build(user, row, models, key, m.Limits)
	if err != nil {
		sendRaw(tr, &protocol.Error{Code: protocol.ErrInternal, Message: err.Error(), Fatal: true})
		tr.Close()
		return
	}

	h.mu.Lock()
	h.sessions[row.ID] = hs
	h.mu.Unlock()

	sendRaw(tr, hs.readyMessage(0))
	hs.conn.Attach(tr, 0)
	go hs.run()
	hs.traceRecord("session.open", map[string]any{"workspace": m.Workspace, "client": m.Client})
	if row.TurnID != "" {
		hs.resumeTurn(row.TurnID)
	}
}

// sessionKey obtains the key this session encrypts with.
//
// A conversation the backend has never written gets a fresh key, minted here
// and wrapped — for the organization, and for the developer when their client
// has registered a public key — into the header of its trace file. From then
// on the backend can only get the key back by asking the client for it, which
// is what the rest of this does: the wrapped copy goes down as a
// SessionKeyRequest and the client returns the opened key.
//
// locked reports a session whose file exists but whose key cannot be
// recovered — no copy was wrapped for this developer, or their client could
// not open the one that was. The caller starts a fresh conversation rather
// than continuing one whose history it cannot read.
func (h *hub) sessionKey(user *store.User, row *store.Session, tr protocol.Transport) (key []byte, locked bool, err error) {
	w := h.srv.traceWriter()
	if w == nil {
		// No trace store: nothing is encrypted, exactly as before session
		// keys existed. The console says so on the organization page.
		return nil, false, nil
	}
	sess := w.Session(row.ID, row.StartedAt)
	header, err := sess.Header()
	if err != nil {
		return nil, false, err
	}
	if header == nil {
		key, _, err = sess.Create(user.ID, user.TracePubKey, row.StartedAt)
		return key, false, err
	}

	wrapped, ok := header.Key(user.ID)
	if !ok {
		return nil, true, nil
	}
	sendRaw(tr, &protocol.SessionKeyRequest{Session: row.ID, Wrapped: wrapped})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	frame, err := tr.ReadFrame(ctx)
	cancel()
	if err != nil {
		return nil, false, err
	}
	msg, err := protocol.DecodeRaw(frame)
	if err != nil {
		return nil, false, err
	}
	answer, ok := msg.(*protocol.SessionKey)
	if !ok || answer.Key == "" {
		return nil, true, nil
	}
	raw, err := base64.StdEncoding.DecodeString(answer.Key)
	if err != nil || len(raw) != trace.KeySize {
		return nil, true, nil
	}
	return raw, false, nil
}

// resume reattaches a parked session's wire.
func (h *hub) resume(user *store.User, m *protocol.SessionResume, tr protocol.Transport) {
	h.mu.Lock()
	hs := h.sessions[m.Session]
	h.mu.Unlock()
	if hs == nil || hs.userID != user.ID {
		sendRaw(tr, &protocol.SessionStale{Reason: "unknown session — open a fresh one"})
		tr.Close()
		return
	}
	if hs.conn.Stale() {
		sendRaw(tr, &protocol.SessionStale{Reason: "replay buffer overflowed while you were away"})
		tr.Close()
		return
	}
	hs.stopParkTimer()
	sendRaw(tr, hs.readyMessage(hs.conn.RecvSeq()))
	hs.conn.Attach(tr, m.Ack)
}

// alreadyNarrowed is which keyword rules a reopened session has already been
// narrowed by. The session records the models it was narrowed to, not the
// rules that did it, so a rule counts as having fired when every one of those
// models is one it permits: firing it again could narrow nothing further, and
// would only ask the developer once more. Asked on a session carried on after
// a restart, with nobody yet at the keyboard to answer, it held the turn
// until it was aborted.
func alreadyNarrowed(row *store.Session, rules []policy.KeywordRule, models []*store.Model) map[string]bool {
	out := map[string]bool{}
	var narrowed []string
	if row == nil || !row.Narrowed || json.Unmarshal([]byte(row.NarrowedModels), &narrowed) != nil || len(narrowed) == 0 {
		return out
	}
	// A rule may name a model by its row id or by its provider's id.
	names := map[string][]string{}
	for _, m := range models {
		names[m.ID] = append(names[m.ID], m.ID)
		if lm, err := m.LLM(); err == nil {
			names[m.ID] = append(names[m.ID], lm.ID)
		}
	}
	for _, rule := range rules {
		permits := map[string]bool{}
		for _, id := range rule.ModelIDs {
			permits[id] = true
		}
		all := true
		for _, id := range narrowed {
			ok := permits[id]
			for _, alias := range names[id] {
				ok = ok || permits[alias]
			}
			all = all && ok
		}
		if all {
			out[rule.PolicyID] = true
		}
	}
	return out
}

// availableModels resolves what a user may run on. Routing policies assign
// models; an organization that has not written any routing policy that says
// who runs on what yet gets every enabled model, so a fresh backend is usable
// before its policy work begins.
//
// A keyword rule does not say that. It narrows a session when its word is
// seen, and until then it changes nothing — but it used to count as routing
// policy all the same, and the first one written left every developer with
// no model at all.
func (h *hub) availableModels(userID string) ([]*store.Model, error) {
	st := h.srv.store
	routing, err := st.Policies("", "model-routing")
	if err != nil {
		return nil, err
	}
	anyEnabled := false
	for _, p := range routing {
		var spec policy.ModelRoutingSpec
		if p.Enabled && json.Unmarshal(p.Spec, &spec) == nil && len(spec.Keywords) == 0 {
			anyEnabled = true
			break
		}
	}
	if anyEnabled {
		return st.ModelsForUser(userID)
	}
	all, err := st.Models()
	if err != nil {
		return nil, err
	}
	var out []*store.Model
	for _, m := range all {
		if m.Status == "enabled" {
			out = append(out, m)
		}
	}
	return out, nil
}

// build assembles the in-memory session: agentkit state from the database,
// the remote tools, the credential-injecting streamer, the trace writer.
// backendLimits is what this backend states on open. The effective ones are
// the smaller of these and the client's, worked out once per session.
var backendLimits = protocol.Limits{
	MaxResultBytes: 1 << 20, MaxConcurrentTools: 4, MaxAttachmentBytes: 8 << 20,
}

func (h *hub) build(user *store.User, row *store.Session, models []*store.Model, key []byte, stated protocol.Limits) (*hubSession, error) {
	current := models[0]
	for _, m := range models {
		if m.ID == row.ModelID {
			current = m
		}
	}
	llmModel, err := current.LLM()
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", current.ID, err)
	}

	limits := backendLimits.Negotiate(stated)
	hs := &hubSession{
		hub:     h,
		id:      row.ID,
		userID:  user.ID,
		email:   user.Email,
		models:  models,
		current: current,
		pending: map[string]chan *protocol.ToolResult{},
		blocked: map[string][]blockedRequest{},
		narrows: map[string]chan *protocol.ModelChoice{},
		limits:  limits,
		// One permit per call the client agreed to run at once. The client
		// enforces the limit too, because it is the side that owns the
		// sandbox — but a backend that sends more calls than it said it would
		// is a backend whose stated limit means nothing, and the extra ones
		// would only sit in a queue at the other end.
		slots: make(chan struct{}, max(limits.MaxConcurrentTools, 1)),

		traceKey: key,
	}
	hs.conn = protocol.NewConn(protocol.Options{OnDetach: func(error) { hs.park() }})

	// Per-session controls: replacements rewrite everything on its way to a
	// model and keep the alias table; keyword rules narrow the session when a
	// listed word is seen. Both live on the session, not the request, so
	// aliases stay stable across turns and a narrowing sticks.
	if specs, err := h.srv.replacementSpecs(user.ID); err == nil {
		hs.replacer, _ = policy.NewReplacer(specs)
	}
	hs.narrowedBy = map[string]bool{}
	if rules, err := h.srv.keywordRules(user.ID); err == nil {
		hs.router = policy.NewRouter(rules)
		hs.narrowedBy = alreadyNarrowed(row, rules, models)
	}

	streamer := &controlStreamer{base: h.srv.streamer(), hs: hs}

	// A batch with an edit or a command in it runs one call at a time, in the
	// order the model wrote them. Run together, "apt-get update" and then
	// "apt-get install" started in the same millisecond and the install found
	// no packages, and "npm run build" then "ls dist" listed the last build.
	// Reads alone still run together: nothing a read does can change another.
	inOrder := func(t agentkit.Tool) agentkit.Tool {
		t.Sequential = true
		return t
	}

	cfg := agentkit.Config{
		LLM:    streamer,
		Model:  llmModel,
		System: systemPrompt,
		Tools: []agentkit.Tool{
			hs.tool("read", "Read a file from the workspace. Returns its content.",
				`{"type":"object","properties":{"path":{"type":"string","description":"workspace-relative path"},"max_bytes":{"type":"integer","description":"truncate beyond this size"}},"required":["path"]}`),
			inOrder(hs.tool("edit", "Change the workspace: replace text in a file, write a whole file with content, or delete one.",
				`{"type":"object","properties":{"path":{"type":"string","description":"workspace-relative path"},"find":{"type":"string","description":"exact text to replace"},"replace":{"type":"string","description":"replacement for find"},"all":{"type":"boolean","description":"replace every occurrence"},"content":{"type":"string","description":"write the file with exactly this content"},"delete":{"type":"boolean","description":"delete the file"}},"required":["path"]}`)),
			inOrder(hs.tool("run", "Run a shell command in the workspace and return its exit code and output.",
				`{"type":"object","properties":{"cmd":{"type":"string","description":"the command"},"cwd":{"type":"string","description":"workspace-relative working directory"},"timeout_ms":{"type":"integer"}},"required":["cmd"]}`)),
		},
	}
	// The stored conversation, unsealed with the key the client returned. A
	// backend with no trace store has no key and stored it in the clear.
	state := row.State
	if trace.IsSealedState(state) {
		state = ""
		if hs.traceKey != nil {
			if plain, err := trace.OpenState(hs.traceKey, row.State); err == nil {
				state = string(plain)
			}
		}
	}
	if state != "" {
		var st agentkit.State
		if err := json.Unmarshal([]byte(state), &st); err == nil {
			// The stored model wins only if still available; the state's
			// history carries over regardless.
			st.Model = llmModel
			// The system prompt is john's instructions, not part of the
			// conversation, so a reopened session runs on today's. Resume
			// takes the one saved with the state, which left a conversation
			// begun before a deploy on the instructions from before it, for
			// as long as it was carried on.
			st.System = systemPrompt
			st.Messages, hs.cutOff = answerInterruptedCalls(st.Messages)
			hs.agent = agentkit.Resume(cfg, st)
		}
	}
	if hs.agent == nil {
		hs.agent = agentkit.New(cfg)
	}

	if w := h.srv.traceWriter(); w != nil && hs.traceKey != nil {
		hs.traces = w.Session(row.ID, row.StartedAt)
	}
	// A call cut off by the stop gets its end in the trace, as an
	// interrupted command does: without one it read as still running.
	for _, b := range hs.cutOff {
		hs.traceRecord("tool.result", map[string]any{"id": b.ID, "tool": b.Name, "status": "interrupted"})
	}
	return hs, nil
}

// interruptedCall is what the model is told about a tool call that was running
// when john stopped: what happened to it, and that what it did is unknown.
const interruptedCall = "interrupted: john stopped while this was running, so it did not finish " +
	"and its result was lost. Whatever it changed before it stopped is still there; check before " +
	"relying on it, and run it again if it is still needed."

// stoppedCall is what the model is told about a tool call the developer
// stopped with Esc: that they stopped it, not that it failed or timed out,
// and that it may have changed things before it did.
const stoppedCall = "interrupted: the developer pressed Esc while this was running, so it was " +
	"stopped and its output was discarded. Whatever it changed before it stopped is still there. " +
	"Don't run it again unless they ask."

// answerInterruptedCalls gives every tool call in a stored conversation that
// never got a result one saying it was interrupted. A conversation whose last
// word is a request for tools is one no model will carry on, and that is
// exactly the one a stop mid-command leaves — `npm install` running when john
// quit. Resuming it used to announce it was carrying on and then end the turn
// as lost. Answered, the turn carries on, and the model knows what happened.
// The calls it answered are returned, so the trace and the developer can be
// told of them too.
func answerInterruptedCalls(msgs []llm.Message) ([]llm.Message, []llm.Block) {
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role == llm.RoleToolResult {
			answered[m.ToolCallID] = true
		}
	}
	out := make([]llm.Message, 0, len(msgs))
	var owed []llm.Block // calls of the last assistant message still unanswered
	var cut []llm.Block  // every call answered here
	flush := func() {
		for _, b := range owed {
			out = append(out, llm.ToolResultMessage(b.ID, b.Name, true, llm.TextBlock(interruptedCall)))
		}
		cut = append(cut, owed...)
		owed = nil
	}
	for _, m := range msgs {
		// The results of a message's calls follow it; the owed ones go after
		// them, before anything else does.
		if m.Role != llm.RoleToolResult && m.Kind == "" {
			flush()
		}
		out = append(out, m)
		if m.Role == llm.RoleAssistant {
			for _, b := range m.Blocks {
				if b.Type == llm.BlockToolCall && !answered[b.ID] {
					owed = append(owed, b)
				}
			}
		}
	}
	flush()
	return out, cut
}

// controlStreamer is where every byte on its way to a provider passes through
// the deterministic controls, immediately before it leaves. It rewrites
// content with the session's replacements, injects the admin-configured key
// for the target model, and — the order is the whole control — refuses to send
// at all when a routing keyword is seen, so the hub can narrow the session
// first. Narrowing after the content had reached a model would be a record of
// a leak rather than a defence against one.
type controlStreamer struct {
	base llm.Streamer
	hs   *hubSession
}

// errNarrow signals the hub that a keyword was seen and the request was held.
type errNarrow struct{ n *policy.Narrowing }

func (e errNarrow) Error() string { return "narrowed: keyword " + e.n.Keyword }

// errorStream returns a stream that fails immediately with err — how the
// control streamer holds a request rather than sending it.
func errorStream(model llm.Model, err error) *llm.Stream {
	return llm.NewStream(model, func(emit func(llm.Event) bool) {
		emit(llm.ErrorEvent(err, llm.Usage{}))
	})
}

func (c *controlStreamer) Stream(ctx context.Context, req llm.Request) *llm.Stream {
	hs := c.hs

	// Scan the outbound content for a routing keyword before anything else.
	// The router and the models are read under the lock: a policy change
	// replaces them while a turn runs.
	hs.mu.Lock()
	router, models := hs.router, hs.models
	already := map[string]bool{}
	for k, v := range hs.narrowedBy {
		already[k] = v
	}
	hs.mu.Unlock()
	if router != nil && router.HasRules() {
		if n := router.Match(outboundText(req), already); n != nil {
			return errorStream(req.Model, errNarrow{n: n})
		}
	}

	// Apply replacements to everything the request carries. The client never
	// sees this happen and has nothing to opt out of, which is the property
	// that makes it worth anything.
	if hs.replacer != nil && hs.replacer.Active() {
		req.System = hs.replacer.Apply(req.System)
		req.Messages = rewriteMessages(req.Messages, hs.replacer.Apply)
	}

	if req.APIKey == "" {
		for _, m := range models {
			if lm, err := m.LLM(); err == nil && lm.ID == req.Model.ID && m.APIKey != "" {
				req.APIKey = m.APIKey
				break
			}
		}
	}
	return c.base.Stream(ctx, req)
}

// outboundText flattens everything a request would send to a model into one
// string for keyword scanning: the system prompt and every text block.
func outboundText(req llm.Request) string {
	var b strings.Builder
	b.WriteString(req.System)
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			if blk.Type == llm.BlockText {
				b.WriteByte('\n')
				b.WriteString(blk.Text)
			}
		}
	}
	return b.String()
}

// rewriteMessages returns a copy of msgs with fn applied to every text block,
// never mutating the caller's slice (R5).
func rewriteMessages(msgs []llm.Message, fn func(string) string) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		m.Blocks = append([]llm.Block(nil), m.Blocks...)
		for j, blk := range m.Blocks {
			if blk.Type == llm.BlockText {
				m.Blocks[j].Text = fn(blk.Text)
			}
		}
		out[i] = m
	}
	return out
}

// hubSession is one live conversation.
type hubSession struct {
	hub     *hub
	id      string
	userID  string
	email   string
	conn    *protocol.Conn
	agent   *agentkit.Session
	models  []*store.Model
	current *store.Model

	// traces is this session's file, and traceKey the key it is written
	// with. The key lives here and nowhere else: not on disk, not in the
	// database, and gone the moment this session leaves memory.
	traces   *trace.Session
	traceKey []byte

	replacer *policy.Replacer
	router   *policy.Router

	// limits are the ones in force for this session — the smaller of what
	// each side stated — and slots is how the outstanding-call half of them
	// is kept.
	limits protocol.Limits
	slots  chan struct{}

	mu         sync.Mutex
	leaving    bool // the backend is stopping: a running turn is kept, not ended
	stopped    bool // the developer pressed Esc during the running turn
	pending    map[string]chan *protocol.ToolResult
	blocked    map[string][]blockedRequest // run calls in flight → what the proxy refused meanwhile
	narrows    map[string]chan *protocol.ModelChoice
	narrowedBy map[string]bool // routing policy ids that have already narrowed
	cutOff     []llm.Block     // tool calls a stop cut off, answered as interrupted when the session was rebuilt
	turnID     string
	turnCancel context.CancelFunc
	turnDone   chan struct{} // closed when the running turn's goroutine returns
	parkTimer  *time.Timer
	closed     bool
}

// controls is the session's keyword router and models, which a policy change
// replaces while turns run.
func (hs *hubSession) controls() (*policy.Router, []*store.Model) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.router, hs.models
}

// reloadControls reads the session's routing and replacement policies, and
// the models it may run on, again. They were read once, when the session
// opened: a keyword rule written while it was open was not enforced in it,
// while the screen said "policies updated". What has already narrowed the
// session stays narrowed, and the aliases already minted stay the same.
func (hs *hubSession) reloadControls() {
	h := hs.hub
	if specs, err := h.srv.replacementSpecs(hs.userID); err == nil {
		hs.mu.Lock()
		if hs.replacer == nil {
			hs.replacer, _ = policy.NewReplacer(specs)
		} else {
			hs.replacer.SetRules(specs)
		}
		hs.mu.Unlock()
	}
	rules, rerr := h.srv.keywordRules(hs.userID)
	models, merr := h.availableModels(hs.userID)
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if rerr == nil {
		hs.router = policy.NewRouter(rules)
	}
	if merr == nil && len(models) > 0 {
		hs.models = models
	}
}

func (hs *hubSession) readyMessage(ack uint64) *protocol.SessionReady {
	var infos []protocol.ModelInfo
	models, narrowed := hs.selectable()
	for _, m := range models {
		infos = append(infos, modelInfo(m))
	}
	epoch, _ := hs.hub.srv.store.PolicyEpoch()
	hs.mu.Lock()
	current := hs.current
	hs.mu.Unlock()
	started := ""
	if row, err := hs.hub.srv.store.SessionByID(hs.id); err == nil {
		started = row.StartedAt.UTC().Format(time.RFC3339)
	}
	used, window := hs.contextUsed(current)
	return &protocol.SessionReady{
		Session:       hs.id,
		StartedAt:     started,
		Context:       used,
		ContextWindow: window,
		PolicyEpoch:   epoch,
		Model:         modelInfo(current),
		Narrowed:      narrowed,
		Models:        infos,
		Limits:        hs.limits,
		ResumableForS: int(resumableFor / time.Second),
		Ack:           ack,
		TurnsSoFar:    hs.turnsSoFar(),
	}
}

// contextUsed is how much of the model's context the conversation fills, as
// its latest model call reported it, and how much there is.
func (hs *hubSession) contextUsed(m *store.Model) (used, window int) {
	if lm, err := m.LLM(); err == nil {
		window = lm.ContextWindow
	}
	if hs.agent == nil {
		return 0, window
	}
	msgs := hs.agent.State().Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if u := msgs[i].Usage; u != nil {
			return u.Input + u.CacheRead + u.CacheWrite + u.Output, window
		}
	}
	return 0, window
}

func modelInfo(m *store.Model) protocol.ModelInfo {
	return protocol.ModelInfo{ID: m.ID, Name: m.Name, Internal: m.Hosting == "internal"}
}

// selectable is what the session may be moved to: the models the developer's
// groups allow, less anything a routing policy has narrowed it away from. It
// says too whether the session is narrowed.
func (hs *hubSession) selectable() ([]*store.Model, bool) {
	_, models := hs.controls()
	row, err := hs.hub.srv.store.SessionByID(hs.id)
	if err != nil || row == nil || !row.Narrowed {
		return models, false
	}
	var keep []string
	if json.Unmarshal([]byte(row.NarrowedModels), &keep) != nil || len(keep) == 0 {
		return models, true
	}
	var out []*store.Model
	for _, m := range models {
		if slices.Contains(keep, m.ID) {
			out = append(out, m)
		}
	}
	return out, true
}

// onModelSelect is the developer's /model. The client only asks; whether the
// session moves is decided here, against the same policy that decided what
// it started on, so /model cannot reach a model the developer's groups do not
// allow or a narrowing ruled out. It answers on the run loop, which is also
// where a prompt starts a turn, so a turn cannot begin halfway through it.
func (hs *hubSession) onModelSelect(m *protocol.ModelSelect) {
	reply := func(r *protocol.ModelSelected) {
		r.ID = m.ID
		hs.conn.Send(context.Background(), r)
	}
	hs.mu.Lock()
	running := hs.turnID != ""
	hs.mu.Unlock()
	if running {
		reply(&protocol.ModelSelected{Error: "a turn is running · switch when it ends, or press Esc to stop it"})
		return
	}

	models, narrowed := hs.selectable()
	var chosen *store.Model
	for _, mo := range models {
		if mo.ID == m.Model {
			chosen = mo
		}
	}
	if chosen == nil {
		why := "not a model your policy groups allow"
		if narrowed {
			why = "not a model this session may use: a routing policy narrowed it"
		}
		reply(&protocol.ModelSelected{Error: why})
		return
	}
	lm, err := chosen.LLM()
	if err != nil {
		reply(&protocol.ModelSelected{Error: "that model is misconfigured: " + err.Error()})
		return
	}

	hs.mu.Lock()
	hs.current = chosen
	hs.mu.Unlock()
	hs.agent.SetModel(lm)
	hs.saveState(chosen.ID)
	hs.traceRecord("model.select", map[string]any{"model": chosen.ID})
	reply(&protocol.ModelSelected{Model: modelInfo(chosen)})
}

// turnsSoFar is the highest turn number the session has started. Turn ids
// are the client's, and a client carrying on a session — after a restart, or
// with /resume — started again at t1, so the same session had two t1s in its
// trace and two turns the backend could not tell apart.
func (hs *hubSession) turnsSoFar() int {
	n := 0
	if row, err := hs.hub.srv.store.SessionByID(hs.id); err == nil {
		n = max(row.LastTurn, row.Turns)
	}
	hs.mu.Lock()
	var running int
	if _, err := fmt.Sscanf(hs.turnID, "t%d", &running); err == nil && running > n {
		n = running
	}
	hs.mu.Unlock()
	return n
}

// run is the receive loop: everything the client sends lands here, in order.
func (hs *hubSession) run() {
	for msg := range hs.conn.In() {
		switch m := msg.(type) {
		case *protocol.Prompt:
			hs.onPrompt(m)
		case *protocol.Interrupt:
			hs.mu.Lock()
			hs.stopped = true
			hs.mu.Unlock()
			hs.agent.Interrupt()
		case *protocol.ToolResult:
			hs.mu.Lock()
			ch := hs.pending[m.ID]
			delete(hs.pending, m.ID)
			hs.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case *protocol.ModelChoice:
			hs.mu.Lock()
			ch := hs.narrows[m.ID]
			delete(hs.narrows, m.ID)
			hs.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case *protocol.ModelSelect:
			hs.onModelSelect(m)
		case *protocol.ProxyDecision:
			hs.onProxyDecision(m)
		case *protocol.SandboxState:
			hs.traceRecord("sandbox.state", m)
		case *protocol.SessionClose:
			hs.conn.Send(context.Background(), &protocol.SessionClosed{Reason: "closed by client"})
			hs.shutdown("closed by client")
			return
		}
	}
}

func (hs *hubSession) onPrompt(m *protocol.Prompt) {
	msg, attached := hs.promptMessage(m)
	hs.traceRecord("prompt", map[string]any{"turn": m.Turn, "text": m.Text, "attachments": attached})
	hs.mu.Lock()
	running := hs.turnCancel != nil
	if !running {
		ctx, cancel := context.WithCancel(context.Background())
		hs.turnID = m.Turn
		hs.stopped = false
		hs.turnCancel = cancel
		// Recorded before the turn is started, not after: a backend that dies
		// in between must come back knowing there was a turn, and the cost of
		// being wrong the other way is one resumed turn that had not begun.
		hs.hub.srv.store.SetSessionTurn(hs.id, m.Turn)
		done := make(chan struct{})
		hs.turnDone = done
		hs.goTurn(ctx, done, m.Turn, &msg)
	}
	hs.mu.Unlock()
	if running {
		// The UI queues prompts while a turn runs, so a prompt arriving
		// mid-turn is deliberate steering.
		hs.agent.SendMessage(msg)
	}
}

// attachedFile is what the trace records about one attachment: enough to say
// what reached a model and to recognise the same file again, and not the file
// itself. A trace that carried the bytes would be a second copy of every
// screenshot anyone ever pasted, and the trace's job is to say what happened.
type attachedFile struct {
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// attachmentTypes is what the backend will pass to a model, whatever a client
// says the bytes are. The client has its own list and they agree; this one is
// the one that decides, because the client is the half a developer can edit.
var attachmentTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	"application/pdf": true,
}

// promptMessage turns a prompt into the message the loop runs on.
//
// This is the one place a file reaches a model, so it is where the two things
// that could make that a mistake are decided: whether the organization allows
// it at all, and whether this model can read it. Everything refused is said
// out loud — a prompt that quietly went without its screenshot looks to a
// developer like a model that ignored it.
func (hs *hubSession) promptMessage(m *protocol.Prompt) (llm.Message, []attachedFile) {
	blocks := []llm.Block{llm.TextBlock(m.Text)}
	if len(m.Attachments) == 0 {
		return llm.UserBlocks(blocks...), nil
	}

	say := func(text string) {
		hs.conn.Send(context.Background(), &protocol.Notice{Level: "warn", Text: text})
	}
	if !hs.attachmentsAllowed() {
		say("attachments are not allowed here · the prompt was sent as text")
		return llm.UserBlocks(blocks...), nil
	}

	lm, _ := hs.current.LLM()
	limit := hs.limits.MaxAttachmentBytes
	total := 0
	var attached []attachedFile
	for _, att := range m.Attachments {
		raw, err := base64.StdEncoding.DecodeString(att.Data)
		if err != nil {
			say("could not read the attachment " + att.Name + " · the prompt was sent without it")
			continue
		}
		total += len(raw)
		if limit > 0 && total > limit {
			say(fmt.Sprintf("%s is past the %d MB a prompt may carry · the prompt was sent without it",
				att.Name, limit>>20))
			continue
		}
		if !attachmentTypes[att.MimeType] {
			say(att.Name + " is not a kind of file john sends to a model · the prompt was sent without it")
			continue
		}

		// A model that cannot read it is told a file was left out, by
		// agentkit, rather than handed bytes it will reject — and the
		// developer is told which model, because the answer is to switch.
		switch {
		case att.MimeType == "application/pdf":
			if !lm.Documents {
				say(hs.current.Name + " does not read documents · " + att.Name + " was named to it, not shown")
			}
			blocks = append(blocks, llm.DocumentBlock(att.MimeType, att.Name, raw))
		default:
			if !lm.Vision {
				say(hs.current.Name + " does not see images · " + att.Name + " was named to it, not shown")
			}
			blocks = append(blocks, llm.ImageBlock(att.MimeType, raw))
		}
		sum := sha256.Sum256(raw)
		attached = append(attached, attachedFile{
			Name: att.Name, Mime: att.MimeType, Bytes: len(raw),
			SHA256: hex.EncodeToString(sum[:]),
		})
	}
	return llm.UserBlocks(blocks...), attached
}

// attachmentsAllowed answers the organization's attachment policy for this
// session. The backend is the side that can answer it completely: the client
// knows whether replacements are active, because the snapshot says so, but
// routing keywords never leave this process — and a keyword is no more
// readable inside a PNG than a replacement is.
func (hs *hubSession) attachmentsAllowed() bool {
	org, err := hs.hub.srv.store.Org()
	if err != nil {
		return false // fail closed: we cannot say what this organization allows
	}
	router, _ := hs.controls()
	underTextControl := (hs.replacer != nil && hs.replacer.Active()) ||
		(router != nil && router.HasRules())
	return (&policy.Snapshot{Attach: org.Attachments}).AttachmentsAllowed(underTextControl)
}

// A run may be held mid-flight by a routing keyword; the turn then negotiates
// a narrower model with the developer and continues, so narrowing is part of
// a turn rather than a failure of one.
// runTurn drives one agentkit run. prompt is the message that starts it, or
// nil to carry on from what the conversation already holds — which is how a
// turn that was in flight when the backend stopped is picked up again.
func (hs *hubSession) runTurn(ctx context.Context, turn string, prompt *llm.Message) {
	send := func(m protocol.Message) { hs.conn.Send(context.Background(), m) }
	send(&protocol.TurnBegin{Turn: turn})

	var thinking []byte
	var usageIn, usageOut int
	window := 0 // the routed model's context, for the status line
	if lm, err := hs.current.LLM(); err == nil {
		window = lm.ContextWindow
	}
	var usageCost float64

	msgID := func(index int) string { return fmt.Sprintf("%s.%d", turn, index) }

	consume := func(events func(func(agentkit.Event) bool)) (reason string, narrow *policy.Narrowing) {
		reason = "completed"
		for ev := range events {
			switch ev.Type {
			case agentkit.EventModel:
				s := ev.Stream
				switch s.Type {
				case llm.EventTextStart:
					send(&protocol.MessageStart{ID: msgID(s.Index)})
				case llm.EventTextDelta:
					send(&protocol.MessageDelta{ID: msgID(s.Index), Text: s.Delta})
				case llm.EventTextEnd:
					send(&protocol.MessageEnd{ID: msgID(s.Index)})
				case llm.EventThinkingDelta:
					thinking = append(thinking, s.Delta...)
					send(&protocol.ThinkingDelta{Turn: turn, Text: string(thinking)})
				}

			case agentkit.EventMessage:
				hs.saveState(hs.current.ID)
				msg := ev.Message
				if msg.Role == llm.RoleAssistant {
					var body []byte
					calls := false
					for _, b := range msg.Blocks {
						switch b.Type {
						case llm.BlockText:
							body = append(body, b.Text...)
						case llm.BlockToolCall:
							calls = true
						}
					}
					// A message with nothing in it said nothing: it is what a
					// request held by a keyword, before it reached any model,
					// leaves behind.
					if len(body) > 0 || calls {
						hs.traceRecord("assistant.message", map[string]any{"turn": turn, "text": string(body)})
					}
				}
				if msg.Usage != nil {
					u := msg.Usage
					usageIn += u.Input + u.CacheRead + u.CacheWrite
					usageOut += u.Output
					usageCost += u.TotalCost
					hs.hub.srv.store.AddUsage(time.Now(), hs.userID, hs.current.ID, store.UsageDelta{
						Requests: 1, TokensIn: u.Input + u.CacheWrite, TokensOut: u.Output,
						TokensCached: u.CacheRead, CostUSD: u.TotalCost,
					})
					send(&protocol.Usage{Turn: turn, TokensIn: usageIn, TokensOut: usageOut, CostUSD: usageCost,
						Context: u.Input + u.CacheRead + u.CacheWrite + u.Output, ContextWindow: window})
				}

			case agentkit.EventRunEnd:
				var en errNarrow
				switch {
				case ev.Err == nil:
					reason = "completed"
				case errors.As(ev.Err, &en):
					reason, narrow = "narrow", en.n
				case errors.Is(ev.Err, agentkit.ErrNothingToDo):
					// Only reachable on a resumed turn: the conversation's
					// last word is an assistant message that had already
					// asked for tools, which agentkit will not carry on from.
					// The turn is over and the developer is the one who can
					// do something about it.
					reason = "lost"
				case errors.Is(ev.Err, agentkit.ErrInterrupted), errors.Is(ev.Err, context.Canceled):
					reason = "interrupted"
				default:
					reason = "failed"
					send(&protocol.Error{Code: protocol.ErrInternal, Message: ev.Err.Error()})
				}
			}
		}
		return reason, narrow
	}

	start := hs.agent.Continue
	if prompt != nil {
		start = func(ctx context.Context) iter.Seq[agentkit.Event] {
			return hs.agent.RunMessage(ctx, *prompt)
		}
	}
	reason, narrow := consume(start(ctx))
	// A keyword was seen and the request was held before it reached a model.
	// Ask which of the permitted models to continue on, switch, and resume;
	// the session is narrowed for good.
	for reason == "narrow" && narrow != nil {
		if !hs.negotiateNarrow(ctx, turn, narrow) {
			reason = "aborted"
			break
		}
		reason, narrow = consume(hs.agent.Continue(ctx))
	}

	hs.mu.Lock()
	if hs.turnID == turn {
		hs.turnID = ""
		if hs.turnCancel != nil {
			hs.turnCancel()
			hs.turnCancel = nil
		}
	}
	hs.mu.Unlock()

	// A turn the backend interrupted on its way out is still this session's
	// turn: leaving the marker in place is the whole of what makes it
	// resumable, and clearing it here would turn every deploy into a lost
	// turn again. Every other ending — completed, failed, the developer's own
	// interrupt, a park window that ran out — is an ending.
	hs.mu.Lock()
	leaving := hs.leaving
	hs.mu.Unlock()
	if !leaving {
		hs.hub.srv.store.SetSessionTurn(hs.id, "")
	}
	hs.hub.srv.store.AddSessionUsage(hs.id, 1, usageIn, usageOut, usageCost)
	if leaving {
		// Not an ending, so the client is not told of one: the next backend
		// picks this turn up, and a developer shown "interrupted" would
		// believe it was over.
		hs.traceRecord("turn.suspend", map[string]any{"turn": turn})
		return
	}
	hs.traceRecord("turn.end", map[string]any{"turn": turn, "reason": reason})
	send(&protocol.TurnEnd{Turn: turn, Reason: reason,
		Usage: &protocol.Usage{Turn: turn, TokensIn: usageIn, TokensOut: usageOut, CostUSD: usageCost}})
}

// resumeTurn picks up a turn that was running when this session last left
// memory — a backend that was restarted or that crashed mid-turn.
//
// Nothing has to be replayed to do it. The conversation is written to the
// database at every message boundary, so what comes back is a history whose
// last entry is a prompt with no answer, a tool result with no reply, or an
// assistant message that ended abnormally — and agentkit carries on from any
// of those. What was missing was only the knowledge that anyone was waiting,
// which is what the session's turn id is.
//
// Two things it deliberately does not do. It does not re-issue tool calls the
// old process had outstanding: they are answered as interrupted when the
// conversation is loaded (answerInterruptedCalls), so the model is told the
// work stopped and decides what to do about it, rather than a `run` command
// being executed a second time by a backend nobody asked. agentkit does not
// do that itself — it carries on from an unanswered call only inside a run
// already going — which is why a stop mid-command used to end the turn as
// lost. And it does not resume silently — the developer's
// client was showing a turn that no longer existed, and the first thing they
// should see is that it is running again.
func (hs *hubSession) resumeTurn(turn string) {
	hs.mu.Lock()
	if hs.turnCancel != nil {
		hs.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	hs.turnID = turn
	hs.stopped = false
	hs.turnCancel = cancel
	done := make(chan struct{})
	hs.turnDone = done
	hs.mu.Unlock()

	hs.traceRecord("turn.resume", map[string]any{"turn": turn})
	hs.conn.Send(context.Background(), &protocol.Notice{Level: "info",
		Text: "carrying on the turn that was running when the backend stopped"})
	// What the stop cut off is said here, where the turn picks up: the
	// screen that showed it running went with the process that stopped.
	hs.mu.Lock()
	cut := hs.cutOff
	hs.cutOff = nil
	hs.mu.Unlock()
	for _, b := range cut {
		hs.conn.Send(context.Background(), &protocol.Notice{Level: "warn",
			Text: cutOffNotice(b) + " was cut off when john stopped · its result was lost, and the model is told so"})
	}
	hs.goTurn(ctx, done, turn, nil)
}

// cutOffNotice names a cut-off call the way the screen named it: the command
// for a run, the path for a file tool.
func cutOffNotice(b llm.Block) string {
	var args struct {
		Cmd  string `json:"cmd"`
		Path string `json:"path"`
	}
	json.Unmarshal(b.Args, &args)
	switch {
	case args.Cmd != "":
		return b.Name + " `" + args.Cmd + "`"
	case args.Path != "":
		return b.Name + " " + args.Path
	}
	return b.Name
}

// goTurn runs a turn on its own goroutine and closes done when it returns, so
// a stopping backend can wait for the turn's last checkpoint.
func (hs *hubSession) goTurn(ctx context.Context, done chan struct{}, turn string, prompt *llm.Message) {
	go func() {
		defer close(done)
		hs.runTurn(ctx, turn, prompt)
	}()
}

// negotiateNarrow runs the model-narrowing dialogue: it holds the request,
// asks the developer which of the permitted models to continue on, records
// the choice, and switches the session's model. It returns false when the
// developer declines — there is nothing safe to fall back to, because the
// keyword has been seen and that cannot be taken back.
func (hs *hubSession) negotiateNarrow(ctx context.Context, turn string, n *policy.Narrowing) bool {
	// The permitted set, resolved to the models this session actually has.
	permit := map[string]bool{}
	for _, id := range n.ModelIDs {
		permit[id] = true
	}
	var infos []protocol.ModelInfo
	var permitted []*store.Model
	_, have := hs.controls()
	for _, m := range have {
		lm, err := m.LLM()
		if err != nil {
			continue
		}
		if permit[m.ID] || permit[lm.ID] {
			permitted = append(permitted, m)
			infos = append(infos, protocol.ModelInfo{ID: m.ID, Name: m.Name, Internal: m.Hosting == "internal"})
		}
	}
	if len(permitted) == 0 {
		hs.conn.Send(context.Background(), &protocol.Error{Code: protocol.ErrNoModel,
			Message: "a routing policy narrowed this session to models you do not have"})
		return false
	}

	id := fmt.Sprintf("%s.narrow.%s", turn, n.PolicyID)
	reply := make(chan *protocol.ModelChoice, 1)
	hs.mu.Lock()
	hs.narrows[id] = reply
	hs.mu.Unlock()

	hs.traceRecord("model.narrow", map[string]any{"turn": turn, "policy": n.PolicyID, "keyword": n.Keyword})
	hs.conn.Send(context.Background(), &protocol.ModelNarrow{
		ID: id, Turn: turn,
		Reason: fmt.Sprintf("keyword %q seen in what was about to reach a model", n.Keyword),
		Policy: "narrowed by policy group " + n.Group,
		Models: infos,
	})

	var choice *protocol.ModelChoice
	select {
	case choice = <-reply:
	case <-ctx.Done():
		return false
	}
	if choice.Cancelled {
		return false
	}

	// Switch to the chosen model, mark the session narrowed by this policy so
	// it never fires again, and persist the narrowing.
	chosen := permitted[0]
	for _, m := range permitted {
		if m.ID == choice.Model {
			chosen = m
		}
	}
	lm, err := chosen.LLM()
	if err != nil {
		return false
	}
	hs.mu.Lock()
	hs.current = chosen
	hs.narrowedBy[n.PolicyID] = true
	var names []string
	for _, m := range permitted {
		names = append(names, m.ID)
	}
	hs.mu.Unlock()
	hs.agent.SetModel(lm)

	models, _ := json.Marshal(names)
	hs.hub.srv.store.SetSessionNarrowed(hs.id, string(models))
	hs.saveState(chosen.ID)
	hs.traceRecord("model.choice", map[string]any{"turn": turn, "model": chosen.ID})
	// The client's own line says so, where the choice was made; a notice
	// here said it a second time.
	return true
}

// saveState writes the conversation to the sessions table, sealed with the
// session key. The backend can write this at every message boundary and read
// it back only for as long as it is holding the key; after that the row is
// opaque to it, and a client returns the key on the next open.
//
// A session with no key — no trace store is configured — writes the state in
// the clear, which is what every session did before this.
func (hs *hubSession) saveState(modelID string) {
	state, err := json.Marshal(hs.agent.State())
	if err != nil {
		return
	}
	stored := string(state)
	if hs.traceKey != nil {
		sealed, err := trace.SealState(hs.traceKey, state)
		if err != nil {
			// Writing the conversation in the clear because sealing failed is
			// exactly the trade this design refuses. Drop the checkpoint; the
			// turn still runs, and the trace still records it.
			return
		}
		stored = sealed
	}
	hs.hub.srv.store.SaveSessionState(hs.id, stored, modelID)
}

// tool builds one of the three remote tools: Execute sends the call down the
// wire and blocks until the client's result comes back. The loop neither
// knows nor cares that execution is remote.
func (hs *hubSession) tool(name, description, schema string) agentkit.Tool {
	return agentkit.Tool{
		Name:        name,
		Description: description,
		Schema:      json.RawMessage(schema),
		Execute: func(ctx context.Context, call agentkit.ToolCall) (agentkit.ToolResult, error) {
			// An edit whose arguments carry a replacement alias is refused
			// before it reaches the client. The model only ever saw the
			// alias; letting it write against one would substitute the real
			// secret back into a file on the strength of the model's output —
			// a write primitive built out of a redaction feature. Safe, and
			// it turns a file containing a matched value into one the agent
			// cannot edit, which is the correct trade for v1.
			if name == "edit" && hs.replacer != nil {
				if alias, found := hs.replacer.ContainsAlias(string(call.Args)); found {
					return agentkit.ToolResult{}, fmt.Errorf(
						"blocked: this edit touches redacted content (%s); it cannot be edited through the agent", alias)
				}
			}

			ch := make(chan *protocol.ToolResult, 1)
			hs.mu.Lock()
			hs.pending[call.ID] = ch
			if name == "run" {
				hs.blocked[call.ID] = nil
			}
			hs.mu.Unlock()
			defer func() {
				hs.mu.Lock()
				delete(hs.pending, call.ID)
				delete(hs.blocked, call.ID)
				hs.mu.Unlock()
			}()

			// The loop runs a batch of calls at once; the session sends at
			// most as many of them as the two ends agreed on, and the rest
			// wait here for one to come back.
			select {
			case hs.slots <- struct{}{}:
				defer func() { <-hs.slots }()
			case <-ctx.Done():
				return agentkit.ToolResult{}, ctx.Err()
			}

			hs.traceRecord("tool.call", map[string]any{"id": call.ID, "tool": name, "args": json.RawMessage(call.Args)})
			if err := hs.conn.Send(ctx, &protocol.ToolCall{ID: call.ID, Tool: name, Args: call.Args}); err != nil {
				return agentkit.ToolResult{}, err
			}
			select {
			case res := <-ch:
				hs.traceRecord("tool.result", toolResultRecord(call.ID, name, res))
				hs.mu.Lock()
				blocked := hs.blocked[call.ID]
				hs.mu.Unlock()
				return mapToolResult(name, res, blocked)
			case <-ctx.Done():
				hs.conn.Send(context.Background(), &protocol.ToolCancel{ID: call.ID, Reason: "interrupted"})
				// The call has an end in the trace like any other: without
				// one, an interrupted command read as one still running.
				hs.traceRecord("tool.result", map[string]any{"id": call.ID, "tool": name, "status": "cancelled"})
				// What the model is told is why it stopped. The bare context
				// error was "context canceled", and after Esc on `sleep 300`
				// the model told the developer the command had timed out.
				hs.mu.Lock()
				byDeveloper := hs.stopped && !hs.leaving
				hs.mu.Unlock()
				if byDeveloper {
					return agentkit.ToolResult{}, errors.New(stoppedCall)
				}
				return agentkit.ToolResult{}, errors.New(interruptedCall)
			}
		},
	}
}

// traceOutputTail is how much of a tool's stdout and stderr the trace keeps:
// the end, where a failure says what it was.
const traceOutputTail = 4 << 10

// toolResultRecord is what the trace keeps of a tool result. The output's tail
// goes in: without it a failed command was "exit 1" and nothing else, and
// why it failed lived only in what the model was told. The exit code is a
// command's, so only run has one.
func toolResultRecord(id, tool string, res *protocol.ToolResult) map[string]any {
	rec := map[string]any{"id": id, "status": res.Status}
	if tool == "run" {
		rec["exit"] = res.Exit
	}
	if s := tailString(res.Stdout, traceOutputTail); s != "" {
		rec["stdout"] = s
	}
	if s := tailString(res.Stderr, traceOutputTail); s != "" {
		rec["stderr"] = s
	}
	return rec
}

// tailString is the last n bytes of s, starting on a whole character.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for i, r := range s {
		if r != utf8.RuneError {
			return "…" + s[i:]
		}
	}
	return ""
}

// mapToolResult turns the wire result into what the model sees. blocked is
// what john's proxy refused while a run was going.
func mapToolResult(tool string, res *protocol.ToolResult, blocked []blockedRequest) (agentkit.ToolResult, error) {
	switch res.Status {
	case "blocked":
		policy := res.Policy
		if policy == "" {
			policy = "a policy"
		}
		return agentkit.ToolResult{}, fmt.Errorf("blocked by %s: %s", policy, res.Stderr)
	case "cancelled":
		// The client cancels a call only when the turn is interrupted.
		return agentkit.ToolResult{}, errors.New(stoppedCall)
	}
	switch tool {
	case "read":
		if res.Status == "failed" {
			return agentkit.ToolResult{}, errors.New(firstNonEmpty(res.Stderr, "read failed"))
		}
		out := agentkit.Text("%s", res.Content)
		if res.Truncated {
			out = agentkit.Text("%s\n[truncated]", res.Content)
		}
		return out, nil
	case "edit":
		if res.Status == "failed" {
			return agentkit.ToolResult{}, errors.New(firstNonEmpty(res.Stderr, "edit failed"))
		}
		return agentkit.Text("ok"), nil
	default: // run — a non-zero exit is the answer, not an error
		body := fmt.Sprintf("exit %d\n%s", res.Exit, res.Stdout)
		if res.Stderr != "" {
			body += "\nstderr:\n" + res.Stderr
		}
		if res.Truncated {
			body += "\n[output truncated]"
		}
		body += blockedNote(blocked)
		return agentkit.Text("%s", body), nil
	}
}

// blockedRequestsShown is how many refused requests a result names; the
// rest are counted.
const blockedRequestsShown = 5

// blockedNote tells the model what john's proxy refused during a command. A
// tool reports a refusal its own way — apt as "403 Forbidden", and npm not at
// all when only its audit was refused, exiting 0 — so without this the model
// could not tell a network policy from a broken mirror, or know there was
// anything to mention.
//
// Each request carries why it was refused, because the reasons call for
// opposite answers: a host no policy covers is one the developer can ask
// for, and one whose access request was rejected is one they were told no
// about. Every refusal used to be "no network policy covers it … the
// developer can approve access", so after REQ-103 was rejected the model said
// the policy still did not cover example.org, as if nobody had decided.
func blockedNote(blocked []blockedRequest) string {
	if len(blocked) == 0 {
		return ""
	}
	var b strings.Builder
	if len(blocked) == 1 {
		b.WriteString("\n\njohn's network proxy blocked 1 request during this command:")
	} else {
		fmt.Fprintf(&b, "\n\njohn's network proxy blocked %d requests during this command:", len(blocked))
	}
	uncovered, refused := false, false
	for i, r := range blocked {
		if r.Refused == "" {
			uncovered = true
		} else {
			refused = true
		}
		if i >= blockedRequestsShown {
			continue
		}
		why := "no network policy covers it"
		if r.Refused != "" {
			why = "refused: " + r.Refused
		}
		b.WriteString("\n- " + r.Req + " (" + why + ")")
	}
	if len(blocked) > blockedRequestsShown {
		fmt.Fprintf(&b, "\n- and %d more", len(blocked)-blockedRequestsShown)
	}
	if uncovered {
		b.WriteString("\nThe developer is shown what john blocks and can ask for access to what no policy covers.")
	}
	if refused {
		b.WriteString("\nWhat was refused has already been decided: don't ask for it again.")
	}
	b.WriteString("\nTell them what was blocked and why rather than trying to get around the policy.")
	return b.String()
}

// blockedRequest is one request the proxy refused during a run, and why.
// Refused is empty when no policy covers it, and otherwise says who decided
// against it.
type blockedRequest struct {
	Req     string
	Refused string
}

// refusedBecause is why a decision refused a request that some policy or
// person did decide, or "" when nothing covered it.
func refusedBecause(m *protocol.ProxyDecision) string {
	switch {
	case m.Rejected != "":
		return "access request " + m.Rejected + " for this host was rejected"
	case m.Verdict == "asked":
		return "the developer turned it down when asked"
	case m.Verdict == "unknown":
		return ""
	}
	return "denied by policy: " + firstNonEmpty(m.Policy, "a deny policy")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (hs *hubSession) onProxyDecision(m *protocol.ProxyDecision) {
	status := "denied"
	switch m.Verdict {
	case "allowed":
		status = "allowed"
	case "asked":
		status = "approved"
		if !m.Allowed {
			status = "rejected"
		}
	case "unknown":
		status = "unknown"
	}
	method, host, port, path, query, scheme := splitRequestURL(m.Method, m.URL)
	hs.hub.srv.store.InsertNetworkRequest(&store.NetworkRequest{
		UserID: hs.userID, SessionID: hs.id,
		Method: method, Scheme: scheme, Host: host, Port: port, Path: path, Query: query,
		Status: status, Reason: m.Policy,
	})
	delta := store.UsageDelta{}
	if m.Allowed {
		delta.Allowed = 1
	} else {
		delta.Blocked = 1
	}
	if m.Verdict == "asked" {
		delta.Approvals = 1
	}
	hs.hub.srv.store.AddUsage(time.Now(), hs.userID, hs.current.ID, delta)
	if !m.Allowed {
		// The decision does not say which command made the request, so it
		// goes to every run in flight; one request seen again is named once.
		// The query stays out: it is where a URL carries a token.
		req := blockedRequest{Req: method + " " + scheme + "://" + host + path, Refused: refusedBecause(m)}
		hs.mu.Lock()
		for id, seen := range hs.blocked {
			if !slices.Contains(seen, req) {
				hs.blocked[id] = append(seen, req)
			}
		}
		hs.mu.Unlock()
	}
	hs.traceRecord("proxy.decision", m)
}

// park starts the grace clock when the wire drops.
func (hs *hubSession) park() {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.closed || hs.parkTimer != nil {
		return
	}
	hs.parkTimer = time.AfterFunc(resumableFor, func() {
		hs.agent.Interrupt()
		hs.shutdown("resume window expired")
	})
}

func (hs *hubSession) stopParkTimer() {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.parkTimer != nil {
		hs.parkTimer.Stop()
		hs.parkTimer = nil
	}
}

// shutdown releases the in-memory session. The database row and its state
// survive: reopening the workspace continues the conversation.
func (hs *hubSession) shutdown(reason string) {
	hs.mu.Lock()
	if hs.closed {
		hs.mu.Unlock()
		return
	}
	hs.closed = true
	cancel := hs.turnCancel
	hs.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	hs.saveState(hs.current.ID)
	hs.traceRecord("session.close", map[string]string{"reason": reason})
	hs.conn.Close()

	hs.hub.mu.Lock()
	if hs.hub.sessions[hs.id] == hs {
		delete(hs.hub.sessions, hs.id)
	}
	hs.hub.mu.Unlock()
}

func (hs *hubSession) traceRecord(kind string, payload any) {
	if hs.traces == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	hs.traces.Append(hs.traceKey, trace.Record{Session: hs.id, Actor: hs.userID, Kind: kind, Payload: data})
}

// splitRequestURL breaks a URL into the columns network_requests wants.
func splitRequestURL(method, raw string) (m, host string, port int, path, query, scheme string) {
	m = method
	scheme, host, path, query = "https", raw, "/", ""
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		if u.Scheme != "" {
			scheme = u.Scheme
		}
		host = u.Hostname()
		if p := u.Port(); p != "" {
			port, _ = strconv.Atoi(p)
		}
		if u.Path != "" {
			path = u.Path
		}
		query = u.RawQuery
	}
	if port == 0 {
		if scheme == "http" {
			port = 80
		} else {
			port = 443
		}
	}
	return
}
