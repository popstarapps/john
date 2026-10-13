package terminalui

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

type blockKind int

const (
	kindUser blockKind = iota
	kindNotice
	kindTool
	kindAssistant
	kindArt
	kindProgress
)

// block is one unit of transcript. Blocks are created by events, rendered in
// the live region while they are open, and committed to the terminal's real
// scrollback once they close. A committed block is never redrawn, which is
// exactly why it stays selectable and copyable.
type block struct {
	kind blockKind
	id   string
	done bool
	// nested marks a block that belongs to the prompt above it: it is drawn
	// indented under that prompt.
	nested bool

	// user / notice
	text  string
	level NoticeLevel
	// prominent is a Notice that asked to stand out.
	prominent bool

	// tool
	verb    string
	target  string
	targets []string // ToolStart.Targets, fitted to the width when rendered
	meta    string
	details []string
	status  ToolStatus
	// requests counts the requests an open proxy line stands for: requests
	// to one host that were all allowed, or all blocked for the same reason,
	// collapsed into one line while they keep coming.
	requests int

	// assistant
	sb strings.Builder

	// art is pre-rendered lines (the logo), passed through untouched
	art []string

	// progress
	step, steps, percent int
	started              time.Time

	// render cache, keyed by the inputs that can change it
	cacheKey  string
	cacheLine []string
}

func (b *block) assistantText() string { return b.sb.String() }

// render draws the block at the given width. spin is the current spinner frame,
// used for tool calls that are still running.
func (b *block) render(t *Theme, md *markdownRenderer, width int, spin string) []string {
	if width < 8 {
		width = 8
	}
	var lines []string
	switch b.kind {
	case kindUser:
		lines = b.renderUser(t, width)
	case kindNotice:
		lines = b.renderNotice(t, width)
	case kindTool:
		lines = b.renderTool(t, width, spin)
	case kindAssistant:
		lines = b.renderAssistant(t, md, width)
	case kindArt:
		lines = b.art
	case kindProgress:
		lines = b.renderProgress(t, width, spin)
	}
	// Individual renderers budget their own columns, but a narrow terminal can
	// leave them nothing to work with. Nothing leaves here wider than asked.
	for i, l := range lines {
		if visibleWidth(l) > width {
			lines[i] = ansi.Truncate(l, width, "")
		}
	}
	return lines
}

func (b *block) renderUser(t *Theme, width int) []string {
	lines := wrapPlain(sanitizeBlock(b.text), width-2)
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		if i == 0 {
			out = append(out, t.UserGlyph.Render(t.Symbols.User)+" "+t.User.Render(l))
			continue
		}
		out = append(out, "  "+t.User.Render(l))
	}
	return out
}

func (b *block) renderNotice(t *Theme, width int) []string {
	style := t.Muted
	if b.prominent {
		style = t.User
	}
	prefix := "  "
	cont := "  "
	if b.nested {
		// Under a prompt the block is already indented to the prompt's text,
		// so a notice with no glyph of its own starts right there: "/new"
		// and what it answered line up, rather than the answer stepping two
		// columns further in.
		prefix, cont = "", ""
	}
	switch b.level {
	case NoticeWarn:
		style = t.Warn
		prefix, cont = t.Warn.Render("!")+" ", "  "
	case NoticeError:
		style = t.Error
		prefix, cont = t.Error.Render(t.Symbols.Fail)+" ", "  "
	}
	lines := wrapPlain(sanitizeBlock(b.text), width-visibleWidth(cont))
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		if i == 0 {
			out = append(out, prefix+style.Render(l))
			continue
		}
		out = append(out, cont+style.Render(l))
	}
	return out
}

func (b *block) renderTool(t *Theme, width int, spin string) []string {
	verb := b.verb
	if verb == "" {
		verb = "tool"
	}
	// The running glyph wears the colour the finished one will, which is the
	// verb's own: the spinner is the same dot, swelling until it settles.
	// Nothing moves or changes hue when the call completes — it just stops
	// pulsing, unless it failed, which is worth a change of colour.
	glyph := t.verbStyle(verb).Render(spin)
	if b.done {
		g, style := t.toolGlyph(verb, b.status)
		glyph = style.Render(g)
	}

	// 2 for the glyph and its space, then the verb, then a space.
	avail := width - 2 - len([]rune(verb)) - 1
	meta := sanitizeLine(b.meta)
	metaCost := 0
	if meta != "" {
		metaCost = visibleWidth(meta) + 2
	}
	// The meta is usually a word or two — "exit 1", "1.3s" — and the target
	// gives way to it. But the target is what the line is about, so it keeps
	// at least half the line, and past that it is a long meta that gives way:
	// "model internal/…  switched by a routing policy…" named everything but
	// the model.
	full := visibleWidth(sanitizeLine(b.target))
	if len(b.targets) > 0 {
		full = visibleWidth(sanitizeLine(strings.Join(b.targets, ", ")))
	}
	budget := max(avail-metaCost, 4)
	if keep := min(full, avail/2); meta != "" && keep > budget {
		if room := avail - keep - 2; room >= 8 {
			meta = truncPlain(meta, room, t.Symbols.Ellipsis)
			budget = keep
		}
	}
	target := truncPlain(b.target, budget, t.Symbols.Ellipsis)
	if len(b.targets) > 0 {
		target = fitNames(b.targets, budget, t.Symbols.Ellipsis)
	}

	head := glyph + " " + t.verbStyle(verb).Render(verb) + " " + t.Target.Render(target)
	if meta != "" {
		head += "  " + t.Meta.Render(meta)
	}
	out := []string{head}

	for i, d := range b.details {
		conn := t.Symbols.TreeMid
		if i == len(b.details)-1 {
			conn = t.Symbols.TreeEnd
		}
		// The gutter is empty, not ruled: a vertical line down the left of
		// every detail adds no information the indent has not already given.
		prefix := "  " + t.Tree.Render(conn+" ")
		out = append(out, prefix+t.Muted.Render(truncPlain(d, width-4, t.Symbols.Ellipsis)))
	}
	return out
}

