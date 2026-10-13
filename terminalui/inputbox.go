package terminalui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The input field sits in a box whose edges carry the session's state, which
// is why there is no status line under it:
//
//	╭─ ~/src/payments ─────────────────────────── 3 unapplied · /apply ─╮
//	│ ❯ ask john to do something…  / commands · @ files                  │
//	╰─ esc to interrupt ─────────────────── codegen-l · 6.1k / 200k ─╯
//
// Top left is the project, and top right the sandbox's state, in the border's
// own colour: it is about the box's contents, not a call to act. Bottom left
// is what can be done now, in the same colour as the rest of the edge's text,
// and is the last thing to give way. Bottom right is the model and how much of its context the conversation
// fills. An alert is a line of its own above the box, and turns the box's
// border the alert colour with it.

// inputBox draws the box, with the search prompt and the completion popup
// inside it above the field.
func (m *uiModel) inputBox() []string {
	t := m.th
	w := m.frameWidth()
	inner := boxInnerWidth(w)
	border := t.Border
	if m.status.Alert != "" {
		border = t.StatusAlert.UnsetBold()
	}

	var body []string
	if m.input.searching {
		body = append(body, m.input.searchLine(inner))
	}
	if pop := m.input.popupView(inner); len(pop) > 0 {
		body = append(body, pop...)
		body = append(body, "")
	}
	m.input.hint = m.placeholderHint()
	body = append(body, strings.Split(m.input.view(), "\n")...)

	v := border.Render(t.Symbols.BoxV)
	out := make([]string, 0, len(body)+2)
	where, sandbox := m.topText(border, w)
	out = append(out, m.boxEdgeText(border, t.Symbols.BoxTL, t.Symbols.BoxTR, where, sandbox, w))
	for _, l := range body {
		if visibleWidth(l) > inner {
			l = ansi.Truncate(l, inner, "")
		}
		out = append(out, v+" "+padTo(l, inner)+" "+v)
	}
	state, ctx := m.stateText(w)
	out = append(out, m.boxEdgeText(border, t.Symbols.BoxBL, t.Symbols.BoxBR, state, ctx, w))
	return out
}

// boxEdgeText draws a top or bottom edge with already-styled text set into it
// on the left and on the right. The caller fits the text; see whereText and
// stateText.
func (m *uiModel) boxEdgeText(border lipgloss.Style, lc, rc, left, right string, w int) string {
	h := m.th.Symbols.BoxH
	if w < 8 {
		return border.Render(strings.Repeat(h, max(w, 1)))
	}
	head := border.Render(lc + h)
	used := 2
	if left != "" {
		head += " " + left + " "
		used += visibleWidth(left) + 2
	}
	tail := border.Render(h + rc)
	used += 2
	if right != "" {
		tail = " " + right + " " + tail
		used += visibleWidth(right) + 2
	}
	return head + border.Render(strings.Repeat(h, max(w-used, 0))) + tail
}

// topText is the top edge's text: the working directory on the left and the
// sandbox's state on the right, fitted to a box w columns wide.
//
// The directory gives way first, and it gives up its leading segments rather
// than its trailing ones, because what tells a developer which project they
// are in is the end of the path and never the beginning — /var/folders/0r/...
// is a temporary directory on every Mac.
func (m *uiModel) topText(border lipgloss.Style, w int) (where, sandbox string) {
	t := m.th
	room := w - 6 // the corners, a rule on each side, a space each side
	if sb := sanitizeLine(m.status.Sandbox); sb != "" {
		sb = truncPlain(sb, room, t.Symbols.Ellipsis)
		room -= visibleWidth(sb) + 2
		sandbox = border.Render(sb)
	}
	if wd := elideLeading(sanitizeLine(m.status.Workdir), room-1, t.Symbols.Ellipsis); wd != "" {
		where = t.StatusBar.Render(wd)
	}
	return where, sandbox
}

// stateText is the bottom edge's text: what can be done now on the left, and
// the model and the context used on the right, fitted to a box w columns wide.
func (m *uiModel) stateText(w int) (state, ctx string) {
	t := m.th
	var parts []string
	switch {
	case m.pendingCT:
		parts = append(parts, "press ctrl+c again to exit")
	default:
		if m.busy {
			parts = append(parts, "esc to interrupt")
		}
		if s := sanitizeLine(m.status.Extra); s != "" {
			parts = append(parts, s)
		}
	}
	plain := strings.Join(parts, " "+t.Symbols.Dot+" ")

	// The model and the context give way before the state does: the context
	// to its short form, then the model, then the context too. The state says
	// what the developer can do.
	var long, short string
	if m.status.Tokens > 0 {
		short = formatTokens(m.status.Tokens)
		long = short
		if m.status.Window > 0 {
			long += " / " + formatTokens(m.status.Window)
		}
	}
	model := sanitizeLine(m.status.Model)
	join := func(a, b string) string {
		if a == "" || b == "" {
			return a + b
		}
		return a + " " + t.Symbols.Dot + " " + b
	}
	room := w - 6 // the corners, a rule on each side, a space each side
	if plain != "" {
		room -= visibleWidth(plain) + 2
	}
	for _, c := range []string{join(model, long), join(model, short), short} {
		if c != "" && visibleWidth(c)+1 <= room {
			ctx = t.StatusBar.Render(c)
			break
		}
	}
	if plain != "" {
		state = t.StatusBar.Render(truncPlain(plain, w-6, t.Symbols.Ellipsis))
	}
	return state, ctx
}

// placeholderHint is the key hint shown after the placeholder in an empty
// field: what there is to type, not what the keys around it do.
func (m *uiModel) placeholderHint() string {
	t := m.th
	dot := t.Hint.Render(" " + t.Symbols.Dot + " ")
	key := func(k, label string) string {
		return t.HintKey.Render(k) + t.Hint.Render(" "+label)
	}
	if len(m.queued) > 0 {
		return key(t.Symbols.Backspace, "take back")
	}
	if m.busy {
		return ""
	}
	var parts []string
	if len(m.opts.Commands) > 0 {
		parts = append(parts, key("/", "commands"))
	}
	if m.opts.FileCompletion {
		parts = append(parts, key("@", "files"))
	}
	return strings.Join(parts, dot)
}

// alertLine states that the session is degraded, above the input box. It is
// the last thing on screen to give way: john fails closed, and the UI should
// say so before it says anything else.
func (m *uiModel) alertLine() string {
	a := m.status.Alert
	if a == "" {
		return ""
	}
	t := m.th
	return "  " + t.StatusAlert.Render(t.Symbols.Alert+" "+truncPlain(sanitizeLine(a), m.frameWidth()-4, t.Symbols.Ellipsis))
}
