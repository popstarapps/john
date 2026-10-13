package terminalui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// runPlain renders the same event stream without a terminal: no live region, no
// cursor movement, no input. Piping john into a file or running it in CI must
// produce a readable log, not a screenful of escape sequences.
//
// It is also where John's fail-closed posture shows up in the UI: a decision
// that cannot be presented is not silently taken for the developer, it is
// refused (or refuses to run at all, with NonInteractiveError).
func (u *UI) runPlain(ctx context.Context) error {
	w := bufio.NewWriter(u.opts.Output)
	defer w.Flush()

	stop := context.AfterFunc(ctx, func() { u.events.close() })
	defer stop()

	p := &plainRenderer{
		w:     w,
		width: plainWidth(u.opts.Output),
		out:   u.actions,
		open:  map[string]*block{},
		fail:  u.opts.NonInteractive == NonInteractiveError,
	}

	for {
		ev, ok := u.events.take()
		if !ok {
			w.Flush()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		if _, isQuit := ev.(Quit); isQuit {
			w.Flush()
			return nil
		}
		if err := p.handle(ev); err != nil {
			w.Flush()
			return err
		}
		w.Flush()
	}
}

func plainWidth(out any) int {
	if f, ok := out.(*os.File); ok {
		if wdt, _, err := term.GetSize(int(f.Fd())); err == nil && wdt > 20 {
			return min(wdt, 120)
		}
	}
	if s := os.Getenv("COLUMNS"); s != "" {
		n := 0
		for _, r := range s {
			if r < '0' || r > '9' {
				n = 0
				break
			}
			n = n*10 + int(r-'0')
		}
		if n > 20 {
			return min(n, 120)
		}
	}
	return 80
}

type plainRenderer struct {
	w     *bufio.Writer
	width int
	out   *actionQueue
	open  map[string]*block
	fail  bool
}

func (p *plainRenderer) line(format string, args ...any) {
	fmt.Fprintf(p.w, format+"\n", args...)
}

// noModelChosen is the outcome of a ModelChoice with nobody to answer it. The
// same words go on screen and into the decision's Reason, so a log and the
// application's own record of the turn say the same thing.
const noModelChosen = "no model chosen: no terminal to ask in"

func (p *plainRenderer) handle(ev Event) error {
	switch e := ev.(type) {
	case Notice:
		if e.Prominent {
			p.line("")
		}
		prefix := "  "
		switch e.Level {
		case NoticeWarn:
			prefix = "! "
		case NoticeError:
			prefix = "x "
		}
		for i, l := range wrapPlain(sanitizeBlock(e.Text), p.width-2) {
			if i == 0 {
				p.line("%s%s", prefix, l)
			} else {
				p.line("  %s", l)
			}
		}

	case AssistantMessage:
		p.assistant(e.Text)

	case AssistantStart:
		p.open[e.ID] = &block{kind: kindAssistant, id: e.ID}
	case AssistantDelta:
		if b := p.open[e.ID]; b != nil {
			b.sb.WriteString(e.Text)
		}
	case AssistantEnd:
		if b := p.open[e.ID]; b != nil {
			delete(p.open, e.ID)
			p.assistant(b.assistantText())
		}

	case ToolStart:
		p.open[e.ID] = &block{kind: kindTool, id: e.ID, verb: e.Verb, target: e.Target, targets: e.Targets, meta: e.Meta}
	case ToolDetail:
		if b := p.open[e.ID]; b != nil {
			b.details = append(b.details, e.Text)
		}
	case ToolEnd:
		b := p.open[e.ID]
		if b == nil {
			return nil
		}
		delete(p.open, e.ID)
		if e.Meta != "" {
			b.meta = e.Meta
		}
		b.status = e.Status
		p.tool(b)

	case Progress:
		// One line per step, so a CI log reads as a build log and not as a
		// flipbook of redrawn bars.
		b := p.open[e.ID]
		if b == nil {
			b = &block{kind: kindProgress, id: e.ID}
			p.open[e.ID] = b
		}
		changed := b.step != e.Step || b.target != e.Label
		b.step, b.steps, b.target = e.Step, e.Steps, e.Label
		switch {
		case e.Done:
			delete(p.open, e.ID)
			p.tool(&block{verb: "build", target: e.Label, meta: e.Meta, status: e.Status})
		case changed:
			meta := ""
			if e.Steps > 0 {
				meta = "step " + itoa(e.Step) + "/" + itoa(e.Steps)
			}
			p.tool(&block{verb: "build", target: e.Label, meta: meta, details: []string{e.Detail}})
		}

	case ProxyDecision:
		b := &block{
			verb:   "proxy",
			target: strings.TrimSpace(e.Method + " " + e.URL),
			meta:   e.Verdict,
			status: proxyStatus(e.Allowed),
		}
		if e.Error != "" {
			b.meta, b.status, b.details = "failed", ToolFail, []string{e.Error}
		}
		p.tool(b)

	case ApprovalRequest:
		if p.fail {
			return fmt.Errorf("%w: approval for %s %s", ErrNotInteractive, e.Method, e.URL)
		}
		p.tool(&block{
			verb:   "proxy",
			target: strings.TrimSpace(e.Method + " " + e.URL),
			meta:   "rejected · no terminal to approve in",
			status: ToolBlocked,
		})
		p.out.push(ApprovalDecision{
			ID:       e.ID,
			Decision: DecisionReject,
			Reason:   "no terminal: approvals cannot be presented",
		})

	case Confirm:
		if p.fail {
			return fmt.Errorf("%w: %s", ErrNotInteractive, e.Question)
		}
		choices := e.Choices
		if len(choices) == 0 {
			choices = DefaultConfirmChoices()
		}
		decision := DecisionNo
		for _, c := range choices {
			if c.Cancel {
				decision = c.Decision
			}
		}
		p.line("")
		p.line("  %s", truncPlain(e.Question, p.width-2, "..."))
		for _, d := range e.Detail {
			p.line("    %s", truncPlain(d, p.width-4, "..."))
		}
		p.line("  answered %q: no terminal to ask in", decision)
		p.line("")
		p.out.push(ConfirmDecision{
			ID: e.ID, Decision: decision,
			Reason: "no terminal: questions cannot be presented",
		})

	case DiffReview:
		if p.fail {
			return fmt.Errorf("%w: review of %d files", ErrNotInteractive, len(e.Files))
		}
		p.diff(e)
		p.out.push(DiffDecision{ID: e.ID, Apply: false})

	case ModelChoice:
		if p.fail {
			return fmt.Errorf("%w: choice of %d models", ErrNotInteractive, len(e.Models))
		}
		// Nothing is chosen on the developer's behalf. A session narrows
		// because something in it must not reach every model, and picking one
		// for them is exactly the decision they were meant to make.
		p.line("")
		if e.Reason != "" {
			p.line("  %s", truncPlain(e.Reason, p.width-2, "..."))
		}
		if e.Policy != "" {
			p.line("  %s", truncPlain(e.Policy, p.width-2, "..."))
		}
		for _, mo := range e.Models {
			p.line("    %s", truncPlain(mo.Name, p.width-4, "..."))
		}
		p.line("  %s", noModelChosen)
		p.line("")
		p.out.push(ModelDecision{ID: e.ID, Cancelled: true, Reason: noModelChosen})

	case Status, Busy, Idle, Thinking, Completions, CancelApproval, CancelConfirm, CancelDiffReview,
		CancelModelChoice:
		// State, not history. Nothing to write.
	}
	return nil
}

func proxyStatus(allowed bool) ToolStatus {
	if allowed {
		return ToolOK
	}
	return ToolBlocked
}

func (p *plainRenderer) tool(b *block) {
	target := b.target
	if len(b.targets) > 0 {
		// A log has no width to run out of, so it keeps every name.
		target = strings.Join(b.targets, ", ")
	}
	head := "  " + b.verb + " " + target
	if b.meta != "" {
		head += "  " + b.meta
	}
	switch b.status {
	case ToolFail:
		head = "x" + head[1:]
	case ToolBlocked:
		head = "x" + head[1:]
	case ToolSkipped:
		head = "o" + head[1:]
	}
	p.line("%s", truncPlain(head, p.width, "..."))
	for _, d := range b.details {
		p.line("    %s", truncPlain(d, p.width-4, "..."))
	}
}

func (p *plainRenderer) assistant(text string) {
	text = strings.TrimSpace(sanitizeBlock(text))
	if text == "" {
		return
	}
	p.line("")
	for _, l := range wrapPlain(text, p.width-2) {
		if l == "" {
			p.line("")
			continue
		}
		p.line("  %s", l)
	}
	p.line("")
}

func (p *plainRenderer) diff(rev DiffReview) {
	title := rev.Title
	if title == "" {
		title = "changes"
	}
	p.line("")
	p.line("  %s", title)
	for _, f := range rev.Files {
		p.line("  %s  +%d -%d", f.Path, f.Added, f.Removed)
		for _, h := range f.Hunks {
			p.line("    %s", truncPlain(h.Header, p.width-4, "..."))
			for _, l := range h.Lines {
				sign := " "
				switch l.Kind {
				case DiffAdd:
					sign = "+"
				case DiffRemove:
					sign = "-"
				}
				p.line("    %s %s", sign, truncPlain(l.Text, p.width-6, "..."))
			}
		}
	}
	p.line("")
	p.line("  not applied: no terminal to review in")
	p.line("")
}
