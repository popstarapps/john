package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/popstarapps/john/client/credstore"
	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/terminalui"
)

// Options assemble a session client. Everything the terminal session needs is
// here, so `john` signed in, `john` standalone, and the tests build the same
// app.
type Options struct {
	BackendURL string
	Token      string
	Email      string // display only
	Standalone bool   // the backend is this process, so nobody signed in anywhere
	Workdir    string
	Sandbox    sandbox.Sandbox // nil picks this platform's machine, or refuses to start
	Version    string

	// Credentials, when set, are kept current for as long as the session
	// runs: the access token above lives hours and a session can outlast it.
	// Nil in standalone and in tests, where nothing expires.
	Credentials *credstore.Credentials

	Plain       bool
	ASCII       bool
	Fast        bool
	BusySpinner string
	BusyEffect  string
	UI          *terminalui.Options // test override; nil builds the default
}

// app is the session client: it renders the conversation, forwards developer
// input to the backend, and executes what comes back — in the sandbox. The
// agent loop is not here, which is the entire point of the architecture.
type app struct {
	ui      *terminalui.UI
	sb      sandbox.Sandbox
	wire    *wire
	workdir string
	email   string
	solo    bool
	version string
	fast    bool

	np *networkProxy

	mu        sync.Mutex
	confirms  map[string]chan terminalui.ConfirmDecision
	diffs     map[string]chan terminalui.DiffDecision
	picks     map[string]chan terminalui.ModelDecision
	approvals map[string]chan terminalui.ApprovalDecision
	seq       int
	model     string
	modelID   string
	models    []protocol.ModelInfo // what /model may choose from, as the backend said
	narrowed  bool
	tokens    int
	window    int
	offline   bool
	sbState   string
	dirty     map[string]bool // what the status line counts as unapplied; see countUnapplied
	recount   chan struct{}   // see recountUnapplied
	limits    protocol.Limits // as negotiated on open
	turnSeq   int
	turn      string  // client-allocated id of the running turn, "" when idle
	update    *update // set when the backend refused this build

	// The environment, watched while the session runs (envwatch.go).
	envChanged   bool          // .john/ no longer matches what the sandbox was built from
	envSeen      string        // the last definition the developer was told about
	rebuilding   bool          // a rebuild is running
	restarting   chan struct{} // closed when a restart of a stopped sandbox finishes; nil when none runs
	rebuildAfter bool          // a rebuild was asked for during a turn, and waits for its end

	// hostAsks is the access-request question, one per host (netreq.go).
	hostAsks map[string]*hostAsk
	// rejected is the request that was turned down for each host, by its
	// reference, so a block on that host says why it is still blocked.
	rejected map[string]string
	// approved is each host whose access request the developer was told was
	// approved, until the policy refresh that brings it, which then says
	// nothing more about it.
	approved map[string]bool
	ctx      context.Context
}

// Session runs one interactive session and returns the exit code.
//
// An update the backend demanded is asked about and installed only after the
// session has wound down completely: the sandbox stopped and its disk
// settled, the wire closed. On Windows the update runs the new john as a
// child of this one, which found the sandbox still held by its own parent
// and said it was in use; on Unix it replaces this process, which cut the
// machine off without the stop that syncs its disk.
func Session(opts Options) int {
	code, u := session(opts)
	if u != nil {
		return runUpdate(u, os.Stdin, os.Stdout, stdinIsATerminal())
	}
	return code
}

// session is Session up to the update: every deferred stop has run by the
// time it returns.
func session(opts Options) (int, *update) {
	if opts.Workdir == "" {
		opts.Workdir, _ = os.Getwd()
	}
	sb := opts.Sandbox
	if sb == nil {
		// A machine one administrative step away from its own virtual machine
		// is worth asking about before settling for a weaker sandbox or for
		// none — and asking here, before the UI is drawn, because this is the
		// last moment the terminal the question needs belongs to us.
		if offerSandboxAdmin(os.Stdin, os.Stdout, stdinIsATerminal(), opts.Workdir, sandboxAdminAsked()) {
			// Taken, and still no machine until the next sign-in: the
			// developer has just been told so, and going on to pick a
			// sandbox would only add an error that reads as if it failed.
			return 2, nil
		}

		var err error
		sb, err = pickSandbox(opts.Workdir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "john:", err)
			return 1, nil
		}
	}

	uiOpts := terminalui.Options{
		Logo:           true,
		FileCompletion: true,
		ASCII:          opts.ASCII,
		Placeholder:    "ask john to do something…",
		Commands: []terminalui.Command{
			{Name: "apply", Description: "review the sandbox's changes and apply them"},
			{Name: "rebuild", Description: "rebuild the environment, keeping what you have not applied"},
			{Name: "reset", Description: "discard the sandbox and start clean"},
			{Name: "policies", Description: "show the policies applied to this session"},
			{Name: "model", Args: "[name]", Description: "show or change the model this session runs on"},
			{Name: "resources", Args: "[cpu=n memory=n disk=n]",
				Description: "show or change this sandbox's limits"},
			{Name: "env", Args: "[init]",
				Description: "show the sandbox environment, or start one for this project"},
			{Name: "resume", Args: "[number]",
				Description: "list this workspace's earlier sessions, or carry one on"},
			{Name: "new", Description: "start a new conversation; this one stays in /resume"},
			{Name: "help", Description: "list commands"},
			{Name: "quit", Description: "end the session"},
		},
	}
	if opts.Plain {
		uiOpts.Mode = terminalui.ModePlain
	}
	uiOpts.BusySpinner = busySpinners[opts.BusySpinner]
	uiOpts.BusyEffect = busyEffects[opts.BusyEffect]
	if opts.UI != nil {
		uiOpts = *opts.UI
	}

	a := newApp(terminalui.New(uiOpts), sb, opts)
	// However the session ends. Stopping is what settles the sandbox's disk:
	// the agent unmounts the workspace and syncs before the machine is cut
	// off. Without it the machine died with this process, a power cut, and
	// whatever the agent wrote in the half-minute before was empty or gone
	// the next time john started.
	defer stopSandbox(a.sb)

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	// The access token is good for hours and a session for as long as someone
	// keeps working, so it is renewed underneath: the wire picks the current
	// one up on its next reconnect.
	if opts.Credentials != nil {
		done := make(chan struct{})
		defer close(done)
		r := &refresher{creds: opts.Credentials, holder: a.wire}
		r.onExpired = func(err error) { a.notice(terminalui.NoticeWarn, err.Error()) }
		go r.run(done)
	}

	a.boot()
	go a.loop(ctx)
	go a.start(ctx)

	if err := a.ui.Run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "john:", err)
		return 1, nil
	}
	a.wire.close()

	// The backend refused this build. Session asks about it once this has
	// returned and the sandbox is stopped — and asking is the point: the
	// binary is not rewritten without a yes.
	return 0, a.pendingUpdate()
}

// stopSandbox stops a sandbox that runs something to stop — a virtual
// machine — and waits a bounded while for it: a sandbox that will not stop
// must not keep the terminal from coming back.
func stopSandbox(sb sandbox.Sandbox) {
	s, ok := sb.(interface{ Stop(context.Context) error })
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.Stop(ctx)
}