// fitNames lists as many whole names as fit in width, then says how many it
// left out: "index.html, package.json, +5 more". A name is never cut, because
// a cut one names nothing — "sr…" could be any of four files. When not even
// the first fits beside the count, it is the one thing cut.
func fitNames(names []string, width int, ellipsis string) string {
	more := func(n int) string { return "+" + itoa(n) + " more" }
	shown := ""
	for i, name := range names {
		next := sanitizeLine(name)
		if shown != "" {
			next = shown + ", " + next
		}
		rest := len(names) - i - 1
		need := visibleWidth(next)
		if rest > 0 {
			need += len(", " + more(rest))
		}
		if need > width {
			if shown != "" {
				return shown + ", " + more(len(names)-i)
			}
			suffix := ""
			if rest > 0 {
				suffix = ", " + more(rest)
			}
			return truncPlain(next, max(width-len(suffix), 1), ellipsis) + suffix
		}
		shown = next
	}
	return shown
}

func (b *block) renderProgress(t *Theme, width int, spin string) []string {
	glyph := t.verbStyle("build").Render(spin)
	if b.done {
		g, style := t.toolGlyph("build", b.status)
		glyph = style.Render(g)
	}

	head := glyph + " " + t.verbStyle("build").Render(sanitizeLine(b.target))
	var meta []string
	if b.steps > 0 {
		meta = append(meta, "step "+itoa(b.step)+"/"+itoa(b.steps))
	}
	if !b.started.IsZero() {
		if d := time.Since(b.started).Truncate(time.Second); d > 0 {
			meta = append(meta, d.String())
		}
	}
	if b.meta != "" {
		meta = append(meta, sanitizeLine(b.meta))
	}
	if len(meta) > 0 {
		head += t.Meta.Render("  " + strings.Join(meta, " "+t.Symbols.Dot+" "))
	}
	out := []string{head}
	if b.done {
		return out
	}

	// The bar and the current line of output hang off the label the way a
	// tool's output hangs off the tool.
	var rows []string
	if b.percent > 0 {
		rows = append(rows, t.progressBar(b.percent, min(width/3, 28))+
			t.Meta.Render("  "+itoa(b.percent)+"%"))
	}
	if b.detail() != "" {
		rows = append(rows, t.Muted.Render(truncPlain(b.detail(), width-4, t.Symbols.Ellipsis)))
	}
	for i, r := range rows {
		conn := t.Symbols.TreeMid
		if i == len(rows)-1 {
			conn = t.Symbols.TreeEnd
		}
		out = append(out, "  "+t.Tree.Render(conn+" ")+r)
	}
	return out
}

// detail is the most recent line of output for a progress block.
func (b *block) detail() string {
	if len(b.details) == 0 {
		return ""
	}
	return b.details[len(b.details)-1]
}

func (b *block) renderAssistant(t *Theme, md *markdownRenderer, width int) []string {
	text := b.assistantText()
	if strings.TrimSpace(text) == "" {
		return nil
	}
	key := cacheKey(width, len(text), b.done)
	if b.cacheKey == key && b.cacheLine != nil {
		return b.cacheLine
	}
	// The continuation indent follows the marker's width, so a two column
	// glyph (the ASCII arrow, say) still lines its text up.
	pad := strings.Repeat(" ", visibleWidth(t.Symbols.Assistant)+1)
	lines := md.render(text, width-len(pad))
	if !md.enabled {
		// Plain text carries no styling of its own, so the theme supplies it.
		for i, l := range lines {
			lines[i] = t.Assistant.Render(l)
		}
	}
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		if i == 0 {
			out = append(out, t.AssistGly.Render(t.Symbols.Assistant)+" "+l)
			continue
		}
		out = append(out, pad+l)
	}
	b.cacheKey, b.cacheLine = key, out
	return out
}

func cacheKey(width, n int, done bool) string {
	var sb strings.Builder
	sb.WriteString(itoa(width))
	sb.WriteByte(':')
	sb.WriteString(itoa(n))
	if done {
		sb.WriteByte('!')
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// spacingBefore says whether a blank line separates prev from cur; prev is nil
// for the first block of the session. One-line things — a prompt, the tool
// calls and notices under it — stack without a gap, so a turn's actions read
// as a list under the prompt that caused them. Prose gets air on both sides:
// an answer run straight into the commands around it is hard to find again.
func spacingBefore(prev, cur *block) bool {
	if prev == nil {
		return false
	}
	return !(compact(prev) && compact(cur) && cur.kind != kindUser)
}

// compact reports whether a block is a line of activity rather than prose.
func compact(b *block) bool {
	switch b.kind {
	case kindUser, kindTool, kindProgress:
		return true
	case kindNotice:
		return !b.prominent
	}
	return false
}

// renderIn draws the block at width, indented under its prompt if it is nested.
func (b *block) renderIn(t *Theme, md *markdownRenderer, width int, spin string) []string {
	if !b.nested {
		return b.render(t, md, width, spin)
	}
	lines := b.render(t, md, width-2, spin)
	out := make([]string, len(lines))
	for i, l := range lines {
		if l != "" {
			l = "  " + l
		}
		out[i] = l
	}
	return out
}
