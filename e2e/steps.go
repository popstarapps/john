package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/popstarapps/john/sandbox"
	"github.com/popstarapps/john/trace"
)

// The steps past the first run's happy path: what a developer does once the
// app builds, and what goes wrong for them. Each checks the trace, the console
// or the working directory rather than what the model says about itself.

// waitTrace polls the trace until ok holds.
func (r *runner) waitTrace(what string, timeout time.Duration, ok func([]trace.Record) bool) error {
	deadline := time.Now().Add(timeout)
	for {
		// A question john asks while a step waits is answered as it would be
		// during a prompt: left alone, it holds the turn being waited for.
		if r.term != nil && r.cur != nil {
			r.answerPolicyAsk(r.cur)
			r.answerModelChoice(r.cur)
		}
		if ok(r.tr().all()) {
			return nil
		}
		if r.term != nil && r.term.Exited() {
			return fmt.Errorf("john exited (%v) waiting for %s", r.term.err, what)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waited %s for %s", timeout, what)
		}
		time.Sleep(time.Second)
	}
}

// running reports whether a run command containing needle has been called
// after the first skip tool calls and has no result yet.
func running(skip int, needle string) func([]trace.Record) bool {
	return func(recs []trace.Record) bool {
		runs := toolRuns(recs)
		for _, t := range runs[min(skip, len(runs)):] {
			if t.Tool == "run" && strings.Contains(t.Cmd, needle) && t.Status == "no result" {
				return true
			}
		}
		return false
	}
}

// answerModelChoice picks the first model when john asks which one to carry
// on with — the only one offered, in the step that makes it ask.
func (r *runner) answerModelChoice(s *step) {
	lines := strings.Split(r.term.Screen(), "\n")
	bottom := strings.Join(lines[max(0, len(lines)-termHeight):], "\n")
	if !strings.Contains(bottom, "CONTINUE ON") || bottom == r.lastPick {
		return
	}
	r.lastPick = bottom
	s.Notes = append(s.Notes, "john asked which model to continue on; picked the first")
	r.term.Key("\r")
	time.Sleep(time.Second)
}

// restartJohn quits john and starts it again in the same project.
func (r *runner) restartJohn(ctx context.Context) error {
	r.term.Type("/quit")
	quit := r.term.WaitExit(30 * time.Second)
	r.term.Kill()
	r.pastScreens = append(r.pastScreens, r.term.Screen())
	r.term = nil
	if quit != nil {
		return fmt.Errorf("/quit: %w", quit)
	}
	if err := r.start(); err != nil {
		return err
	}
	return r.term.WaitFor(ctx, "sandbox ready", 0, 10*time.Minute)
}

// lastPrompt is the newest prompt the backend received.
func (r *runner) lastPrompt() (promptRec, bool) {
	ps := prompts(r.tr().all())
	if len(ps) == 0 {
		return promptRec{}, false
	}
	return ps[len(ps)-1], true
}

