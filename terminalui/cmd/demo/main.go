// Command demo drives terminalui with a fake john: a scripted agent, a fake
// backend whose routing policy narrows the session, a fake proxy that asks for
// approval, and a fake sandbox with changes to review.
//
// It exists to show how the real john binary is meant to talk to the UI, and to
// let the UI be developed and reviewed without any of the rest of the system:
//
//	go run ./terminalui/cmd/demo          # interactive session
//	go run ./terminalui/cmd/demo -script  # starts with a prompt already typed
//	go run ./terminalui/cmd/demo | cat    # the non-tty rendering
//	go run ./terminalui/cmd/demo -plain   # the same, without redirecting
//
// Try: ask anything, then /apply, then /model. Esc interrupts a turn.
//
// The landing page's recording is made from this program, so what it says is
// kept to what the client says (client/app.go, client/wire.go,
// client/proxy.go, client/diff.go): the same notices, the same tool lines, the
// same boxes. Where it would show something the real john does not do, it is
// wrong, however good it looks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/popstarapps/john/terminalui"
)

func main() {
	var (
		plain  = flag.Bool("plain", false, "force the non-interactive renderer")
		ascii  = flag.Bool("ascii", false, "use the ASCII glyph set")
		script = flag.Bool("script", false, "run one scripted turn without waiting for input")
		fast   = flag.Bool("fast", false, "no artificial delays")
	)
	flag.Parse()

	opts := terminalui.Options{
		Placeholder:    "ask john to do something…",
		FileCompletion: true,
		Logo:           true,
		ASCII:          *ascii,
		Commands: []terminalui.Command{
			{Name: "apply", Description: "review the sandbox's changes and apply them"},
			{Name: "reset", Description: "discard the sandbox and start clean"},
			{Name: "model", Description: "show the model this session is routed to"},
			{Name: "help", Description: "list commands"},
			{Name: "quit", Description: "end the session"},
		},
		History: []string{"why does the build fail on arm?", "run the billing tests"},
	}
	if *plain {
		opts.Mode = terminalui.ModePlain
	}

	ui := terminalui.New(opts)
	app := newDemo(ui, *fast)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	app.boot()
	go app.loop(ctx)

	// Without a terminal there is nobody to type, so the demo scripts itself
	// and ends. That is also the shape of `john -p "..." | tee log`.
	scripted := *script || !ui.Interactive()
	if scripted {
		go func() {
			time.Sleep(300 * time.Millisecond)
			app.runTurn(ctx, "the retry in billing/client.go gives up on 429s. fix it and prove it.")
			if !ui.Interactive() {
				app.review(ctx)
				ui.Quit()
			}
		}()
	}

	if err := ui.Run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

// The two models in this story. The session starts on an external one; the
// payments-restricted routing policy narrows it to the internally hosted one
// once card-handling code is about to reach a model.
var (
	externalModel = model{id: "m-sonnet", name: "claude-sonnet-5-5", window: 200_000}
	internalModel = model{id: "m-codegen-l", name: "internal/codegen-l", window: 128_000, internal: true}
)

// routingKeyword is what the routing policy matches. The files that handle
// cardholder data carry it as a marker comment.
const routingKeyword = "PCI-SCOPE"

type model struct {
	id, name string
	window   int
	internal bool
}

// demo is the fake application. In the real john this is the agent loop, the
// sandbox and the proxy; here it is a script.
type demo struct {
	ui   *terminalui.UI
	fast bool

	mu        sync.Mutex
	turn      context.CancelFunc
	seq       int
	approvals map[string]chan terminalui.ApprovalDecision
	confirms  map[string]chan terminalui.ConfirmDecision
	picks     map[string]chan terminalui.ModelDecision
	diffReply chan terminalui.DiffDecision
	model     model
	narrowed  bool
	requested bool // an access request is waiting for an administrator
	tokens    int
	// changed is what the sandbox holds that the working directory does not,
	// path → whether apply offers it ticked.
	changed map[string]bool
}

func newDemo(ui *terminalui.UI, fast bool) *demo {
	return &demo{
		ui: ui, fast: fast, model: externalModel,
		approvals: map[string]chan terminalui.ApprovalDecision{},
		confirms:  map[string]chan terminalui.ConfirmDecision{},
		picks:     map[string]chan terminalui.ModelDecision{},
		changed:   map[string]bool{},
	}
}

// boot says what john says as it starts, in the words the client uses: who is
// signed in, the protections in force, and the sandbox coming up.
func (d *demo) boot() {
	d.ui.Send(
		terminalui.Notice{Text: "signed in as dana@example.com · single sign-on"},
		terminalui.Notice{Text: "workspace protection: 2 path rules · protected paths are not the agent's to change"},
		terminalui.Progress{ID: "sandbox-build", Label: "sandbox ready", Done: true, Status: terminalui.ToolOK},
	)
	d.status()
}

// status is what the client shows on the input box: the model, "N unapplied"
// counting only what apply would offer ticked — a ready sandbox is the normal
// case and says nothing — and how full the model's context is.
func (d *demo) status() {
	d.mu.Lock()
	sb := ""
	n := 0
	for _, ticked := range d.changed {
		if ticked {
			n++
		}
	}
	if n > 0 {
		sb = fmt.Sprintf("%d unapplied · /apply", n)
	}
	st := terminalui.Status{
		Workdir: "~/src/payments",
		Model:   d.model.name,
		Sandbox: sb,
		Tokens:  d.tokens,
		Window:  d.model.window,
	}
	d.mu.Unlock()
	d.ui.Send(st)
}

func (d *demo) nextID(prefix string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	return fmt.Sprintf("%s-%d", prefix, d.seq)
}

func (d *demo) addTokens(n int) {
	d.mu.Lock()
	d.tokens += n
	d.mu.Unlock()
	d.status()
}

// loop is the application side of the boundary: everything the developer does
// arrives here, and nothing else does.
func (d *demo) loop(ctx context.Context) {
	for act := range d.ui.Actions() {
		switch a := act.(type) {
		case terminalui.UserPrompt:
			go d.runTurn(ctx, a.Text)

		case terminalui.SlashCommand:
			go d.runCommand(ctx, a)

		case terminalui.Interrupt:
			d.mu.Lock()
			cancel := d.turn
			d.mu.Unlock()
			if cancel != nil {
				cancel()
			}

		case terminalui.ApprovalDecision:
			d.mu.Lock()
			ch := d.approvals[a.ID]
			delete(d.approvals, a.ID)
			d.mu.Unlock()
			if ch != nil {
				ch <- a
			}

		case terminalui.ConfirmDecision:
			d.mu.Lock()
			ch := d.confirms[a.ID]
			delete(d.confirms, a.ID)
			d.mu.Unlock()
			if ch != nil {
				ch <- a
			}

		case terminalui.ModelDecision:
			d.mu.Lock()
			ch := d.picks[a.ID]
			delete(d.picks, a.ID)
			d.mu.Unlock()
			if ch != nil {
				ch <- a
			}

		case terminalui.DiffDecision:
			d.mu.Lock()
			ch := d.diffReply
			d.diffReply = nil
			d.mu.Unlock()
			if ch != nil {
				ch <- a
			}

		case terminalui.CompletionRequest:
			d.ui.Send(terminalui.Completions{Token: a.Token, Items: completeFiles(a.Query)})
		}
	}
}

func (d *demo) pause(ctx context.Context, dur time.Duration) bool {
	if d.fast {
		dur /= 20
	}
	select {
	case <-time.After(dur):
		return true
	case <-ctx.Done():
		return false
	}
}

// beginTurn makes the turn interruptible: Esc cancels this context, and the
// script unwinds through the same path as any other cancellation.
func (d *demo) beginTurn(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	d.mu.Lock()
	if d.turn != nil {
		d.turn()
	}
	d.turn = cancel
	d.mu.Unlock()
	return ctx, func() {
		interrupted := ctx.Err() != nil
		cancel()
		d.mu.Lock()
		d.turn = nil
		d.mu.Unlock()
		if interrupted {
			d.ui.Send(terminalui.Notice{Level: terminalui.NoticeWarn,
				Text: "interrupted · the sandbox keeps whatever was already written"})
		}
		d.ui.Send(terminalui.Idle{})
	}
}

func (d *demo) runTurn(parent context.Context, prompt string) {
	ctx, done := d.beginTurn(parent)
	defer done()

	d.ui.Send(terminalui.Busy{Label: "thinking"})
	d.addTokens(2100)
	d.think(ctx, "reading billing/client.go to find the retry policy")
	d.read(ctx, "billing/client.go", 6214)

	// What the read returned is about to reach the model, and it carries the
	// routing keyword. The backend holds the request and asks which of the
	// models the policy leaves to carry on with; the conversation goes with
	// the session, whichever it is.
	if !d.narrow(ctx) {
		return
	}

	d.think(ctx, "the 429 branch returns early instead of backing off")
	d.think(ctx, "planning: a failing test first, then honour Retry-After with a capped backoff")
	d.read(ctx, "billing/client_test.go", 3870)
	if ctx.Err() != nil {
		return
	}

	d.stream(ctx, "Found it: the 429 branch hands the response back instead of retrying. "+
		"I'll write a test that shows it first.")
	d.edit(ctx, "billing/client_test.go")

	// The red run. The test helper's telemetry matches an ask-developer
	// policy: held, with the body in hand, until the developer says.
	decision := ""
	d.run(ctx, "go test ./billing/...", 1500*time.Millisecond, func() {
		decision = d.askApproval(ctx)
	}, 1, "--- FAIL: TestRetry429 (0.02s)",
		"client_test.go:88: gave up after 1 attempt, want 4",
		"FAIL")
	if ctx.Err() != nil {
		return
	}

	// A dependency for the backoff. The module proxy is allowed by policy and
	// is one line; Go's checksum database is a host no policy mentions, so it
	// is blocked on the spot, without a question about the request itself —
	// only whether to ask an administrator for a policy.
	d.think(ctx, "the test fails the way the bug says it should; cenkalti/backoff has the backoff")
	d.run(ctx, "go get github.com/cenkalti/backoff/v4", 900*time.Millisecond, func() {
		d.ui.Send(terminalui.ProxyDecision{
			Method:  "GET",
			URL:     "https://proxy.golang.org/github.com/cenkalti/backoff/v4/@v/v4.3.0.zip",
			Verdict: "allowed",
			Allowed: true,
		})
		d.pause(ctx, 250*time.Millisecond)
		d.ui.Send(terminalui.ProxyDecision{
			Method:  "GET",
			URL:     sumURL,
			Verdict: "blocked · no matching policy",
		})
		d.requestPolicy(ctx)
	}, 1, "go: github.com/cenkalti/backoff/v4@v4.3.0: verifying module: reading "+
		sumURL+": 403 Forbidden")
	if ctx.Err() != nil {
		return
	}

	d.stream(ctx, "The sandbox can't reach `sum.golang.org`, so the dependency can't be "+
		"verified. A capped, jittered backoff is a dozen lines; I'll write it rather than wait.")
	d.edit(ctx, "billing/client.go")
	d.run(ctx, "go test -coverprofile=coverage/billing.out ./billing/...", 1300*time.Millisecond, nil, 0)
	d.mu.Lock()
	d.changed["coverage/billing.out"] = false // .gitignore'd: apply offers it unticked
	d.mu.Unlock()
	if ctx.Err() != nil {
		return
	}

	summary := "429 is now retryable, with `Retry-After` honoured and the backoff capped at 30s.\n\n" +
		"- `billing/client_test.go` — `TestRetry429` failed before the fix and passes after it\n" +
		"- `billing/client.go` — the retry loop handles 429 and 503, with jittered backoff\n\n"
	switch decision {
	case terminalui.DecisionAllowOnce:
		summary += "You allowed the test helper's telemetry POST once; I did not repeat it."
	default:
		summary += "The test helper tried to post telemetry. You rejected it, and the tests " +
			"do not need it."
	}
	d.mu.Lock()
	filed := d.requested
	d.mu.Unlock()
	if filed {
		summary += "\n\nThe backoff is written by hand, so nothing waits on the request you " +
			"filed for `sum.golang.org`."
	}
	summary += "\n\nNothing has reached your working directory yet — run `/apply` to review."

	d.stream(ctx, summary)
	d.addTokens(1900)
}

func (d *demo) think(ctx context.Context, text string) {
	if ctx.Err() != nil {
		return
	}
	d.ui.Send(terminalui.Thinking{Text: text})
	d.pause(ctx, 1200*time.Millisecond)
}

// narrow is the backend's routing policy acting mid-turn, the first time the
// keyword reaches it: the ModelChoice the client shows for a ModelNarrow, in
// the backend's words. False means the turn is over.
func (d *demo) narrow(ctx context.Context) bool {
	d.mu.Lock()
	already := d.narrowed
	d.mu.Unlock()
	if already || ctx.Err() != nil {
		return ctx.Err() == nil
	}
	id := d.nextID("narrow")
	reply := make(chan terminalui.ModelDecision, 1)
	d.mu.Lock()
	d.picks[id] = reply
	d.mu.Unlock()
	d.ui.Send(terminalui.ModelChoice{
		ID:     id,
		Reason: fmt.Sprintf("keyword %q seen in what was about to reach a model", routingKeyword),
		Policy: "narrowed by policy group payments-restricted",
		Models: []terminalui.ModelOption{
			{ID: internalModel.id, Name: internalModel.name, Internal: internalModel.internal},
		},
	})
	select {
	case dec := <-reply:
		if dec.Cancelled {
			return false
		}
	case <-ctx.Done():
		d.ui.Send(terminalui.CancelModelChoice{ID: id, Reason: "turn interrupted"})
		return false
	}
	d.mu.Lock()
	d.narrowed = true
	d.model = internalModel
	d.mu.Unlock()
	d.status()
	return true
}

// sumURL is the checksum lookup `go get` makes, and no policy covers.
const sumURL = "https://sum.golang.org/lookup/github.com/cenkalti/backoff/v4@v4.3.0"

// requestRef is the access request the developer files for it.
const requestRef = "REQ-117"

// requestPolicy is client/netreq.go's question after a request no policy
// covers was blocked: whether to ask an administrator for a policy, for the
// whole host or the one path. The request itself stays blocked either way.
func (d *demo) requestPolicy(ctx context.Context) {
	id := d.nextID("ask-policy")
	reply := make(chan terminalui.ConfirmDecision, 1)
	d.mu.Lock()
	d.confirms[id] = reply
	d.mu.Unlock()
	d.ui.Send(terminalui.Confirm{
		ID:       id,
		Title:    "REQUEST A POLICY",
		Question: "Ask an administrator to allow sum.golang.org?",
		Detail: []string{
			"GET " + sumURL,
			"This request was blocked. Filing a request does not unblock it — it asks for a policy for next time.",
			"The whole host is what most tools need; anything else blocked on it while the request is open joins it.",
		},
		Choices: []terminalui.Choice{
			{Key: 'h', Label: "the whole host", Decision: "host", Default: true},
			{Key: 'p', Label: "only /lookup/github.com/cenkalti/backoff/v4@v4.3.0", Decision: "path"},
			{Key: 'n', Label: "no", Decision: "no", Cancel: true},
		},
	})
	var dec terminalui.ConfirmDecision
	select {
	case dec = <-reply:
	case <-ctx.Done():
		d.ui.Send(terminalui.CancelConfirm{ID: id, Reason: "interrupted"})
		return
	}
	switch dec.Decision {
	case "host":
		d.filed("sum.golang.org")
	case "path":
		d.filed("sum.golang.org/lookup/github.com/cenkalti/backoff/v4@v4.3.0")
	}
}

func (d *demo) filed(what string) {
	d.mu.Lock()
	d.requested = true
	d.mu.Unlock()
	d.ui.Send(terminalui.Notice{Text: "network access request " + requestRef + " filed for " + what +
		" · an administrator will decide it"})
}

// decide is an administrator answering the request in the console, which the
// session hears about as it happens: client/proxy.go's accessDecided.
func (d *demo) decide() {
	d.mu.Lock()
	filed := d.requested
	d.requested = false
	d.mu.Unlock()
	if filed {
		d.ui.Send(terminalui.Notice{Text: requestRef + " approved · sum.golang.org"})
	}
}

// askApproval sends an ApprovalRequest and blocks until the developer decides.
// This is the pattern for any request/response over the boundary.
func (d *demo) askApproval(ctx context.Context) string {
	id := d.nextID("approval")
	reply := make(chan terminalui.ApprovalDecision, 1)
	d.mu.Lock()
	d.approvals[id] = reply
	d.mu.Unlock()

	d.ui.Send(terminalui.ApprovalRequest{
		ID:     id,
		Method: "POST",
		URL:    "https://telemetry.vendor.io/v1/events",
		Headers: []terminalui.Header{
			{Name: "content-type", Value: "application/json"},
			{Name: "user-agent", Value: "vendor-testkit/2.3.0 (linux/arm64)"},
			{Name: "content-length", Value: "219"},
		},
		Body: `{"project":"payments","event":"tests.started","duration_ms":3140,` +
			`"host":"sandbox-payments","user":"dana@example.com","packages":` +
			`["billing","ledger","internal/secrets"],"git_remote":` +
			`"git@github.com:example/payments.git"}`,
		Verdict: "ask developer",
		Policy:  "this request matched an ask-developer policy",
		Options: terminalui.DefaultApprovalOptions(),
	})

	select {
	case dec := <-reply:
		return dec.Decision
	case <-ctx.Done():
		d.ui.Send(terminalui.CancelApproval{ID: id, Reason: "turn interrupted"})
		return terminalui.DecisionReject
	}
}

// read, edit and run draw a tool call the way client/wire.go does: a read says
// how many bytes it returned, an edit says nothing, and a command says how
// long it took or — when it failed — why, and its exit code.
func (d *demo) read(ctx context.Context, path string, size int) {
	if ctx.Err() != nil {
		return
	}
	id := d.nextID("read")
	d.ui.Send(terminalui.ToolStart{ID: id, Verb: "read", Target: path})
	if !d.pause(ctx, 350*time.Millisecond) {
		d.ui.Send(terminalui.ToolEnd{ID: id, Status: terminalui.ToolSkipped, Meta: "cancelled"})
		return
	}
	d.ui.Send(terminalui.ToolEnd{ID: id, Status: terminalui.ToolOK, Meta: fmt.Sprintf("%d bytes", size)})
	d.addTokens(size / 4)
}

func (d *demo) edit(ctx context.Context, path string) {
	if ctx.Err() != nil {
		return
	}
	id := d.nextID("edit")
	d.ui.Send(terminalui.ToolStart{ID: id, Verb: "edit", Target: path})
	if !d.pause(ctx, 700*time.Millisecond) {
		d.ui.Send(terminalui.ToolEnd{ID: id, Status: terminalui.ToolSkipped, Meta: "cancelled"})
		return
	}
	d.ui.Send(terminalui.ToolEnd{ID: id, Status: terminalui.ToolOK})
	d.mu.Lock()
	d.changed[path] = true
	d.mu.Unlock()
	d.addTokens(600)
}

// run runs a command; during, if set, is what happens on the network while it
// does. A non-zero exit shows the reason it failed — the first lines of what
// it wrote, as client/wire.go's failureLines picks them.
func (d *demo) run(ctx context.Context, cmd string, dur time.Duration, during func(), exit int, reason ...string) {
	if ctx.Err() != nil {
		return
	}
	id := d.nextID("run")
	d.ui.Send(terminalui.ToolStart{ID: id, Verb: "run", Target: cmd})
	d.ui.Send(terminalui.BusyLabel{Label: "running"})
	defer d.ui.Send(terminalui.BusyLabel{Label: "thinking"})
	start := time.Now()
	if !d.pause(ctx, dur/2) {
		d.ui.Send(terminalui.ToolEnd{ID: id, Status: terminalui.ToolSkipped, Meta: "cancelled"})
		return
	}
	if during != nil {
		during()
	}
	if !d.pause(ctx, dur/2) {
		d.ui.Send(terminalui.ToolEnd{ID: id, Status: terminalui.ToolSkipped, Meta: "cancelled"})
		return
	}
	if exit == 0 {
		took := time.Since(start)
		if during != nil {
			took = dur // the time spent deciding was not the command's
		}
		d.ui.Send(terminalui.ToolEnd{ID: id, Status: terminalui.ToolOK,
			Meta: fmt.Sprintf("%.1fs", took.Seconds())})
	} else {
		for _, line := range reason {
			d.ui.Send(terminalui.ToolDetail{ID: id, Text: line})
		}
		d.ui.Send(terminalui.ToolEnd{ID: id, Status: terminalui.ToolFail, Meta: fmt.Sprintf("exit %d", exit)})
	}
	d.addTokens(400)
}

// stream sends a message the way a model produces one: in fragments.
func (d *demo) stream(ctx context.Context, text string) {
	if ctx.Err() != nil {
		return
	}
	id := d.nextID("msg")
	d.ui.Send(terminalui.AssistantStart{ID: id})
	for _, word := range splitKeepingSpace(text) {
		if ctx.Err() != nil {
			break
		}
		d.ui.Send(terminalui.AssistantDelta{ID: id, Text: word})
		d.pause(ctx, 32*time.Millisecond)
	}
	d.ui.Send(terminalui.AssistantEnd{ID: id})
}

func splitKeepingSpace(s string) []string {
	fields := strings.SplitAfter(s, " ")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func (d *demo) runCommand(ctx context.Context, cmd terminalui.SlashCommand) {
	switch cmd.Name {
	case "quit", "exit":
		d.ui.Quit()

	case "help":
		d.ui.Send(terminalui.AssistantMessage{Text: "**Commands**\n\n" +
			"- `/apply` — review what the sandbox changed and write it to your working directory\n" +
			"- `/reset` — discard the sandbox entirely, unapplied changes included\n" +
			"- `/model` — the model this session is routed to\n" +
			"- `/quit` — end the session\n\n" +
			"`@` mentions a file, `ctrl+r` searches history, Esc interrupts a turn. " +
			"Type while john works and your message is queued."})

	case "model":
		d.mu.Lock()
		text := "model " + d.model.name
		if d.narrowed {
			text += " · switched by a routing policy for this session"
		}
		d.mu.Unlock()
		d.ui.Send(terminalui.Notice{Text: text})

	case "reset":
		d.mu.Lock()
		d.changed = map[string]bool{}
		d.mu.Unlock()
		d.ui.Send(terminalui.Notice{Level: terminalui.NoticeWarn, Text: "sandbox reset"})
		d.status()

	case "apply":
		d.review(ctx)

	default:
		d.ui.Send(terminalui.Notice{Level: terminalui.NoticeError,
			Text: "unknown command: /" + cmd.Name})
	}
}

// review is client/app.go's: say what is left unticked and why, open the
// review, confirm the write, and apply.
func (d *demo) review(ctx context.Context) {
	d.mu.Lock()
	changed := make(map[string]bool, len(d.changed))
	for p, t := range d.changed {
		changed[p] = t
	}
	d.mu.Unlock()
	if len(changed) == 0 {
		d.ui.Send(terminalui.Notice{Text: "nothing to apply · the sandbox matches your working directory"})
		return
	}

	if ticked, ok := changed["coverage/billing.out"]; ok && !ticked {
		d.ui.Send(terminalui.Notice{Text: "1 file not selected (coverage/) · ignored by .gitignore · " +
			"tick them in the review to apply them too"})
	}

	id := d.nextID("review")
	reply := make(chan terminalui.DiffDecision, 1)
	d.mu.Lock()
	d.diffReply = reply
	d.mu.Unlock()
	d.ui.Send(sampleDiff(id, changed))

	var dec terminalui.DiffDecision
	select {
	case dec = <-reply:
	case <-ctx.Done():
		d.ui.Send(terminalui.CancelDiffReview{ID: id})
		return
	}
	if !dec.Apply || !d.confirm(ctx, terminalui.Confirm{
		Title:    "APPLY TO WORKING DIRECTORY",
		Question: fmt.Sprintf("Write %s into ~/src/payments?", plural(len(dec.Paths), "file")),
		Detail:   dec.Paths,
	}) {
		d.ui.Send(terminalui.Notice{Text: "not applied · everything stays in the sandbox"})
		return
	}

	tool := d.nextID("apply")
	d.ui.Send(terminalui.ToolStart{ID: tool, Verb: "apply", Targets: dec.Paths})
	d.pause(ctx, 500*time.Millisecond)
	d.ui.Send(terminalui.ToolEnd{ID: tool, Status: terminalui.ToolOK,
		Meta: plural(len(dec.Paths), "file") + " → payments"})
	d.mu.Lock()
	for _, p := range dec.Paths {
		delete(d.changed, p)
	}
	d.mu.Unlock()
	d.status()

	// Somewhere else, meanwhile, an administrator got to the request.
	if d.pause(ctx, 1500*time.Millisecond) {
		d.decide()
	}
}

func (d *demo) confirm(ctx context.Context, q terminalui.Confirm) bool {
	q.ID = d.nextID("confirm")
	reply := make(chan terminalui.ConfirmDecision, 1)
	d.mu.Lock()
	d.confirms[q.ID] = reply
	d.mu.Unlock()
	d.ui.Send(q)
	select {
	case dec := <-reply:
		return dec.Decision == terminalui.DecisionYes
	case <-ctx.Done():
		d.ui.Send(terminalui.CancelConfirm{ID: q.ID, Reason: "interrupted"})
		return false
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// sampleDiff is what the sandbox holds, as client/diff.go renders it: in path
// order, the files apply offers ticked first and the ones it leaves unticked
// after them.
func sampleDiff(id string, changed map[string]bool) terminalui.DiffReview {
	all := []terminalui.DiffFile{
		{
			Path: "billing/client.go", Added: 11, Removed: 3,
			Hunks: []terminalui.DiffHunk{{
				Header: "@@ -18,7 +18,15 @@ func (c *Client) do(req *http.Request)",
				Lines: []terminalui.DiffLine{
					{Kind: terminalui.DiffContext, Text: "	resp, err := c.http.Do(req)"},
					{Kind: terminalui.DiffContext, Text: "	if err != nil {"},
					{Kind: terminalui.DiffContext, Text: "		return nil, err"},
					{Kind: terminalui.DiffRemove, Text: "	if resp.StatusCode == http.StatusTooManyRequests {"},
					{Kind: terminalui.DiffRemove, Text: "		return resp, nil // caller decides"},
					{Kind: terminalui.DiffRemove, Text: "	}"},
					{Kind: terminalui.DiffAdd, Text: "	if retryable(resp.StatusCode) {"},
					{Kind: terminalui.DiffAdd, Text: "		delay := retryAfter(resp.Header)"},
					{Kind: terminalui.DiffAdd, Text: "		if delay <= 0 {"},
					{Kind: terminalui.DiffAdd, Text: "			delay = backoff(attempt)"},
					{Kind: terminalui.DiffAdd, Text: "		}"},
					{Kind: terminalui.DiffAdd, Text: "		if delay > maxRetryDelay {"},
					{Kind: terminalui.DiffAdd, Text: "			delay = maxRetryDelay"},
					{Kind: terminalui.DiffAdd, Text: "		}"},
					{Kind: terminalui.DiffAdd, Text: "		time.Sleep(jitter(delay))"},
					{Kind: terminalui.DiffAdd, Text: "		continue"},
					{Kind: terminalui.DiffAdd, Text: "	}"},
					{Kind: terminalui.DiffContext, Text: "	return resp, nil"},
				},
			}},
		},
		{
			Path: "billing/client_test.go", Added: 21, Removed: 0,
			Hunks: []terminalui.DiffHunk{{
				Header: "@@ -80,6 +80,27 @@ func TestRetry(t *testing.T)",
				Lines: []terminalui.DiffLine{
					{Kind: terminalui.DiffAdd, Text: "func TestRetry429(t *testing.T) {"},
					{Kind: terminalui.DiffAdd, Text: "	var attempts int"},
					{Kind: terminalui.DiffAdd, Text: "	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {"},
					{Kind: terminalui.DiffAdd, Text: "		attempts++"},
					{Kind: terminalui.DiffAdd, Text: "		if attempts < 4 {"},
					{Kind: terminalui.DiffAdd, Text: "			w.Header().Set(\"Retry-After\", \"1\")"},
					{Kind: terminalui.DiffAdd, Text: "			w.WriteHeader(http.StatusTooManyRequests)"},
					{Kind: terminalui.DiffAdd, Text: "			return"},
					{Kind: terminalui.DiffAdd, Text: "		}"},
					{Kind: terminalui.DiffAdd, Text: "	}))"},
					{Kind: terminalui.DiffAdd, Text: "	defer srv.Close()"},
				},
			}},
		},
		{
			Path: "coverage/billing.out", Added: 46, Removed: 0,
			Hunks: []terminalui.DiffHunk{{
				Header: "@@ -0,0 +1,46 @@",
				Lines: []terminalui.DiffLine{
					{Kind: terminalui.DiffAdd, Text: "mode: set"},
					{Kind: terminalui.DiffAdd, Text: "payments/billing/client.go:18.45,20.16 2 1"},
					{Kind: terminalui.DiffAdd, Text: "payments/billing/client.go:20.16,22.4 1 0"},
				},
			}},
		},
	}
	rev := terminalui.DiffReview{ID: id, Title: "apply to ~/src/payments"}
	var unticked []terminalui.DiffFile
	for _, f := range all {
		ticked, ok := changed[f.Path]
		if !ok {
			continue
		}
		f.Selected = ticked
		if ticked {
			rev.Files = append(rev.Files, f)
		} else {
			unticked = append(unticked, f)
		}
	}
	rev.Files = append(rev.Files, unticked...)
	return rev
}

var workspaceFiles = []string{
	"billing/client.go",
	"billing/client_test.go",
	"billing/retry.go",
	"ledger/posting.go",
	"ledger/posting_test.go",
	"internal/secrets/vault.go",
	"cmd/payments/main.go",
	"Dockerfile",
	"go.mod",
	"readme.md",
}

func completeFiles(query string) []terminalui.Completion {
	q := strings.ToLower(query)
	out := make([]terminalui.Completion, 0, len(workspaceFiles))
	for _, f := range workspaceFiles {
		if q != "" && !strings.Contains(strings.ToLower(f), q) {
			continue
		}
		detail := "file"
		if strings.HasSuffix(f, "_test.go") {
			detail = "test"
		}
		out = append(out, terminalui.Completion{Value: f, Detail: detail})
	}
	return out
}