// runUpdate asks, then updates. It is the one path both the start-up check
// and the handshake refusal end in, so the developer sees the same thing
// either way. Somewhere with nobody to ask, it says what to run and stops:
// replacing a binary unattended is not a decision to make on someone's
// behalf.
func runUpdate(u *update, in io.Reader, out io.Writer, interactive bool) int {
	if !interactive {
		fmt.Fprintf(out, "This backend runs john %s and you have %s.\n"+
			"Run john from a terminal to update, or install %s yourself.\n",
			u.Required, u.Current, u.Required)
		return 1
	}
	if !u.confirm(in, out) {
		fmt.Fprintln(out, "Left alone. This backend will not run john "+u.Current+".")
		return 1
	}
	code, err := u.apply(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "john update:", err)
		return 1
	}
	return code
}

func newApp(ui *terminalui.UI, sb sandbox.Sandbox, opts Options) *app {
	a := &app{
		ui: ui, sb: sb, workdir: opts.Workdir, email: opts.Email, solo: opts.Standalone,
		version: opts.Version, fast: opts.Fast,
		confirms:  map[string]chan terminalui.ConfirmDecision{},
		diffs:     map[string]chan terminalui.DiffDecision{},
		picks:     map[string]chan terminalui.ModelDecision{},
		approvals: map[string]chan terminalui.ApprovalDecision{},
		dirty:     map[string]bool{},
		sbState:   "sandbox starting",
		recount:   make(chan struct{}, 1),
	}
	a.wire = newWire(a, opts.BackendURL, opts.Token, opts.Workdir)
	return a
}

// pickSandbox gives this machine the sandbox, or none.
//
// There is one, and it is a virtual machine. A weaker sandbox that arrives
// without announcing itself would be worse than none at all: nothing on screen
// would tell a developer which one their tool calls had just run in.
//
// So when the machine cannot start, john refuses and says what is missing. It
// does not quietly fall back to the in-memory sandbox, which isolates nothing
// — every tool call would run directly on the developer's machine while the
// whole design says otherwise. That is the posture everywhere here: the
// sandbox is a control, and a control that silently degrades is not one.
// Running without isolation stays possible and has to be asked for, by name,
// on the command line.
func pickSandbox(workdir string) (sandbox.Sandbox, error) {
	vm, err := sandbox.NewVM(workdir, sandbox.VMOptions{Version: buildVersion,
		DefaultEnvironment: sandbox.StarterEnvironment(), RegistryCredentials: registryCredentials})
	if err != nil {
		return nil, err
	}
	if err := vm.HypervisorAvailable(); err != nil {
		return nil, errNoSandbox(err)
	}
	return vm, nil
}

// errNoSandbox says what is missing and what to do about it.
//
// It is one sentence of john's and then the machine's own words, because the
// machine is the only thing that knows which of the reasons it is — a firmware
// setting, a group membership, a library to install — and each of them asks
// something different of the person reading it. Every hypervisor's Available()
// is written to be the second half of this message.
func errNoSandbox(reason error) error {
	return fmt.Errorf("this machine cannot start a sandbox, and john will not run tool calls outside one.\n"+
		"  %s",
		strings.TrimPrefix(reason.Error(), "sandbox: "))
}

func (a *app) boot() {
	who := a.email
	if who == "" {
		who = "developer"
	}
	if a.solo {
		a.ui.Send(terminalui.Notice{Text: "standalone · running as " + who})
	} else {
		a.ui.Send(terminalui.Notice{Text: "signed in as " + who + " · single sign-on"})
	}
	a.status()
}

// start brings the sandbox up and opens the session, off the UI thread. The
// network proxy is established first, so the sandbox can be built with the
// network route it will actually run under: through the proxy, and nowhere
// else.
func (a *app) start(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
	if a.wire.currentToken() != "" {
		a.np = a.setupProxy(ctx)
		if a.np != nil {
			// The sandbox's only route out is the proxy, so it has to be
			// told about it before it starts rather than after.
			switch sb := a.sb.(type) {
			case *sandbox.VM:
				sb.SetProxy(a.np.proxySocket(), a.np.ca.CertPEM())
				sb.SetBuildProxy(a.np.buildSocket)
				// Where an environment build's traffic goes is the
				// administrator's decision and travels in the signed
				// snapshot: direct by default, because a build is how a
				// project gets its compilers and is what makes the sandbox
				// useful under a policy that allows no egress at all.
				if snap := a.np.store.Current(); snap != nil {
					sb.SetBuildNetwork(snap.BuildNetwork())
				}
			}
			// Before the build, so an excluded path is never seeded in the
			// first place rather than removed afterwards.
			if n := a.protect(a.np.store.Current()); n > 0 {
				a.notice(terminalui.NoticeInfo, fmt.Sprintf(
					"workspace protection: %s · protected paths are not the agent's to change", plural(n, "path rule")))
			}
		}
	}

	// The first time a project gets a sandbox, the file that decides what is
	// in it is written where the developer will see it, before the build
	// that reads it.
	if vm, ok := a.sb.(*sandbox.VM); ok {
		if written, err := vm.WriteStarterIfNew(); err != nil {
			a.notice(terminalui.NoticeWarn, "could not write "+sandbox.EnvFile+": "+err.Error())
		} else if written != "" {
			a.notice(terminalui.NoticeInfo, "wrote "+written+" · the sandbox's environment, with basic "+
				"developer tools · add this project's own there, and john picks up the change")
		}
	}

	progressID := "sandbox-build"
	build := func() error {
		return a.sb.Build(ctx, func(p sandbox.Progress) {
			a.ui.Send(terminalui.Progress{ID: progressID, Label: p.Label,
				Step: p.Step, Steps: p.Steps, Detail: p.Detail})
		})
	}
	err := build()
	// A workspace with more than one sandbox, or with its only one already in
	// use, is the one question about a sandbox a developer has to answer. It
	// is asked here rather than reported, because the answer is a choice made
	// in the moment and not a reason to start john again with a flag.
	if choice, ok := sandbox.AsInstanceChoice(err); ok {
		if picked := a.chooseSandbox(ctx, choice); picked != "" {
			if vm, isVM := a.sb.(*sandbox.VM); isVM {
				vm.SetInstance(picked)
				err = build()
			}
		}
	}
	if err != nil {
		a.ui.Send(terminalui.Progress{ID: progressID, Label: "sandbox failed", Done: true,
			Status: terminalui.ToolFail, Meta: err.Error()})
		a.setSandboxState("sandbox unavailable")
	} else {
		a.ui.Send(terminalui.Progress{ID: progressID, Label: "sandbox ready", Done: true,
			Status: terminalui.ToolOK})
		a.setSandboxState("sandbox ready")
		if vm, ok := a.sb.(*sandbox.VM); ok {
			go a.watchEnvironment(ctx, vm)
		}
		// What an earlier session left unapplied counts from the start.
		go a.countUnapplied(ctx)
		a.recountUnapplied()
	}

	if err := a.wire.connect(ctx); err != nil {
		var ve versionError
		if errors.As(err, &ve) {
			a.updateRequired(ve, false)
			return
		}
		var refused refusedError
		if errors.As(err, &refused) {
			a.notice(terminalui.NoticeError, "the backend refused the session: "+refused.message)
		} else {
			a.notice(terminalui.NoticeError, "backend unreachable: "+err.Error())
		}
		a.setOffline(true)
		go a.wire.reopenFresh()
	}
}