// stepTyping checks that what is typed is what the backend receives: text
// beyond ASCII, a pasted block of several lines, and a long line. Windows
// dropped every character it delivers as an Alt and keypad sequence.
func (r *runner) stepTyping(s *step) error {
	var problems []string
	check := func(what, want string) {
		p, ok := r.lastPrompt()
		switch {
		case !ok:
			problems = append(problems, what+": no prompt reached the backend")
		case p.Text != want:
			problems = append(problems, fmt.Sprintf("%s: typed %q, the backend received %q", what, want, p.Text))
		}
	}

	text := "Reply with only the text between the guillemets, exactly as written: «a—b😀c ✓ naïve ünïcödé»"
	if err := r.prompt(s, text); err != nil {
		return err
	}
	check("text beyond ASCII", text)

	before := len(prompts(r.tr().all()))
	paste := "How many lines does this message have? Reply with just the number.\nsecond line\nthird line"
	s.Notes = append(s.Notes, "› (pasted) "+strings.ReplaceAll(paste, "\n", " ⏎ "))
	r.term.Key("\x1b[200~" + paste + "\x1b[201~")
	time.Sleep(time.Second)
	if err := r.prompt(s, ""); err != nil {
		return err
	}
	if n := len(prompts(r.tr().all())) - before; n != 1 {
		problems = append(problems, fmt.Sprintf("a pasted block of 3 lines arrived as %d prompts", n))
		// Let whatever else it started finish before the next step types.
		r.waitTrace("the paste's turns to end", r.opts.turnTimeout, func(recs []trace.Record) bool {
			return count(recs, "turn.end") >= count(recs, "prompt")
		})
	} else {
		check("a pasted block", paste)
	}

	var long strings.Builder
	long.WriteString("Reply with just the number of the last sentence.")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&long, " Sentence %d is here.", i)
	}
	if err := r.prompt(s, long.String()); err != nil {
		return err
	}
	check(fmt.Sprintf("a %d-character line", long.Len()), long.String())

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// stepInterrupt starts a long command and presses Esc while it runs. The turn
// ends as interrupted, promptly, and the sandbox carries on answering.
func (r *runner) stepInterrupt(s *step) error {
	recs := r.tr().all()
	ends, calls := len(turnEnds(recs)), len(toolRuns(recs))
	text := "Run `sleep 300; echo finished` and tell me what it printed."
	s.Notes = append(s.Notes, "› "+text)
	r.term.Type(text)
	busyMark := r.term.Mark()
	if err := r.waitTrace("`sleep 300` to start", 5*time.Minute, running(calls, "sleep 300")); err != nil {
		return err
	}
	// While it runs, john says it is running something, not thinking.
	if err := r.term.WaitFor(context.Background(), "running ·", busyMark, 10*time.Second); err != nil {
		return fmt.Errorf("the busy line does not say a command is running: %w", err)
	}
	time.Sleep(2 * time.Second)
	pressed := time.Now()
	r.term.Key("\x1b")
	s.Notes = append(s.Notes, "pressed Esc while `sleep 300` ran")
	var end turnEnd
	if err := r.waitTrace("the turn to end after Esc", time.Minute, func(recs []trace.Record) bool {
		e := turnEnds(recs)
		if len(e) > ends {
			end = e[len(e)-1]
			return true
		}
		return false
	}); err != nil {
		return err
	}
	s.Notes = append(s.Notes, fmt.Sprintf("the turn ended %s after Esc: %s",
		time.Since(pressed).Round(time.Second), end.Reason))
	if end.Reason != "interrupted" {
		return fmt.Errorf("the turn ended %q, want interrupted", end.Reason)
	}
	for _, t := range toolRuns(r.tr().all())[calls:] {
		if strings.Contains(t.Cmd, "sleep 300") {
			s.Notes = append(s.Notes, fmt.Sprintf("the interrupted command's result: %s, exit %d", t.Status, t.Exit))
			if t.Status != "cancelled" {
				return fmt.Errorf("the interrupted command is recorded as %q, want cancelled", t.Status)
			}
		}
	}

	// The sandbox still answers, and nothing of the sleep is left running.
	before := len(toolRuns(r.tr().all()))
	if err := r.pursue(s, "`echo still-here` and then `pgrep -x sleep || echo no-sleep` run in the sandbox",
		"Run `echo still-here`, then `pgrep -x sleep || echo no-sleep`, and tell me what each printed.",
		func() bool {
			runs := toolRuns(r.tr().all())[before:]
			return ranOK(runs, "echo still-here") && ranOK(runs, "pgrep -x sleep")
		}, 2); err != nil {
		return err
	}
	for _, t := range toolRuns(r.tr().all())[before:] {
		if strings.Contains(t.Cmd, "pgrep -x sleep") && !strings.Contains(t.Stdout, "no-sleep") {
			return fmt.Errorf("the interrupted sleep is still running in the sandbox: pgrep printed %q",
				strings.TrimSpace(t.Stdout))
		}
	}
	return nil
}

