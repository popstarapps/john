// Command e2e runs john the way a developer meets it for the first time, and
// writes down everything that happened.
//
// It is the loop that kept finding bugs when it was done by hand: a fresh
// machine, standalone, a model added, the sandbox built, a React app asked for,
// Node and its packages installed through network access the developer
// approves, the build run — then john restarted, the build run again, and the
// result applied to the working directory. Each of those is a step with a
// check that does not depend on what the model says about itself: the trace
// the backend wrote says whether `npm run build` exited 0, and the working
// directory says whether apply wrote the files.
//
// john runs in a real pseudo-terminal, so what it draws is what a developer
// would see; the screen goes in the report. At the end `claude -p` reads the
// run — the screen, the decoded trace, the guest console — with the source
// beside it, and writes up what looks wrong with john (not with the model).
//
//	ANTHROPIC_API_KEY=sk-ant-… go run ./e2e
//	ANTHROPIC_API_KEY=sk-ant-… go run ./e2e -build -model sonnet
//
// Everything goes in e2e/runs/<time>/. The run uses a JOHN_HOME of its own, so
// the developer's own ~/.john — sandboxes included — is never touched.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

type options struct {
	john        string
	build       bool
	model       string
	model2      string
	turnTimeout time.Duration
	review      string
	driver      string
	keepHome    bool
	repo        string
}

func main() {
	var o options
	flag.StringVar(&o.john, "john", "", "the john binary to test (default: the newest full build for this platform in dist/)")
	flag.BoolVar(&o.build, "build", false, "run `make release` first")
	flag.StringVar(&o.model, "model", "haiku", "the Anthropic model to add, matched against the catalog's ids")
	flag.StringVar(&o.model2, "model2", "sonnet", "the second model, which a routing policy switches the session to")
	flag.DurationVar(&o.turnTimeout, "turn-timeout", 20*time.Minute, "how long one turn may take")
	flag.StringVar(&o.review, "review", "auto", "have `claude -p` review the run: auto (when claude is on PATH), yes, no")
	flag.StringVar(&o.driver, "driver", "sonnet", "the model `claude -p` plays the developer with when a step needs another message; none always says yes")
	flag.BoolVar(&o.keepHome, "keep-home", false, "keep the run's JOHN_HOME (sandbox disks included) instead of deleting it")
	flag.StringVar(&o.repo, "repo", ".", "the john repository: where dist/ is, and what the reviewer reads")
	flag.Parse()

	code, err := run(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
	}
	os.Exit(code)
}

func run(o options) (int, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return 2, errors.New("set ANTHROPIC_API_KEY: the run adds a model with it, as a developer would in the console")
	}
	repo, err := filepath.Abs(o.repo)
	if err != nil {
		return 2, err
	}
	if o.build {
		if runtime.GOOS == "windows" {
			return 2, errors.New("-build needs make and a shell; build on a Mac or Linux and pass -john")
		}
		fmt.Println("== make release")
		cmd := exec.Command("make", "release")
		cmd.Dir, cmd.Stdout, cmd.Stderr = repo, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return 2, fmt.Errorf("make release: %w", err)
		}
	}
	bin, err := findJohn(o.john, repo)
	if err != nil {
		return 2, err
	}
	// Standalone answers on one fixed port, so a john already running here
	// would be the one the run talked to.
	if ln, err := net.Listen("tcp", "127.0.0.1:7171"); err != nil {
		return 2, errors.New("127.0.0.1:7171 is taken — close any running john or `john console` first")
	} else {
		ln.Close()
	}

	stamp := time.Now().Format("2006-01-02_150405")
	out := filepath.Join(repo, "e2e", "runs", stamp)
	home := filepath.Join(os.TempDir(), "john-e2e-"+stamp)
	project := filepath.Join(out, "project")
	for _, d := range []string{out, home, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return 2, err
		}
	}

	r := &runner{
		opts: o, bin: bin, out: out, home: home, project: project, key: key,
		env: johnEnv(home), tr: openTracesLater(home), started: time.Now(), models: map[string]string{},
	}
	fmt.Printf("john:    %s\nrun:     %s\nhome:    %s\n\n", bin, out, home)

	ctx := context.Background()
	r.scenario(ctx)

	if r.term != nil {
		r.term.Kill()
	}
	r.collect()
	r.writeReport()
	fmt.Printf("\nreport: %s\n", filepath.Join(out, "report.md"))

	if r.wantReview() {
		fmt.Println("\n== claude -p is reviewing the run")
		if err := r.reviewRun(repo); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: review:", err)
		} else {
			fmt.Printf("review: %s\n", filepath.Join(out, "review.md"))
		}
	}

	if !o.keepHome {
		os.RemoveAll(home)
	}
	if r.failed() {
		return 1, nil
	}
	return 0, nil
}