// updateRequired is the backend refusing this build. It is not something to
// reconnect through: this client is the wrong build, and every retry would be
// refused the same way. So the UI stops, and the developer is asked about the
// update on a plain terminal rather than under a running session.
//
// midSession is a backend that was upgraded under a session that was already
// running: the session cannot carry on, but the conversation is the
// backend's, and /resume finds it again after the update.
func (a *app) updateRequired(ve versionError, midSession bool) {
	a.mu.Lock()
	a.update = ve.u
	a.mu.Unlock()
	if midSession {
		a.notice(terminalui.NoticeWarn, "the backend was updated to john "+ve.u.Required+
			" and will not run "+ve.u.Current+" · stopping to update; /resume picks this conversation up after")
	} else {
		a.notice(terminalui.NoticeWarn, ve.Error())
	}
	a.ui.Quit()
}

// pendingUpdate is the update the backend demanded mid-handshake, if any. It
// is read after the UI has stopped, because that is when there is a terminal
// free to ask on.
func (a *app) pendingUpdate() *update {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.update
}

// protect installs the workspace-protection policies from a verified snapshot
// into the sandbox, and returns how many path rules are in force.
//
// A session that cannot reach the backend does not thereby lose them: with no
// snapshot nothing is installed, and the sandbox keeps what it last persisted.
// Loosening a protection takes a snapshot that says so, signed.
func (a *app) protect(snap *policy.Snapshot) int {
	if snap == nil {
		return 0
	}
	prot := snap.Protections()
	if err := a.sb.Protect(prot); err != nil {
		if errors.Is(err, sandbox.ErrStoppedAnswering) {
			// Not the protections' fault: the sandbox is gone. They are
			// stored, and the restarted sandbox starts with them.
			a.sandboxStopped()
			return prot.Rules()
		}
		a.notice(terminalui.NoticeWarn, "workspace protection could not be applied: "+err.Error())
		return 0
	}
	return prot.Rules()
}

// applyReady digests session.ready: which model, what the session may use.
func (a *app) applyReady(m *protocol.SessionReady) {
	a.mu.Lock()
	a.model = m.Model.Name
	if a.model == "" {
		a.model = m.Model.ID
	}
	a.modelID = m.Model.ID
	a.models = m.Models
	a.narrowed = m.Narrowed
	a.limits = m.Limits
	if m.TurnsSoFar > a.turnSeq {
		a.turnSeq = m.TurnsSoFar // carrying on a session: number on from it
	}
	// How full the conversation is, as it stands: a reopened one shows its
	// figure before its next turn, and a new one starts again from nothing.
	a.tokens = m.Context
	if m.ContextWindow > 0 {
		a.window = m.ContextWindow
	}
	a.mu.Unlock()
	a.status()
}

// carryingOn says, once, that john opened on a conversation it had before
// rather than a new one. Running john in a workspace carries on its session,
// and nothing on screen said so: what the model remembered, and the
// instructions it was carrying, were a surprise.
func (a *app) carryingOn(r *protocol.SessionReady) {
	if r.TurnsSoFar == 0 {
		return
	}
	when := ""
	if t, err := time.Parse(time.RFC3339, r.StartedAt); err == nil {
		t = t.Local()
		if y, m, d := time.Now().Date(); t.Year() == y && t.Month() == m && t.Day() == d {
			when = " from " + t.Format("15:04")
		} else {
			when = " from " + t.Format("Mon 2 Jan 15:04")
		}
	}
	turns := "turns"
	if r.TurnsSoFar == 1 {
		turns = "turn"
	}
	a.ui.Send(terminalui.Notice{Level: terminalui.NoticeInfo, Prominent: true, Text: fmt.Sprintf(
		"carrying on this workspace's session%s · %d %s so far · /new starts a new one",
		when, r.TurnsSoFar, turns)})
}

// startNew is /new: a new conversation in this workspace. The one it leaves
// is ended rather than deleted, so /resume still has it; the sandbox, and
// whatever in it is not applied yet, is not part of a conversation and stays
// as it is.
func (a *app) startNew() {
	a.mu.Lock()
	running := a.turn != ""
	a.mu.Unlock()
	if running {
		a.notice(terminalui.NoticeWarn, "a turn is running · press Esc to stop it, then /new")
		return
	}
	if err := a.wire.newSession(); err != nil {
		a.notice(terminalui.NoticeError, "could not start a new session: "+err.Error())
		return
	}
	a.notice(terminalui.NoticeInfo, "new session · the last one is in /resume")
}

func (a *app) setOffline(v bool) {
	a.mu.Lock()
	changed := a.offline != v
	a.offline = v
	a.mu.Unlock()
	if changed {
		if v {
			a.notice(terminalui.NoticeWarn, "backend unreachable · prompts are refused until it returns")
		} else {
			a.notice(terminalui.NoticeInfo, "backend reachable again")
		}
		a.status()
	}
}

// setContext records how much of the model's context the conversation
// fills, and of how much.
func (a *app) setContext(n, window int) {
	a.mu.Lock()
	a.tokens, a.window = n, window
	a.mu.Unlock()
	a.status()
}

func (a *app) setSandboxState(s string) {
	a.mu.Lock()
	a.sbState = s
	a.mu.Unlock()
	a.status()
}

func (a *app) markDirty(path string) {
	a.mu.Lock()
	a.dirty[path] = true
	a.mu.Unlock()
	a.status()
}

// recountUnapplied asks for the unapplied count to be worked out again. It
// never waits: requests that arrive while a count is running fold into one.
func (a *app) recountUnapplied() {
	select {
	case a.recount <- struct{}{}:
	default:
	}
}

// countUnapplied keeps the status line's unapplied count true, one count at a
// time, for as long as the session lasts. It counts what the sandbox holds,
// not what the edit tool wrote: `npm install` writes package-lock.json without
// an edit, and a sandbox carries the last session's work into this one. What
// apply would leave unticked — node_modules, what .gitignore ignores — is not
// counted, so the number is what /apply offers.
func (a *app) countUnapplied(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.recount:
		}
		paths, err := a.sb.Changed(ctx)
		if err != nil {
			continue
		}
		skip, _ := unselectedByDefault(ctx, a.sb)
		dirty := map[string]bool{}
		for _, p := range paths {
			if !skip(p) {
				dirty[p] = true
			}
		}
		a.mu.Lock()
		a.dirty = dirty
		a.mu.Unlock()
		a.status()
	}
}

func (a *app) status() {
	a.mu.Lock()
	st := terminalui.Status{
		Workdir: displayPath(a.workdir),
		Model:   a.model,
		Tokens:  a.tokens,
		Window:  a.window,
	}
	// A ready sandbox is the normal case and says nothing worth a column;
	// only a sandbox that is not ready, or has something to say, is shown.
	var sb []string
	if a.sbState != "sandbox ready" && a.sbState != "" {
		sb = append(sb, a.sbState)
	}
	if n := len(a.dirty); n > 0 {
		sb = []string{fmt.Sprintf("%d unapplied · /apply", n)}
	}
	if a.envChanged && !a.rebuilding {
		sb = append(sb, "environment changed, /rebuild")
	}
	st.Sandbox = strings.Join(sb, " · ")
	if a.offline {
		st.Alert = "backend unreachable · prompts blocked"
	}
	a.mu.Unlock()
	a.ui.Send(st)
}