// stepReject has the agent reach a host nobody allowed, files the request
// for it, and rejects that in the console. The host stays unreachable, and
// asking about it again does not file it again.
func (r *runner) stepReject(s *step) error {
	const host = "example.org"
	if err := r.prompt(s, "Run `curl -sS -o /dev/null -w '%{http_code}\\n' https://example.org/` once. "+
		"If network policy blocks it, stop and tell me — don't try anything else."); err != nil {
		return err
	}
	open, err := r.con.openRequests()
	if err != nil {
		return fmt.Errorf("listing access requests: %w", err)
	}
	var req *accessRequest
	for i := range open {
		if open[i].Proposed.Spec.Host == host {
			req = &open[i]
		}
	}
	if req == nil {
		return fmt.Errorf("no access request was filed for %s", host)
	}
	mark := r.term.Mark()
	if err := r.con.reject(*req); err != nil {
		return fmt.Errorf("rejecting %s: %w", req.Ref, err)
	}
	s.Notes = append(s.Notes, "rejected "+req.Ref+" "+host+" in the console")
	// The developer is told, without asking.
	if err := r.term.WaitFor(context.Background(), req.Ref+" rejected", mark, 30*time.Second); err != nil {
		return fmt.Errorf("john did not say the request was rejected: %w", err)
	}

	decided := len(proxyDecisions(r.tr().all()))
	r.declineAsks = true
	err = r.prompt(s, "I rejected the network access request for example.org. Run the same curl command once more "+
		"and tell me what happened. Don't try another way.")
	r.declineAsks = false
	if err != nil {
		return err
	}
	decisions := proxyDecisions(r.tr().all())
	for _, d := range decisions {
		if strings.Contains(d.URL, host) && d.Allowed {
			return fmt.Errorf("a request to %s was allowed: %s", host, d.URL)
		}
	}
	// The trace says why the second attempt was blocked, as the screen does:
	// it said "no policy covers this request" when the screen said the
	// request was rejected, and the agent was told the same and invited to
	// ask again.
	named := false
	for _, d := range decisions[min(decided, len(decisions)):] {
		if strings.Contains(d.URL, host) {
			s.Notes = append(s.Notes, "traced: "+d.Verdict+" · "+d.Policy)
			named = named || (d.Verdict == "denied" && strings.Contains(d.Policy, req.Ref))
		}
	}
	if !named {
		return fmt.Errorf("no traced decision for %s after the rejection is a deny naming %s", host, req.Ref)
	}
	if open, err := r.con.openRequests(); err == nil {
		for _, q := range open {
			if q.Proposed.Spec.Host == host {
				return fmt.Errorf("%s was filed again as %s after it was rejected", host, q.Ref)
			}
		}
	}
	rejected, err := r.con.requests("rejected")
	if err != nil {
		return err
	}
	found := false
	for _, q := range rejected {
		found = found || q.ID == req.ID
	}
	if !found {
		return fmt.Errorf("%s is not listed as rejected", req.Ref)
	}
	for _, l := range strings.Split(r.term.Since(mark), "\n") {
		if strings.Contains(l, host) {
			s.Notes = append(s.Notes, "on screen: "+strings.TrimSpace(l))
		}
	}
	return nil
}

// stepUnreachable allows a host that does not exist, through a policy written
// in the console, and has the agent fetch it. The request is allowed and
// fails, and both the trace and the screen say it failed, not only that it
// was allowed.
func (r *runner) stepUnreachable(s *step) error {
	const host = "john-e2e.invalid"
	if err := r.con.addPolicy("allow "+host, map[string]any{
		"kind": "network", "methods": []string{"GET"}, "scheme": "http", "host": host, "port": nil,
		"path": "/*", "headerMatches": []any{}, "queryMatches": []any{}, "decision": "always-allow",
		"maxBodyBytes": nil,
	}); err != nil {
		return fmt.Errorf("adding the policy: %w", err)
	}
	s.Notes = append(s.Notes, "allowed GET http://"+host+"/* in the console")
	// The backend tells the client its policies changed; give that a moment.
	time.Sleep(5 * time.Second)

	mark := r.term.Mark()
	if err := r.prompt(s, "Run `curl -sS http://john-e2e.invalid/` once and tell me exactly what it printed. "+
		"Don't retry."); err != nil {
		return err
	}
	var got *proxyRec
	for _, d := range proxyDecisions(r.tr().all()) {
		if strings.Contains(d.URL, host) {
			got = &d
		}
	}
	switch {
	case got == nil:
		return fmt.Errorf("no request to %s reached the proxy", host)
	case !got.Allowed:
		return fmt.Errorf("the request was %s, not allowed: the policy did not reach the proxy", got.Verdict)
	case got.Error == "":
		return errors.New("an allowed request that could not reach its host was recorded with no error")
	}
	s.Notes = append(s.Notes, "the trace says: "+got.Error)
	for _, l := range strings.Split(r.term.Since(mark), "\n") {
		if strings.Contains(l, host) && strings.Contains(l, "failed") {
			s.Notes = append(s.Notes, "on screen: "+strings.TrimSpace(l))
			return nil
		}
	}
	return errors.New("the screen does not say the request failed")
}