// johnEnv is the environment john runs in: this run's home, no browser to
// open, a terminal it can draw colour on — and no Anthropic key. john does not
// read one, and `claude -p` would, billing the review to it instead of the
// account it is signed in with.
func johnEnv(home string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "ANTHROPIC_API_KEY", "JOHN_HOME", "JOHN_NO_BROWSER", "TERM":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "JOHN_HOME="+home, "JOHN_NO_BROWSER=1", "TERM=xterm-256color")
}

func findJohn(given, repo string) (string, error) {
	if given != "" {
		return filepath.Abs(given)
	}
	pattern := fmt.Sprintf("john-*-full-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		pattern += ".exe"
	}
	matches, _ := filepath.Glob(filepath.Join(repo, "dist", pattern))
	if len(matches) == 0 {
		return "", fmt.Errorf("no %s in %s — run with -build, or pass -john", pattern, filepath.Join(repo, "dist"))
	}
	sort.Slice(matches, func(i, j int) bool {
		a, _ := os.Stat(matches[i])
		b, _ := os.Stat(matches[j])
		return a.ModTime().After(b.ModTime())
	})
	return matches[0], nil
}

// step is one thing the run did and whether it held.
type step struct {
	Name  string
	OK    bool
	Took  time.Duration
	Notes []string
	Err   string
}

type runner struct {
	opts                    options
	bin, out, home, project string
	key                     string
	env                     []string
	term                    *term
	con                     *console
	tr                      func() *traces
	steps                   []*step
	approved                []string
	started                 time.Time
	sessions                int
	pastScreens             []string
	lastAsk                 string
	stop                    bool

	// declineAsks answers "REQUEST A POLICY" with no instead of the whole
	// host, for a step that has just had a host rejected and must not file
	// it again.
	declineAsks bool
	lastPick    string
	models      map[string]string // name to console id, of the models the run added
	cur         *step             // the step running, for what is answered while waiting
}

func openTracesLater(home string) func() *traces {
	// The keys are written when standalone first starts, so they are read
	// each time rather than once before they exist.
	return func() *traces { return openTraces(home) }
}

func (r *runner) failed() bool {
	for _, s := range r.steps {
		if !s.OK {
			return true
		}
	}
	return false
}

// do runs one step. A step that fails stops the run when fatal is set:
// nothing after "the sandbox never came up" can tell anyone anything.
func (r *runner) do(name string, fatal bool, fn func(s *step) error) bool {
	if r.halted() {
		return false
	}
	fmt.Printf("── %s\n", name)
	s := &step{Name: name}
	r.steps = append(r.steps, s)
	r.cur = s
	start := time.Now()
	err := func() (err error) {
		// A bug in a step is a failed step, not a lost run: the report and
		// the cleanup after it still happen.
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("the harness panicked: %v", p)
			}
		}()
		return fn(s)
	}()
	s.Took = time.Since(start).Round(time.Second)
	s.OK = err == nil
	if err != nil {
		s.Err = err.Error()
		fmt.Printf("   ✗ %s (%s)\n", err, s.Took)
		if fatal {
			s.Notes = append(s.Notes, "the run stopped here")
		}
	} else {
		fmt.Printf("   ✓ %s\n", s.Took)
	}
	for _, n := range s.Notes {
		fmt.Printf("     %s\n", n)
	}
	if err != nil && fatal {
		r.stop = true
	}
	return err == nil
}