func (a *app) notice(level terminalui.NoticeLevel, text string) {
	a.ui.Send(terminalui.Notice{Level: level, Text: text})
}

// loop is the UI action pump: developer input goes toward the backend, and
// modal answers go to whoever is blocked on them.
func (a *app) loop(ctx context.Context) {
	for act := range a.ui.Actions() {
		switch t := act.(type) {
		case terminalui.UserPrompt:
			a.prompt(t.Text)

		case terminalui.SlashCommand:
			go a.command(ctx, t)

		case terminalui.Interrupt:
			a.mu.Lock()
			turn := a.turn
			a.mu.Unlock()
			if turn != "" {
				a.wire.send(&protocol.Interrupt{Turn: turn})
			}
			// Local cancellation keeps in-flight commands from running on
			// after the developer said stop; their results still report.
			a.wire.mu.Lock()
			for _, cancel := range a.wire.tools {
				cancel()
			}
			a.wire.mu.Unlock()

		case terminalui.ApprovalDecision:
			deliver(a, a.approvals, t.ID, t)
		case terminalui.ConfirmDecision:
			deliver(a, a.confirms, t.ID, t)
		case terminalui.DiffDecision:
			deliver(a, a.diffs, t.ID, t)
		case terminalui.ModelDecision:
			deliver(a, a.picks, t.ID, t)

		case terminalui.CompletionRequest:
			a.ui.Send(terminalui.Completions{Token: t.Token, Items: a.completeFiles(t.Query)})
		}
	}
}

func deliver[T any](a *app, m map[string]chan T, id string, v T) {
	a.mu.Lock()
	ch := m[id]
	delete(m, id)
	a.mu.Unlock()
	if ch != nil {
		ch <- v
	}
}

// prompt sends the developer's text to the loop — unless the backend is
// unreachable, in which case john fails closed and says so.
func (a *app) prompt(text string) {
	// What this session is working on, remembered for the sandbox. It is the
	// first thing the developer asked for, and it is what tells two sandboxes
	// apart weeks later: "last used two days ago" identifies one of them, and
	// "refactor the core interfaces" identifies the work.
	if vm, ok := a.sb.(*sandbox.VM); ok {
		vm.SetNote(text)
	}

	// Before the session is open there is no Conn to send on, and the wire
	// would drop this line: the sandbox may still be building, or the
	// handshake may be in flight. Saying so is the honest answer — the
	// alternative is a swallowed prompt and a UI that thinks forever.
	if !a.wire.connected() {
		a.notice(terminalui.NoticeWarn,
			"not connected yet · the session is still opening, try again in a moment")
		return
	}
	a.mu.Lock()
	if a.offline {
		a.mu.Unlock()
		a.notice(terminalui.NoticeError,
			"blocked: the agent loop runs in the backend, and the backend is unreachable")
		return
	}
	a.mu.Unlock()

	// A mention that names an image or a PDF travels with the prompt rather
	// than waiting for a read the model has nothing to do with. Read first:
	// it is the developer's own file and the reading of it can fail, and
	// saying so before the turn begins is better than a turn that quietly
	// went without it.
	attachments := a.attach(context.Background(), text)

	a.mu.Lock()
	a.turnSeq++
	turn := fmt.Sprintf("t%d", a.turnSeq)
	if a.turn == "" {
		a.turn = turn
	}
	a.mu.Unlock()
	a.ui.Send(terminalui.Busy{Label: "thinking"})
	a.wire.send(&protocol.Prompt{Turn: turn, Text: text, Attachments: attachments})
}

// turnLeftBehind is the client giving up a turn whose stream is gone: a
// resume the backend refused, or a move to another conversation.
//
// Until this existed the UI simply kept thinking. The backend had restarted,
// the session had been reopened, and nothing was ever going to answer the
// turn the spinner was for — the same shape as the prompt that used to be
// swallowed before the session was open, and the same answer: say so.
func (a *app) turnLeftBehind() {
	a.mu.Lock()
	had := a.turn
	a.turn = ""
	a.mu.Unlock()
	if had == "" {
		return
	}
	a.ui.Send(terminalui.Idle{})
	a.notice(terminalui.NoticeWarn,
		"the turn that was running did not come with the reconnect · ask again unless the backend picks it up")
	a.status()
}

// beginTurn takes up a turn the backend says is running. Usually it is the
// one this client just sent; after a backend restart it is one this process
// never saw, and adopting it is what makes Esc and the status line mean
// something again.
func (a *app) beginTurn(turn string) {
	a.mu.Lock()
	if turn != "" {
		a.turn = turn
	}
	a.mu.Unlock()
	a.ui.Send(terminalui.Busy{Label: "thinking"})
}

// endTurn closes out a turn the backend finished.
func (a *app) endTurn(m *protocol.TurnEnd) {
	a.mu.Lock()
	a.turn = ""
	rebuild := a.rebuildAfter
	a.rebuildAfter = false
	ctx := a.ctx
	a.mu.Unlock()
	if rebuild {
		defer a.startRebuild(ctx)
	}
	switch m.Reason {
	case "interrupted":
		a.notice(terminalui.NoticeWarn, "interrupted · the sandbox keeps whatever was already written")
	case "failed":
		a.notice(terminalui.NoticeError, "the turn failed · see the message above")
	case "aborted":
		a.notice(terminalui.NoticeWarn, "turn aborted")
	case "lost":
		a.notice(terminalui.NoticeWarn,
			"the backend stopped in the middle of this turn and could not pick it up · ask again")
	}
	a.ui.Send(terminalui.Idle{})
	a.status()
}

// onNarrow is a routing policy acting mid-session: the backend holds the
// model request and asks which of the remaining models to continue on.
func (a *app) onNarrow(m *protocol.ModelNarrow) {
	reply := make(chan terminalui.ModelDecision, 1)
	a.mu.Lock()
	a.picks[m.ID] = reply
	a.mu.Unlock()

	models := make([]terminalui.ModelOption, 0, len(m.Models))
	for _, opt := range m.Models {
		models = append(models, terminalui.ModelOption{
			ID: opt.ID, Name: opt.Name, Detail: opt.Detail, Internal: opt.Internal,
		})
	}
	a.ui.Send(terminalui.ModelChoice{
		ID: m.ID, Reason: m.Reason, Policy: m.Policy, Models: models,
	})

	dec := <-reply
	if dec.Cancelled {
		a.wire.send(&protocol.ModelChoice{ID: m.ID, Cancelled: true})
		return
	}
	a.wire.send(&protocol.ModelChoice{ID: m.ID, Model: dec.ModelID})
	a.mu.Lock()
	a.narrowed = true
	for _, opt := range m.Models {
		if opt.ID == dec.ModelID {
			a.model, a.modelID = opt.Name, opt.ID
		}
	}
	a.mu.Unlock()
	a.status()
}