var modelLine = regexp.MustCompile(`model ([^·\n]+?)\s*(?:·|\n|$)`)

// stepSwitchModel makes a routing policy narrow the session to the model it
// is not on, by a keyword, and checks the conversation carries over: a number
// told to the first model is answered by the second.
func (r *runner) stepSwitchModel(ctx context.Context, s *step) error {
	name2, id2, err := r.con.addModel(r.opts.model2, r.key)
	if err != nil {
		return fmt.Errorf("adding a second model: %w", err)
	}
	r.models[name2] = id2
	s.Notes = append(s.Notes, "added "+name2)

	mark := r.term.Mark()
	r.term.Type("/model")
	if err := r.term.WaitFor(ctx, "model ", mark, 10*time.Second); err != nil {
		return err
	}
	time.Sleep(time.Second)
	m := modelLine.FindAllStringSubmatch(r.term.Since(mark), -1)
	if m == nil {
		return errors.New("/model did not say which model the session is on")
	}
	current := strings.TrimSpace(m[len(m)-1][1])
	target, targetID := "", ""
	for name, id := range r.models {
		if name != current {
			target, targetID = name, id
		}
	}
	if targetID == "" {
		return fmt.Errorf("no model other than %q to switch to (have %v)", current, r.models)
	}
	s.Notes = append(s.Notes, "the session is on "+current+"; narrowing to "+target)

	const keyword = "NIGHTJAR-7"
	if err := r.con.addPolicy("narrow on "+keyword, map[string]any{
		"kind": "model-routing", "mode": "restrict", "modelIds": []string{targetID}, "keywords": []string{keyword},
	}); err != nil {
		return fmt.Errorf("adding the routing policy: %w", err)
	}
	// No restart: a policy written while the session is open applies in it.
	time.Sleep(2 * time.Second)

	if err := r.prompt(s, "Remember the number 4417 for later. Reply with just OK."); err != nil {
		return err
	}
	from := len(r.tr().all())
	mark = r.term.Mark()
	if err := r.prompt(s, "This is project "+keyword+". Reply with just the word ready."); err != nil {
		return err
	}
	recs := r.tr().all()[from:]
	if len(payloadsOf(recs, "model.narrow")) == 0 {
		return errors.New("the keyword did not narrow the session")
	}
	chose := payloadsOf(recs, "model.choice")
	if len(chose) == 0 {
		return errors.New("no model was chosen after the narrowing")
	}
	if got, _ := chose[len(chose)-1]["model"].(string); got != targetID {
		return fmt.Errorf("the session continued on %q, want %s (%s)", got, target, targetID)
	}
	// Said once, with why.
	switched := r.term.Since(mark)
	if n := strings.Count(switched, "switched by a routing policy"); n != 1 {
		return fmt.Errorf("the screen says the model was switched %d times, want once", n)
	}
	if !strings.Contains(switched, keyword) {
		return errors.New("the screen does not say which keyword switched the model")
	}

	from = len(r.tr().all())
	if err := r.prompt(s, "What number did I ask you to remember? Reply with just the number."); err != nil {
		return err
	}
	if said := replies(r.tr().all(), from); !strings.Contains(said, "4417") {
		return fmt.Errorf("the conversation did not carry over the switch: asked for the number, got %q",
			strings.TrimSpace(said))
	}
	mark = r.term.Mark()
	r.term.Type("/model")
	if err := r.term.WaitFor(ctx, "model "+target, mark, 10*time.Second); err != nil {
		return fmt.Errorf("/model after the switch: %w", err)
	}
	return nil
}

var selectedCount = regexp.MustCompile(`(\d+) selected`)