func (r *runner) halted() bool { return r.stop }

// start opens a john session in the project, in a terminal of its own.
func (r *runner) start() error {
	r.sessions++
	t, err := startTerm(r.bin, r.project, r.env, filepath.Join(r.out, fmt.Sprintf("terminal-%d.raw", r.sessions)))
	if err != nil {
		return err
	}
	r.term = t
	return nil
}

// linkLost is what john says when the sandbox stops answering. Not "EOF",
// which is what it said before it said anything better: every heredoc the
// agent writes ends in one.
var linkLost = regexp.MustCompile(`the sandbox stopped answering`)

// refusedPrompt is what john says when a prompt goes nowhere.
var refusedPrompt = regexp.MustCompile(`not connected yet[^\n]*|the backend refused the session[^\n]*|prompts are refused[^\n]*`)

// prompt types a message and waits for the turn it starts to end, by the
// trace rather than by the screen.
func (r *runner) prompt(s *step, text string) error {
	before := count(r.tr().all(), "turn.end")
	mark := r.term.Mark()
	s.Notes = append(s.Notes, "› "+text)
	r.term.Type(text)

	deadline := time.Now().Add(r.opts.turnTimeout)
	for {
		r.answerPolicyAsk(s)
		r.answerModelChoice(s)
		if count(r.tr().all(), "turn.end") > before {
			// The ask can come up just as the turn ends: give it a moment,
			// rather than typing the next prompt into it.
			time.Sleep(2 * time.Second)
			r.answerPolicyAsk(s)
			r.answerModelChoice(s)
			break
		}
		if r.term.Exited() {
			return fmt.Errorf("john exited during the turn (%v)", r.term.err)
		}
		// A prompt john refused starts no turn, so there is no end to wait
		// for: say so now rather than at the end of the timeout.
		if m := refusedPrompt.FindString(r.term.Since(mark)); m != "" {
			return fmt.Errorf("john refused the prompt: %q", m)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the turn did not end within %s", r.opts.turnTimeout)
		}
		time.Sleep(2 * time.Second)
	}
	if m := linkLost.FindString(r.term.Since(mark)); m != "" {
		r.stop = true
		return fmt.Errorf("the sandbox stopped answering during the turn (the screen says %q)", m)
	}
	return nil
}

// answerPolicyAsk answers john's "REQUEST A POLICY" question when it is on
// screen, the way a developer who wants the work done would: ask for the
// whole host. The request it files is what promptApproving then approves.
//
// It has to be answered rather than typed past. The question takes single
// keys, so the next prompt typed into it answers it a letter at a time — the
// "n" in "Run `node`" is "no", and nothing on screen says so.
func (r *runner) answerPolicyAsk(s *step) {
	lines := strings.Split(r.term.Screen(), "\n")
	bottom := strings.Join(lines[max(0, len(lines)-termHeight):], "\n")
	if !strings.Contains(bottom, "REQUEST A POLICY") || bottom == r.lastAsk {
		// Not asking — or asking on a screen already answered, which is
		// the old drawing rather than a new question; another key would go
		// into the prompt.
		return
	}
	r.lastAsk = bottom
	// "…to allow deb.debian.org?", or for another method on a host asked
	// about before, "…to allow POST to registry.npmjs.org too?".
	what := ""
	if i := strings.Index(bottom, "to allow "); i >= 0 {
		words := strings.Fields(bottom[i+len("to allow "):] + " ")
		what = strings.TrimSuffix(words[0], "?")
		if len(words) >= 3 && words[1] == "to" {
			what = strings.TrimSuffix(words[2], "?") + " (" + words[0] + ")"
		}
	}
	if r.declineAsks {
		s.Notes = append(s.Notes, "declined to ask for a policy for "+what)
		r.term.Key("n")
	} else {
		s.Notes = append(s.Notes, "asked for a policy for "+what+" (the whole host)")
		r.term.Key("h")
	}
	time.Sleep(time.Second)
}