// chooseModel is /model. With a name it moves the session to the model that
// name picks out; without one it opens the list to choose from. Either way
// the client only asks: the backend decides, against policy, and answers
// with model.selected.
func (a *app) chooseModel(arg string) {
	a.mu.Lock()
	current, currentID, narrowed := a.model, a.modelID, a.narrowed
	models := append([]protocol.ModelInfo(nil), a.models...)
	a.seq++
	id := fmt.Sprintf("select.%d", a.seq)
	a.mu.Unlock()

	if len(models) <= 1 {
		text := "model " + current
		switch {
		case narrowed:
			text += " · a routing policy narrowed this session to it"
		case len(models) == 1:
			text += " · the only model your policy groups allow"
		}
		a.notice(terminalui.NoticeInfo, text)
		return
	}

	var want string
	if arg = strings.TrimSpace(arg); arg != "" {
		matches := matchModels(models, arg)
		switch len(matches) {
		case 1:
			want = matches[0].ID
		case 0:
			a.notice(terminalui.NoticeWarn, fmt.Sprintf("no model matches %q · you can use %s", arg, modelNames(models)))
			return
		default:
			a.notice(terminalui.NoticeWarn, fmt.Sprintf("%q matches %s · be more specific", arg, modelNames(matches)))
			return
		}
	} else {
		options := make([]terminalui.ModelOption, 0, len(models))
		for _, m := range models {
			opt := terminalui.ModelOption{ID: m.ID, Name: m.Name, Internal: m.Internal}
			if m.ID == currentID {
				opt.Detail = "current"
			}
			options = append(options, opt)
		}
		reply := make(chan terminalui.ModelDecision, 1)
		a.mu.Lock()
		a.picks[id] = reply
		a.mu.Unlock()
		detail := "for this session"
		if narrowed {
			detail = "what the session's narrowing left"
		}
		a.ui.Send(terminalui.ModelChoice{ID: id, Title: "MODEL", Detail: detail,
			Models: options, Selected: currentID, Preference: true})
		dec := <-reply
		if dec.Cancelled {
			return
		}
		want = dec.ModelID
	}

	if want == currentID {
		a.notice(terminalui.NoticeInfo, "model "+current+" · already on it")
		return
	}
	a.wire.send(&protocol.ModelSelect{ID: id, Model: want})
}

// onModelSelected is the backend's answer to /model.
func (a *app) onModelSelected(m *protocol.ModelSelected) {
	if m.Error != "" {
		a.notice(terminalui.NoticeWarn, "model not changed · "+m.Error)
		return
	}
	a.mu.Lock()
	a.model, a.modelID = m.Model.Name, m.Model.ID
	if a.model == "" {
		a.model = m.Model.ID
	}
	name := a.model
	a.mu.Unlock()
	a.notice(terminalui.NoticeInfo, "model → "+name+" · from the next prompt on")
	a.status()
}

// matchModels finds what a /model argument names: an exact id or name first,
// then any whose name or id contains it, ignoring case.
func matchModels(models []protocol.ModelInfo, arg string) []protocol.ModelInfo {
	q := strings.ToLower(arg)
	for _, m := range models {
		if strings.ToLower(m.ID) == q || strings.ToLower(m.Name) == q {
			return []protocol.ModelInfo{m}
		}
	}
	var out []protocol.ModelInfo
	for _, m := range models {
		if strings.Contains(strings.ToLower(m.Name), q) || strings.Contains(strings.ToLower(m.ID), q) {
			out = append(out, m)
		}
	}
	return out
}

func modelNames(models []protocol.ModelInfo) string {
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
}

func (a *app) command(ctx context.Context, cmd terminalui.SlashCommand) {
	switch cmd.Name {
	case "quit", "exit":
		a.ui.Quit()

	case "help":
		a.ui.Send(terminalui.AssistantMessage{Text: "**Commands**\n\n" +
			"- `/apply` — review what the sandbox changed and write it to your working directory\n" +
			"- `/rebuild` — rebuild the environment, keeping everything you have not applied\n" +
			"- `/reset` — discard the sandbox entirely, unapplied changes included\n" +
			"- `/policies` — the policies applied to this session\n" +
			"- `/model` — the model this session runs on; `/model <name>` or a pick from the list moves it to another\n" +
			"- `/resources` — the sandbox's cpu, memory, disk and process limits; " +
			"`/resources cpu=8 memory=16` changes them\n" +
			"- `/env` — the environment this sandbox is built from; " +
			"`/env init` starts one for this project\n" +
			"- `/resume` — earlier sessions in this workspace; `/resume 2` carries one on\n" +
			"- `/new` — start a new conversation; this one stays in `/resume`\n" +
			"- `/quit` — end the session\n\n" +
			"`@` mentions a file, `ctrl+r` searches history, Esc interrupts a turn. " +
			"Type while john works and your message is queued."})

	case "policies":
		a.policies()

	case "model":
		go a.chooseModel(cmd.Args)

	case "resources":
		a.resources(cmd.Args)

	case "env":
		a.environment(cmd.Args)

	case "resume":
		a.resume(cmd.Args)

	case "new":
		a.startNew()

	case "apply":
		a.review(ctx)

	case "rebuild":
		a.rebuild(ctx)

	case "reset":
		a.reset(ctx)

	default:
		a.notice(terminalui.NoticeError, "unknown command: /"+cmd.Name)
	}
}

// policies says what the snapshot this session is actually holding says.
//
// Policies are written in the console and that is where the argument about
// them belongs, but three of the things they decide are invisible from here
// until something refuses: whether the network is open at all, how much of the
// workspace is not the agent's to change, and where an image build's traffic
// goes. The last is the reason this exists — a developer watching a build
// fetch nothing has no other way to tell that it is being policed rather than
// broken.
func (a *app) policies() {
	var snap *policy.Snapshot
	if a.np != nil {
		snap = a.np.store.Current()
	}
	if snap == nil {
		a.notice(terminalui.NoticeWarn, "no verified policy snapshot — nothing is allowed out "+
			"until one arrives, and the console is where they are written")
		return
	}

	build := map[string]string{
		policy.BuildDirect:  "`direct` — unrestricted, and not recorded",
		policy.BuildProxied: "`proxied` — held to the network policies above",
	}[snap.BuildNetwork()]

	replacements := "off"
	if snap.ReplacementsActive {
		replacements = "on — matched values are aliased before anything leaves this machine"
	}

	a.ui.Send(terminalui.AssistantMessage{Text: fmt.Sprintf(
		"**Policies** in force for this session\n\n"+
			"- `network` — %s; everything not allowed is refused\n"+
			"- `workspace` — %s the agent may not change\n"+
			"- `replacements` — %s\n"+
			"- `image builds` — %s\n\n"+
			"Signed by the backend and checked here, so this is what the proxy enforces "+
			"rather than what the console last showed. They are written in the console; "+
			"`/policies` only reports them.",
		plural(len(snap.Network), "rule"), plural(snap.Protections().Rules(), "path rule"), replacements, build)})
}