// selected is how many files the open review says are ticked.
func (r *runner) selected(mark int) int {
	m := selectedCount.FindAllStringSubmatch(r.term.Since(mark), -1)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

// openReview types /apply and waits for the review to draw.
func (r *runner) openReview(ctx context.Context) (int, error) {
	mark := r.term.Mark()
	r.term.Type("/apply")
	if err := r.term.WaitFor(ctx, "selected", mark, 30*time.Second); err != nil {
		return mark, fmt.Errorf("the review did not open: %w", err)
	}
	time.Sleep(time.Second)
	return mark, nil
}

// confirmApply presses a, checks the confirmation names want and none of
// notWant, and says yes.
func (r *runner) confirmApply(ctx context.Context, want, notWant []string) error {
	mark := r.term.Mark()
	r.term.Key("a")
	if err := r.term.WaitFor(ctx, "APPLY TO WORKING DIRECTORY", mark, 10*time.Second); err != nil {
		return fmt.Errorf("the review never offered to apply: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	shown := r.term.Since(mark)
	for _, w := range want {
		if !strings.Contains(shown, w) {
			return fmt.Errorf("the confirmation does not name %s", w)
		}
	}
	for _, w := range notWant {
		if strings.Contains(shown, w) {
			return fmt.Errorf("the confirmation names %s, which should not be applied", w)
		}
	}
	mark = r.term.Mark()
	r.term.Key("y")
	// "1 file →" or "3 files →".
	deadline := time.Now().Add(5 * time.Minute)
	for !appliedLine.MatchString(r.term.Since(mark)) {
		if r.term.Exited() || time.Now().After(deadline) {
			return errors.New("apply did not say it had written the files")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil
}

var appliedLine = regexp.MustCompile(`\d+ files? →`)

func (r *runner) hostFile(rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(r.project, filepath.FromSlash(rel)))
	return string(b), err == nil
}

// stepPartialApply writes two files in the sandbox and applies one of them:
// everything unticked, a filter to the one file, ticked, applied.
func (r *runner) stepPartialApply(ctx context.Context, s *step) error {
	if err := r.pursue(s, "two new files written with the edit tool: notes/e2e-a.txt containing alpha, "+
		"and notes/e2e-b.txt containing beta",
		"With the edit tool, create two files: notes/e2e-a.txt containing just the word alpha, and "+
			"notes/e2e-b.txt containing just the word beta. Don't run anything.",
		func() bool { return r.wrote("notes/e2e-a.txt") && r.wrote("notes/e2e-b.txt") }, 2); err != nil {
		return err
	}
	mark, err := r.openReview(ctx)
	if err != nil {
		return err
	}
	for i := 0; i < 2 && r.selected(mark) != 0; i++ {
		r.term.Key("s")
		time.Sleep(700 * time.Millisecond)
	}
	if n := r.selected(mark); n != 0 {
		return fmt.Errorf("could not untick everything: %d selected", n)
	}
	r.term.Key("/")
	for _, c := range "e2e-b" {
		r.term.Key(string(c))
		time.Sleep(30 * time.Millisecond)
	}
	r.term.Key("\r")
	time.Sleep(500 * time.Millisecond)
	r.term.Key("s")
	time.Sleep(700 * time.Millisecond)
	if n := r.selected(mark); n != 1 {
		return fmt.Errorf("filtered to e2e-b and ticked it, but %d are selected", n)
	}
	if err := r.confirmApply(ctx, []string{"e2e-b.txt"}, []string{"e2e-a.txt"}); err != nil {
		return err
	}
	if b, ok := r.hostFile("notes/e2e-b.txt"); !ok || strings.TrimSpace(b) != "beta" {
		return fmt.Errorf("notes/e2e-b.txt in the working directory is %q, want beta", b)
	}
	if _, ok := r.hostFile("notes/e2e-a.txt"); ok {
		return errors.New("notes/e2e-a.txt was applied, though it was not ticked")
	}
	return nil
}

// stepHostEdit changes a file on the host that the sandbox did not touch: the
// sandbox sees it at once, and the next review offers only what is left.
func (r *runner) stepHostEdit(ctx context.Context, s *step) error {
	path := filepath.Join(r.project, "index.html")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("\n<!-- e2e-host-edit -->\n")
	f.Close()
	if err != nil {
		return err
	}
	s.Notes = append(s.Notes, "appended <!-- e2e-host-edit --> to index.html on the host")

	before := len(toolRuns(r.tr().all()))
	if err := r.pursue(s, "`grep -c e2e-host-edit index.html` run in the sandbox, finding the line added on the host",
		"I just edited index.html on my machine. Run `grep -c e2e-host-edit index.html` and tell me the count.",
		func() bool { return ranOK(toolRuns(r.tr().all())[before:], "grep -c e2e-host-edit") }, 2); err != nil {
		return err
	}

	mark, err := r.openReview(ctx)
	if err != nil {
		return err
	}
	if r.selected(mark) == 0 {
		r.term.Key("s")
		time.Sleep(700 * time.Millisecond)
	}
	if err := r.confirmApply(ctx, []string{"e2e-a.txt"}, []string{"index.html", "e2e-b.txt"}); err != nil {
		return err
	}
	if b, ok := r.hostFile("notes/e2e-a.txt"); !ok || strings.TrimSpace(b) != "alpha" {
		return fmt.Errorf("notes/e2e-a.txt in the working directory is %q, want alpha", b)
	}
	if b, _ := r.hostFile("index.html"); !strings.Contains(b, "e2e-host-edit") {
		return errors.New("the host's edit to index.html was lost")
	}
	return nil
}

// stepEnvironment changes .john/Dockerfile on the host. john notices, the
// rebuild installs what was added, and work not yet applied survives it.
func (r *runner) stepEnvironment(ctx context.Context, s *step) error {
	if err := r.pursue(s, "notes/e2e-unapplied.txt written with the edit tool, containing kept",
		"With the edit tool, create notes/e2e-unapplied.txt containing just the word kept. Don't run anything.",
		func() bool { return r.wrote("notes/e2e-unapplied.txt") }, 2); err != nil {
		return err
	}

	mark := r.term.Mark()
	dockerfile := sandbox.StarterEnvironment() +
		"\n# Added by the end-to-end run: the toolchain, and a tool the built-in image lacks.\n" +
		"RUN apt-get update && apt-get install -y nodejs npm tree && rm -rf /var/lib/apt/lists/*\n"
	if err := os.MkdirAll(filepath.Join(r.project, sandbox.EnvDir), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(r.project, filepath.FromSlash(sandbox.EnvFile)), []byte(dockerfile), 0o644); err != nil {
		return err
	}
	s.Notes = append(s.Notes, "wrote "+sandbox.EnvFile+" on the host, adding nodejs, npm and tree")
	if err := r.term.WaitFor(ctx, sandbox.EnvFile+" changed", mark, time.Minute); err != nil {
		return fmt.Errorf("john did not notice: %w", err)
	}

	mark = r.term.Mark()
	r.term.Type("/rebuild")
	if err := r.term.WaitFor(ctx, "REBUILD ENVIRONMENT", mark, 30*time.Second); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	r.term.Key("r")
	deadline := time.Now().Add(30 * time.Minute)
	for {
		since := r.term.Since(mark)
		if strings.Contains(since, "environment rebuilt") {
			break
		}
		if strings.Contains(since, "rebuild failed") {
			return errors.New("the rebuild failed; see the screen")
		}
		if r.term.Exited() || time.Now().After(deadline) {
			return errors.New("the rebuild did not finish")
		}
		time.Sleep(2 * time.Second)
	}
	s.Notes = append(s.Notes, "rebuilt")

	before := len(toolRuns(r.tr().all()))
	return r.pursue(s, "`tree --version`, `node --version` and `cat notes/e2e-unapplied.txt` all run in the sandbox",
		"The environment was rebuilt. Run `tree --version`, `node --version` and `cat notes/e2e-unapplied.txt`, "+
			"and tell me what they printed.",
		func() bool {
			runs := toolRuns(r.tr().all())[before:]
			return ranOK(runs, "tree --version") && ranOK(runs, "node --version") &&
				ranOK(runs, "cat notes/e2e-unapplied.txt")
		}, 2)
}

// stepCrash kills john in the middle of a command, starts it again, and
// resumes the session: the sandbox comes back, the turns number on, and the
// model remembers what it was told before the crash.
func (r *runner) stepCrash(ctx context.Context, s *step) error {
	if err := r.prompt(s, "Remember the word PELICAN for later. Reply with just OK."); err != nil {
		return err
	}
	told, ok := r.lastPrompt()
	if !ok {
		return errors.New("no prompt reached the backend")
	}
	calls := len(toolRuns(r.tr().all()))
	text := "Run `sleep 120; echo done`."
	s.Notes = append(s.Notes, "› "+text)
	r.term.Type(text)
	if err := r.waitTrace("`sleep 120` to start", 5*time.Minute, running(calls, "sleep 120")); err != nil {
		return err
	}
	cut, _ := r.lastPrompt()
	r.term.Kill()
	r.pastScreens = append(r.pastScreens, r.term.Screen())
	r.term = nil
	s.Notes = append(s.Notes, "killed john in the middle of `sleep 120`, in "+told.Session)

	if err := r.start(); err != nil {
		return err
	}
	if err := r.term.WaitFor(ctx, "sandbox ready", 0, 10*time.Minute); err != nil {
		return fmt.Errorf("after the crash: %w", err)
	}
	// john carries on a session the backend stopped in the middle of, by
	// itself: then the turn that was cut off runs again, and is waited for.
	// Otherwise the developer resumes it.
	time.Sleep(10 * time.Second)
	if strings.Contains(r.term.Since(0), "carrying on the turn") {
		s.Notes = append(s.Notes, "john carried on the cut-off turn by itself")
		if err := r.waitTrace("the carried-on turn to end", r.opts.turnTimeout, func(recs []trace.Record) bool {
			n := 0
			for _, e := range turnEnds(recs) {
				if e.Session == cut.Session && e.Turn == cut.Turn {
					n++
				}
			}
			return n > 0
		}); err != nil {
			return err
		}
		time.Sleep(2 * time.Second)
	} else {
		mark := r.term.Mark()
		r.term.Type("/resume " + told.Session)
		if err := r.term.WaitFor(ctx, "resuming", mark, 30*time.Second); err != nil {
			return err
		}
		time.Sleep(5 * time.Second)
		if since := r.term.Since(mark); strings.Contains(since, "could not resume") {
			return fmt.Errorf("could not resume: %s", strings.TrimSpace(since))
		}
	}

	from := len(r.tr().all())
	if err := r.prompt(s, "What word did I ask you to remember? Reply with just the word."); err != nil {
		return err
	}
	p, _ := r.lastPrompt()
	if p.Session != told.Session {
		return fmt.Errorf("the conversation carried on in %s, not the resumed %s", p.Session, told.Session)
	}
	if turnNumber(p.Turn) <= turnNumber(cut.Turn) {
		return fmt.Errorf("the resumed session numbered its turn %s, after %s before the crash", p.Turn, cut.Turn)
	}
	for _, e := range turnEnds(r.tr().all()) {
		if e.Session == cut.Session && e.Turn == cut.Turn {
			s.Notes = append(s.Notes, "the cut-off turn "+cut.Turn+" ended: "+e.Reason)
		}
	}
	// The command the kill cut off has an end, in the trace and on screen.
	for _, t := range toolRuns(r.tr().all())[calls:] {
		if strings.Contains(t.Cmd, "sleep 120") && t.Status != "interrupted" {
			return fmt.Errorf("the cut-off `sleep 120` is recorded as %q, want interrupted", t.Status)
		}
	}
	if !strings.Contains(r.term.Since(0), "cut off when john stopped") {
		return errors.New("the screen does not say the command was cut off")
	}
	if said := replies(r.tr().all(), from); !strings.Contains(strings.ToUpper(said), "PELICAN") {
		return fmt.Errorf("the resumed session did not remember the word: got %q", strings.TrimSpace(said))
	}
	return nil
}

// wrote reports whether the edit tool wrote a file whose path ends in name.
func (r *runner) wrote(name string) bool {
	for _, t := range toolRuns(r.tr().all()) {
		if t.Tool == "edit" && strings.HasSuffix(t.Cmd, name) && t.Status != "failed" {
			return true
		}
	}
	return false
}