// promptApproving sends a prompt and, while the turn leaves access requests
// open, approves them in the console and says so — the loop a developer goes
// round when the agent reaches for a host nothing allows yet.
func (r *runner) promptApproving(s *step, text string) error {
	if err := r.prompt(s, text); err != nil {
		return err
	}
	for round := 0; round < 4; round++ {
		reqs, err := r.con.openRequests()
		if err != nil {
			return fmt.Errorf("listing access requests: %w", err)
		}
		if len(reqs) == 0 {
			return nil
		}
		var hosts []string
		for _, q := range reqs {
			if err := r.con.approve(q); err != nil {
				return fmt.Errorf("approving %s: %w", q.Ref, err)
			}
			label := q.Ref + " " + q.Proposed.Spec.Host
			if q.Proposed.Spec.Host == "" {
				label = q.Ref + " " + q.Title
			}
			hosts = append(hosts, strings.TrimSpace(label))
		}
		r.approved = append(r.approved, hosts...)
		s.Notes = append(s.Notes, "approved "+strings.Join(hosts, ", "))
		if err := r.prompt(s, "I approved network access for "+strings.Join(hosts, ", ")+". Run the same command again."); err != nil {
			return err
		}
	}
	return errors.New("access requests were still open after four rounds of approving them")
}