// environment shows what the sandbox is built from, and starts a definition
// for a project that has none. The environment is a file in the repository on
// purpose: it makes the contents of the sandbox reviewable, and it makes every
// developer on the project get the same one.
func (a *app) environment(args string) {
	file, id, custom, err := sandbox.Environment(a.workdir)
	if err != nil {
		a.notice(terminalui.NoticeError, "could not read the sandbox environment: "+err.Error())
		return
	}

	if strings.TrimSpace(args) == "init" {
		if custom {
			a.notice(terminalui.NoticeInfo, file+" already exists · edit it, and /rebuild applies the change")
			return
		}
		written, err := sandbox.InitEnvironment(a.workdir)
		if err != nil {
			a.notice(terminalui.NoticeError, "could not write the environment: "+err.Error())
			return
		}
		a.notice(terminalui.NoticeInfo, "wrote "+written+
			" · add the tools this project needs, then /rebuild to build the sandbox from it")
		return
	}

	if !custom {
		a.ui.Send(terminalui.AssistantMessage{Text: "**Sandbox environment**\n\n" +
			"This project has no `" + sandbox.EnvFile + "`, so the sandbox is built from john's " +
			"built-in environment: `" + sandbox.DefaultBaseImage + "` with basic developer tools.\n\n" +
			"`/env init` writes that same definition to the file. Add the compilers, linters and " +
			"test runners this project needs to it, and every developer's sandbox will have " +
			"them. It is a development environment, not a deployment image — the " +
			"`Dockerfile` at the root of the repository is deliberately not used."})
		return
	}
	a.ui.Send(terminalui.AssistantMessage{Text: "**Sandbox environment**\n\n" +
		"Built from `" + file + "` (`" + id + "`).\n\n" +
		"Edit it while john runs and john says so; `/rebuild` then builds the sandbox " +
		"from it, and anything the sandbox has written and you have not applied survives " +
		"that. Otherwise a changed definition is rebuilt on the next start. `/reset` also " +
		"rebuilds, but it discards the sandbox entirely, unapplied changes included."})
}

// resources shows the sandbox's limits, or changes them. They belong to this
// machine, not to a policy; nobody manages them centrally.
func (a *app) resources(args string) {
	cur := a.sb.Resources()
	if strings.TrimSpace(args) == "" {
		a.ui.Send(terminalui.AssistantMessage{Text: fmt.Sprintf(
			"**Sandbox resources** for %s\n\n"+
				"- `cpu` — %d vCPU\n"+
				"- `memory` — %d GB\n"+
				"- `disk` — %d GB\n"+
				"- `processes` — %d\n\n"+
				"These belong to this machine, not to a policy: nobody centrally manages "+
				"them and nothing in the console shows them. Change one with "+
				"`/resources cpu=8 memory=16`, and reset the sandbox to give it back.",
			displayPath(a.workdir), cur.CPUs, cur.MemoryGB, cur.DiskGB, cur.Processes)})
		return
	}

	next, changed := cur, []string{}
	for _, field := range strings.Fields(args) {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			a.notice(terminalui.NoticeError, "expected name=value, got "+field)
			return
		}
		n, err := parseSize(value)
		if err != nil || n <= 0 {
			a.notice(terminalui.NoticeError, "not a number: "+name+"="+value)
			return
		}
		switch strings.ToLower(name) {
		case "cpu", "cpus":
			next.CPUs = n
		case "memory", "mem":
			next.MemoryGB = n
		case "disk":
			next.DiskGB = n
		case "processes", "procs":
			next.Processes = n
		default:
			a.notice(terminalui.NoticeError, "unknown resource: "+name+" · try cpu, memory, disk or processes")
			return
		}
		changed = append(changed, fmt.Sprintf("%s=%d", name, n))
	}

	if err := a.sb.SetResources(next); err != nil {
		a.notice(terminalui.NoticeError, "could not change resources: "+err.Error())
		return
	}
	a.notice(terminalui.NoticeInfo, "sandbox resources: "+strings.Join(changed, " ")+
		" · applies when the sandbox next starts")
}

// review opens the diff viewer and applies what the developer selected.
// Reviewing and applying are one action: applying always means reading a diff
// first, because the review is the control.
func (a *app) review(ctx context.Context) {
	diffs, err := a.sb.Diff(ctx)
	if err != nil {
		a.notice(terminalui.NoticeError, "diff failed: "+err.Error())
		return
	}
	diffs, held := splitProtected(diffs)
	if len(held) > 0 {
		a.notice(terminalui.NoticeWarn, heldBackNotice(held))
	}
	if len(diffs) == 0 {
		a.mu.Lock()
		a.dirty = map[string]bool{}
		a.mu.Unlock()
		if len(held) > 0 {
			a.notice(terminalui.NoticeInfo, "nothing to apply · every change is to a protected path")
		} else {
			a.notice(terminalui.NoticeInfo, "nothing to apply · the sandbox matches your working directory")
		}
		a.status()
		return
	}

	applied := a.applyReviewed(ctx, diffs, "apply to "+displayPath(a.workdir))
	if touchesEnvironment(applied) {
		a.offerRebuild(ctx)
	}
}

// applyReviewed shows diffs in the review, and writes the files the developer
// picks into the working directory once they confirm. It returns the paths it
// wrote, none when nothing was.
func (a *app) applyReviewed(ctx context.Context, diffs []sandbox.FileDiff, title string) []string {
	id := fmt.Sprintf("review-%d", a.nextID())
	reply := make(chan terminalui.DiffDecision, 1)
	a.mu.Lock()
	a.diffs[id] = reply
	a.mu.Unlock()

	skip, why := unselectedByDefault(ctx, a.sb)
	if note := unselectedNotice(diffs, skip, why); note != "" {
		a.notice(terminalui.NoticeInfo, note)
	}
	a.ui.Send(renderDiffReview(id, title, diffs, skip))

	var dec terminalui.DiffDecision
	select {
	case dec = <-reply:
	case <-ctx.Done():
		a.ui.Send(terminalui.CancelDiffReview{ID: id})
		return nil
	}
	if !dec.Apply {
		a.notice(terminalui.NoticeInfo, "not applied · everything stays in the sandbox")
		return nil
	}

	// Writing to the developer's files is the one thing john does that it
	// cannot take back, so it is confirmed rather than assumed.
	if !a.confirm(ctx, terminalui.Confirm{
		Title:    "APPLY TO WORKING DIRECTORY",
		Question: fmt.Sprintf("Write %s into %s?", plural(len(dec.Paths), "file"), displayPath(a.workdir)),
		Detail:   dec.Paths,
	}) {
		a.notice(terminalui.NoticeInfo, "not applied · everything stays in the sandbox")
		return nil
	}

	toolID := fmt.Sprintf("apply-%d", a.nextID())
	a.ui.Send(terminalui.ToolStart{ID: toolID, Verb: "apply", Targets: dec.Paths})
	n, err := a.sb.Apply(ctx, dec.Paths)
	if err != nil {
		a.ui.Send(terminalui.ToolEnd{ID: toolID, Status: terminalui.ToolFail, Meta: err.Error()})
		return nil
	}
	a.ui.Send(terminalui.ToolEnd{ID: toolID, Status: terminalui.ToolOK,
		Meta: appliedMeta(n, a.workdir)})

	a.mu.Lock()
	for _, p := range dec.Paths {
		delete(a.dirty, p)
	}
	a.mu.Unlock()
	// Held-back changes stay in the sandbox forever by design, so they do not
	// count as work waiting to be applied.
	remaining, _ := a.sb.Changed(ctx)
	a.wire.send(&protocol.SandboxState{State: "applied", PendingFiles: len(remaining)})
	a.status()
	a.recountUnapplied()
	return dec.Paths
}