func (r *runner) scenario(ctx context.Context) {
	r.do("first run: choose standalone", true, func(s *step) error {
		if err := r.start(); err != nil {
			return err
		}
		if err := r.term.WaitFor(ctx, "Choose [1/2]", 0, time.Minute); err != nil {
			return err
		}
		r.term.Type("1")
		return nil
	})

	r.do("add a model in the console", true, func(s *step) error {
		if err := r.term.WaitFor(ctx, "Waiting for a model", 0, time.Minute); err != nil {
			return err
		}
		con, err := signIn("http://127.0.0.1:7171")
		if err != nil {
			return fmt.Errorf("signing in to the console: %w", err)
		}
		r.con = con
		name, id, err := con.addModel(r.opts.model, r.key)
		if err != nil {
			return err
		}
		r.models[name] = id
		s.Notes = append(s.Notes, "added "+name)
		return r.term.WaitFor(ctx, "added", 0, time.Minute)
	})

	r.do("sandbox ready", true, func(s *step) error {
		return r.term.WaitFor(ctx, "sandbox ready", 0, 30*time.Minute)
	})

	runs := func() []toolRun { return toolRuns(r.tr().all()) }
	builds := []string{"npm run build", "react-scripts build", "vite build"}

	r.do("type beyond ASCII, paste several lines, and type a long line", false, r.stepTyping)

	r.do("ask for a React tic-tac-toe app", false, func(s *step) error {
		return r.pursue(s, "a tic tac toe game as a Vite + React project (package.json, vite.config.js, index.html, src/), in the working directory",
			"Build me a tic tac toe game as a React project using Vite, in this directory. Write the files yourself "+
				"with the edit tool: package.json (dependencies react and react-dom, devDependencies vite and "+
				"@vitejs/plugin-react, and a \"build\": \"vite build\" script), vite.config.js, index.html, src/main.jsx "+
				"and src/App.jsx. Don't run npm or npx yet — Node isn't installed.",
			func() bool {
				return r.wrote("package.json") || ranOK(runs(), "create-react-app", "create vite", "npm init", "npm create")
			}, 3)
	})

	r.do("install Node (approving apt's hosts)", false, func(s *step) error {
		return r.pursue(s, "Node.js and npm installed in the sandbox, shown working with `node --version`",
			"Install Node.js and npm in the sandbox: run `apt-get update` and then `apt-get install -y nodejs npm`. "+
				"If a download is blocked by network policy, stop and tell me which host — I'll approve it. "+
				"When it's installed, run `node --version` and `npm --version`.",
			func() bool { return ranOK(runs(), "node --version", "node -v") }, 4)
	})

	r.do("install the npm packages (approving the registry)", false, func(s *step) error {
		return r.pursue(s, "the project's npm packages installed with npm install",
			"Run `npm install` in this directory. If the npm registry is blocked by network policy, stop and tell me — "+
				"I'll approve it.",
			func() bool { return ranOK(runs(), "npm install", "npm i ", "npm ci") }, 4)
	})

	r.do("run the React build", false, func(s *step) error {
		before := len(runs())
		return r.pursue(s, "a successful production build with `npm run build`",
			"Run `npm run build` and tell me whether it succeeded.",
			func() bool { return ranOK(runs()[before:], builds...) }, 4)
	})

	r.do("interrupt a long command with Esc", false, r.stepInterrupt)
	r.do("reject an access request", false, r.stepReject)
	r.do("an allowed host that cannot be reached", false, r.stepUnreachable)

	r.do("restart john", true, func(s *step) error {
		if err := r.restartJohn(ctx); err != nil {
			return err
		}
		scr := r.term.Screen()
		for _, again := range []string{"Choose [1/2]", "Waiting for a model"} {
			if strings.Contains(scr, again) {
				return fmt.Errorf("the second start asked again: %q", again)
			}
		}
		return nil
	})

	r.do("build again after the restart", false, func(s *step) error {
		before := len(runs())
		return r.pursue(s, "a successful `npm run build` again, after john was restarted",
			"Run `npm run build` again, then run `ls dist` to show what it produced.",
			func() bool { return ranOK(runs()[before:], builds...) }, 2)
	})

	r.do("switch models by a routing policy", false, func(s *step) error { return r.stepSwitchModel(ctx, s) })

	r.do("apply to the working directory", false, func(s *step) error {
		offset := r.term.Mark()
		r.term.Type("/apply")
		time.Sleep(3 * time.Second)
		r.term.Key("a")
		if r.term.WaitFor(ctx, "APPLY TO WORKING DIRECTORY", offset, 10*time.Second) != nil {
			// Nothing was selected, so `a` did nothing: select everything
			// shown, and apply that.
			r.term.Key("s")
			time.Sleep(500 * time.Millisecond)
			r.term.Key("a")
			if err := r.term.WaitFor(ctx, "APPLY TO WORKING DIRECTORY", offset, 10*time.Second); err != nil {
				return fmt.Errorf("the review never offered to apply: %w", err)
			}
		}
		r.term.Key("y")
		if err := r.term.WaitFor(ctx, "files →", offset, 5*time.Minute); err != nil {
			return err
		}
		// Whole names and a count, never a name cut short: the line once
		// read "apply index.html, package-lock.json, package.json, sr…".
		for _, l := range strings.Split(r.term.Since(offset), "\n") {
			if strings.Contains(l, "files →") && strings.Contains(l, "…") {
				return fmt.Errorf("the apply line cut a file name: %s", strings.TrimSpace(l))
			}
		}
		for _, want := range []string{"package.json", "src"} {
			if _, err := os.Stat(filepath.Join(r.project, want)); err != nil {
				return fmt.Errorf("%s is not in the working directory after apply", want)
			}
		}
		return nil
	})

	r.do("apply one file of two", false, func(s *step) error { return r.stepPartialApply(ctx, s) })
	r.do("edit on the host, then apply what is left", false, func(s *step) error { return r.stepHostEdit(ctx, s) })
	r.do("change the environment and rebuild", false, func(s *step) error { return r.stepEnvironment(ctx, s) })
	r.do("crash mid-command, restart and resume", false, func(s *step) error { return r.stepCrash(ctx, s) })

	r.do("quit", false, func(s *step) error {
		r.term.Type("/quit")
		return r.term.WaitExit(30 * time.Second)
	})
}