// offerRebuild asks, right after an apply that changed .john/, whether to
// rebuild the environment from it — the moment the developer has just
// decided they want the change is the moment to ask. The watcher would say
// the same a second later; it is told this change has been dealt with.
func (a *app) offerRebuild(ctx context.Context) {
	vm, ok := a.sb.(*sandbox.VM)
	if !ok {
		return
	}
	running, _ := vm.Environment()
	_, now, _, err := sandbox.Environment(a.workdir)
	if err != nil || now == running {
		return
	}
	a.mu.Lock()
	a.envSeen = now
	a.mu.Unlock()
	if err := sandbox.CheckEnvironment(a.workdir); err != nil {
		a.notice(terminalui.NoticeWarn, sandbox.EnvFile+" as applied would not build: "+err.Error())
		return
	}
	if a.confirm(ctx, terminalui.Confirm{
		Title:    "REBUILD ENVIRONMENT",
		Question: "What you applied changes the sandbox's environment. Rebuild it now?",
		Detail: []string{
			"Everything the sandbox has written and you have not applied is kept.",
			"Otherwise the change is used the next time john starts, or when you /rebuild.",
		},
		Choices: []terminalui.Choice{
			{Key: 'r', Label: "rebuild now", Decision: terminalui.DecisionYes},
			{Key: 'l', Label: "later", Decision: terminalui.DecisionNo, Cancel: true},
		},
	}) {
		a.startRebuild(ctx)
		return
	}
	a.mu.Lock()
	a.envChanged = true
	a.mu.Unlock()
	a.status()
}

// rebuild builds the environment again and keeps the work.
//
// It answers an environment that is wrong rather than a sandbox that is wrong:
// a package that half-installed, a base image that moved, a Dockerfile that
// was right and built at a bad moment. Until this existed the only way to
// force a rebuild was /reset, which throws away everything the sandbox has
// written — so the fix for a broken toolchain cost a developer their
// afternoon.
func (a *app) rebuild(ctx context.Context) {
	if _, ok := a.sb.(*sandbox.VM); !ok {
		a.notice(terminalui.NoticeInfo, "this sandbox has no environment to rebuild")
		return
	}
	// The build reads .john/ from the working directory, never from the
	// sandbox. A Dockerfile the agent changed and nobody applied would be
	// left out without a word, and the rebuild would install what was there
	// before — so the changes are shown first, to apply or leave.
	leftOut := a.applyEnvironmentFirst(ctx)
	if err := sandbox.CheckEnvironment(a.workdir); err != nil {
		a.notice(terminalui.NoticeWarn, sandbox.EnvFile+" would not build: "+err.Error())
		return
	}

	file, _, custom, _ := sandbox.Environment(a.workdir)
	from := "the built-in image"
	if custom {
		from = file
	}
	detail := []string{
		"Everything the sandbox has written and you have not applied is kept.",
		"Installed tools and build caches are made again, which takes as long as it did the first time.",
	}
	if leftOut {
		detail = append(detail, "The changes to "+sandbox.EnvDir+"/ you did not apply are not part of this build.")
	}
	if !a.confirm(ctx, terminalui.Confirm{
		Title:    "REBUILD ENVIRONMENT",
		Question: "Build this project's environment again from " + from + "?",
		Detail:   detail,
		Choices: []terminalui.Choice{
			{Key: 'r', Label: "rebuild", Decision: terminalui.DecisionYes},
			{Key: 'k', Label: "keep it", Decision: terminalui.DecisionNo, Cancel: true},
		},
	}) {
		a.notice(terminalui.NoticeInfo, "environment kept")
		return
	}
	a.startRebuild(ctx)
}

// applyEnvironmentFirst offers the sandbox's unapplied changes to .john/ for
// review before a rebuild, and reports whether any were left unapplied.
func (a *app) applyEnvironmentFirst(ctx context.Context) (leftOut bool) {
	pending := a.unappliedEnvironment(ctx)
	if len(pending) == 0 {
		return false
	}
	a.notice(terminalui.NoticeInfo, "the sandbox has changes to "+sandbox.EnvDir+
		"/ you have not applied · the build reads your working directory, so they come first")
	applied := a.applyReviewed(ctx, pending, "apply before rebuilding · "+displayPath(a.workdir))
	return len(applied) < len(pending)
}

// unappliedEnvironment is what the sandbox has changed under .john/ that the
// working directory does not have yet, less anything a policy holds back.
func (a *app) unappliedEnvironment(ctx context.Context) []sandbox.FileDiff {
	diffs, err := a.sb.Diff(ctx)
	if err != nil {
		return nil
	}
	diffs, _ = splitProtected(diffs)
	var out []sandbox.FileDiff
	for _, d := range diffs {
		if touchesEnvironment([]string{d.Path}) {
			out = append(out, d)
		}
	}
	return out
}

// stoppedAnswering is what the model is told about a tool call the sandbox
// never answered: what happened, that it is being dealt with, and what to do.
const stoppedAnswering = "the sandbox stopped answering, and john is restarting it; whatever this " +
	"call was doing was lost. Run it again — the sandbox's files and installed tools are kept."

// sandboxStopped starts the sandbox again after it stopped answering, once,
// however many calls found out. The developer is told once rather than by
// every call that failed, and calls that arrive meanwhile wait for it (see
// awaitSandbox) instead of failing one after another.
func (a *app) sandboxStopped() {
	type restarter interface {
		Restart(context.Context, func(sandbox.Progress)) error
	}
	r, ok := a.sb.(restarter)
	a.mu.Lock()
	if !ok || a.restarting != nil {
		a.mu.Unlock()
		return
	}
	done := make(chan struct{})
	a.restarting = done
	ctx := a.ctx
	a.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	a.notice(terminalui.NoticeWarn, "the sandbox stopped answering · restarting it · what you have not applied is kept")
	a.setSandboxState("sandbox restarting")
	go func() {
		defer func() {
			a.mu.Lock()
			a.restarting = nil
			a.mu.Unlock()
			close(done)
		}()
		progressID := fmt.Sprintf("sandbox-restart-%d", a.nextID())
		err := r.Restart(ctx, func(p sandbox.Progress) {
			a.ui.Send(terminalui.Progress{ID: progressID, Label: p.Label,
				Step: p.Step, Steps: p.Steps, Detail: p.Detail})
		})
		if err != nil {
			a.ui.Send(terminalui.Progress{ID: progressID, Label: "sandbox restart failed", Done: true,
				Status: terminalui.ToolFail, Meta: err.Error()})
			a.setSandboxState("sandbox unavailable")
			return
		}
		a.ui.Send(terminalui.Progress{ID: progressID, Label: "sandbox restarted", Done: true,
			Status: terminalui.ToolOK})
		a.setSandboxState("sandbox ready")
		a.recountUnapplied()
	}()
}