// collect copies out what the run left: the screen, the decoded trace, and
// each sandbox's guest console.
func (r *runner) collect() {
	if r.term != nil {
		os.WriteFile(filepath.Join(r.out, "screen.txt"), []byte(r.allScreens()), 0o644)
	}
	writeDecoded(r.tr().all(), filepath.Join(r.out, "trace.jsonl"))
	// Numbered in boot order: an instance's console.prev.log is the boot
	// before its console.log, and sorts after it by name.
	logs, _ := filepath.Glob(filepath.Join(r.home, "sandboxes", "*", "instances", "*", "console*.log"))
	sort.SliceStable(logs, func(i, j int) bool {
		if di, dj := filepath.Dir(logs[i]), filepath.Dir(logs[j]); di != dj {
			return di < dj
		}
		return strings.HasSuffix(logs[i], ".prev.log") && !strings.HasSuffix(logs[j], ".prev.log")
	})
	for i, p := range logs {
		copyFile(p, filepath.Join(r.out, fmt.Sprintf("guest-console-%d.log", i+1)))
	}
}

// screens holds each session's screen as it ended; the current one is read
// live.
func (r *runner) allScreens() string {
	var b strings.Builder
	for i, s := range r.pastScreens {
		fmt.Fprintf(&b, "════ john session %d ════\n%s\n\n", i+1, s)
	}
	if r.term != nil {
		fmt.Fprintf(&b, "════ john session %d ════\n%s\n", len(r.pastScreens)+1, r.term.Screen())
	}
	return b.String()
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func (r *runner) writeReport() {
	var b strings.Builder
	verdict := "PASSED"
	if r.failed() {
		verdict = "FAILED"
	}
	fmt.Fprintf(&b, "# john end-to-end run — %s\n\n", verdict)
	fmt.Fprintf(&b, "- binary: `%s`\n- platform: %s/%s\n- model: %s\n- started: %s\n- took: %s\n\n",
		r.bin, runtime.GOOS, runtime.GOARCH, r.opts.model, r.started.Format(time.RFC3339),
		time.Since(r.started).Round(time.Second))

	b.WriteString("## Steps\n\n| | step | took | |\n|---|---|---|---|\n")
	for _, s := range r.steps {
		mark := "✓"
		if !s.OK {
			mark = "✗"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", mark, s.Name, s.Took, strings.ReplaceAll(s.Err, "|", "\\|"))
	}
	b.WriteString("\n")
	for _, s := range r.steps {
		if len(s.Notes) == 0 {
			continue
		}
		fmt.Fprintf(&b, "**%s**\n\n", s.Name)
		for _, n := range s.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
		b.WriteString("\n")
	}

	runs := toolRuns(r.tr().all())
	failed := 0
	for _, t := range runs {
		if t.Status == "failed" || (t.Tool == "run" && t.Exit != 0) {
			failed++
		}
	}
	fmt.Fprintf(&b, "## Tool calls\n\n%d calls, %d failed.\n\n", len(runs), failed)
	b.WriteString("| tool | exit | status | command or path |\n|---|---|---|---|\n")
	for _, t := range runs {
		cmd := strings.ReplaceAll(strings.ReplaceAll(t.Cmd, "\n", " ⏎ "), "|", "\\|")
		if len(cmd) > 140 {
			cmd = cmd[:140] + "…"
		}
		fmt.Fprintf(&b, "| %s | %d | %s | `%s` |\n", t.Tool, t.Exit, t.Status, cmd)
	}
	if len(r.approved) > 0 {
		fmt.Fprintf(&b, "\n## Access approved\n\n- %s\n", strings.Join(r.approved, "\n- "))
	}
	b.WriteString("\n## Files\n\n- `screen.txt` — every john session as the terminal drew it\n" +
		"- `terminal-N.raw` — the bytes john wrote, escape sequences and all\n" +
		"- `trace.jsonl` — the backend's trace, decrypted\n" +
		"- `guest-console-N.log` — the sandbox kernel's console\n" +
		"- `project/` — the working directory, after apply\n")
	os.WriteFile(filepath.Join(r.out, "report.md"), []byte(b.String()), 0o644)
}