// awaitSandbox holds a tool call while a restart runs, so it runs in the
// sandbox that comes back rather than failing against the one going away.
func (a *app) awaitSandbox(ctx context.Context) {
	a.mu.Lock()
	done := a.restarting
	a.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// startRebuild rebuilds the environment it has already been asked to. Not
// under a running turn: the machine goes down for it, and the agent's next
// command would find no sandbox. So during a turn it waits for the end.
func (a *app) startRebuild(ctx context.Context) {
	vm, ok := a.sb.(*sandbox.VM)
	if !ok {
		return
	}
	if err := sandbox.CheckEnvironment(a.workdir); err != nil {
		a.notice(terminalui.NoticeWarn, "not rebuilt · "+sandbox.EnvFile+" would not build: "+err.Error())
		return
	}
	a.mu.Lock()
	if a.turn != "" {
		a.rebuildAfter = true
		a.mu.Unlock()
		a.notice(terminalui.NoticeInfo, "the environment is rebuilt when this turn ends")
		return
	}
	if a.rebuilding {
		a.mu.Unlock()
		return
	}
	a.rebuilding = true
	a.mu.Unlock()

	a.setSandboxState("sandbox starting")
	progressID := "sandbox-rebuild"
	go func() {
		err := vm.RebuildEnvironment(context.WithoutCancel(ctx), func(p sandbox.Progress) {
			a.ui.Send(terminalui.Progress{ID: progressID, Label: p.Label,
				Step: p.Step, Steps: p.Steps, Detail: p.Detail})
		})
		running, _ := vm.Environment()
		a.mu.Lock()
		a.rebuilding = false
		if err == nil {
			a.envChanged, a.envSeen = false, running
		}
		a.mu.Unlock()
		if err != nil {
			a.ui.Send(terminalui.Progress{ID: progressID, Label: "rebuild failed", Done: true,
				Status: terminalui.ToolFail, Meta: err.Error()})
			a.setSandboxState("sandbox unavailable")
			return
		}
		a.ui.Send(terminalui.Progress{ID: progressID, Label: "environment rebuilt", Done: true})
		a.setSandboxState("sandbox ready")
	}()
}

// reset throws away work that was never applied, so it asks first.
func (a *app) reset(ctx context.Context) {
	diffs, _ := a.sb.Diff(ctx)
	q := terminalui.Confirm{
		Title:    "DISCARD SANDBOX",
		Question: "Reset the sandbox for " + displayPath(a.workdir) + "?",
		Detail: []string{
			"Installed tools, caches and build state are rebuilt on the next run.",
		},
		Choices: []terminalui.Choice{
			{Key: 'd', Label: "discard", Decision: terminalui.DecisionYes, Danger: true},
			{Key: 'k', Label: "keep it", Decision: terminalui.DecisionNo, Cancel: true},
		},
	}
	if len(diffs) > 0 {
		q.Detail = append([]string{
			unappliedDetail(len(diffs)),
		}, q.Detail...)
	}
	if !a.confirm(ctx, q) {
		a.notice(terminalui.NoticeInfo, "sandbox kept")
		return
	}
	if err := a.sb.Reset(ctx); err != nil {
		a.notice(terminalui.NoticeError, "reset failed: "+err.Error())
		return
	}
	a.mu.Lock()
	a.dirty = map[string]bool{}
	a.mu.Unlock()
	a.notice(terminalui.NoticeWarn,
		fmt.Sprintf("sandbox reset · %s discarded", plural(len(diffs), "unapplied file")))
	a.setSandboxState("sandbox starting")
	a.wire.send(&protocol.SandboxState{State: "reset"})
	go func() {
		if err := a.sb.Build(ctx, nil); err == nil {
			a.setSandboxState("sandbox ready")
		} else {
			a.setSandboxState("sandbox unavailable")
		}
	}()
}

// confirm asks a question and blocks until it is answered.
// chooseSandbox asks which of this workspace's sandboxes to carry on with, and
// returns the answer. An empty answer means the developer backed out, which is
// a session that does not start rather than one that starts somewhere they did
// not choose.
func (a *app) chooseSandbox(ctx context.Context, choice sandbox.InstanceChoice) string {
	q := terminalui.Confirm{
		Title:    "CHOOSE SANDBOX",
		Question: choice.Question,
	}
	for i, in := range choice.Instances {
		line := in.ID + " · " + in.Describe
		if in.Busy {
			// Shown because it is why the question is being asked, and not
			// offered because another john has it open.
			q.Detail = append(q.Detail, line+" · in use by another john")
			continue
		}
		q.Detail = append(q.Detail, line)
		key := rune('1' + i)
		if len(in.ID) == 1 {
			key = rune(in.ID[0])
		}
		q.Choices = append(q.Choices, terminalui.Choice{
			Key: key, Label: "sandbox " + in.ID, Decision: in.ID,
		})
	}
	q.Detail = append(q.Detail, "n · a new sandbox, empty, with nothing carried over")
	q.Choices = append(q.Choices,
		terminalui.Choice{Key: 'n', Label: "new sandbox", Decision: sandbox.NewInstance},
		terminalui.Choice{Key: 'q', Label: "quit", Decision: "", Cancel: true})
	return a.choose(ctx, q)
}

// choose is confirm for a question with more than two answers: it returns the
// decision that was picked rather than whether it was the affirmative one.
func (a *app) choose(ctx context.Context, q terminalui.Confirm) string {
	q.ID = fmt.Sprintf("confirm-%d", a.nextID())
	reply := make(chan terminalui.ConfirmDecision, 1)
	a.mu.Lock()
	a.confirms[q.ID] = reply
	a.mu.Unlock()

	a.ui.Send(q)
	select {
	case d := <-reply:
		return d.Decision
	case <-ctx.Done():
		a.ui.Send(terminalui.CancelConfirm{ID: q.ID, Reason: "interrupted"})
		return ""
	}
}

func (a *app) confirm(ctx context.Context, q terminalui.Confirm) bool {
	q.ID = fmt.Sprintf("confirm-%d", a.nextID())
	reply := make(chan terminalui.ConfirmDecision, 1)
	a.mu.Lock()
	a.confirms[q.ID] = reply
	a.mu.Unlock()

	a.ui.Send(q)
	select {
	case d := <-reply:
		return d.Decision == terminalui.DecisionYes
	case <-ctx.Done():
		a.ui.Send(terminalui.CancelConfirm{ID: q.ID, Reason: "interrupted"})
		return false
	}
}

func (a *app) nextID() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	return a.seq
}

// completeFiles walks the workspace for @-mention completion.
func (a *app) completeFiles(query string) []terminalui.Completion {
	q := strings.ToLower(query)
	var out []terminalui.Completion
	filepath.WalkDir(a.workdir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || len(out) >= 200 {
			return filepath.SkipAll
		}
		name := entry.Name()
		if entry.IsDir() {
			if name == ".git" || name == "node_modules" || name == ".john" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(a.workdir, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if q == "" || strings.Contains(strings.ToLower(rel), q) {
			out = append(out, terminalui.Completion{Value: rel})
		}
		return nil
	})
	return out
}

func displayPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

// parseSize reads a whole number, ignoring a trailing unit so `memory=16g`
// and `memory=16` mean the same thing.
func parseSize(v string) (int, error) {
	v = strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), "b"), "g")
	if v == "" {
		return 0, errBadSize
	}
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0, errBadSize
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

type badSizeError struct{}

func (badSizeError) Error() string { return "not a number" }

var errBadSize = badSizeError{}
