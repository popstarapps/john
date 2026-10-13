package terminalui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// choiceBox is the machinery shared by every interactive box that asks a
// question: a scrollable body, a row of buttons, and one decision at the end.
//
// The approval inspector and the confirmation box differ only in what they put
// in the body, so that is all they implement.
type choiceBox struct {
	theme   *Theme
	choices []Choice
	sel     int

	lines  []string
	offset int
	width  int
}

func newChoiceBox(t *Theme, choices []Choice) choiceBox {
	b := choiceBox{theme: t, choices: choices}
	// The way out is where the cursor starts: Enter, Esc and Ctrl+C then all
	// mean the same thing, and the committing answer takes a deliberate move.
	for i, c := range choices {
		if c.Cancel {
			b.sel = i
			break
		}
	}
	for i, c := range choices {
		if c.Default {
			b.sel = i
			break
		}
	}
	return b
}

func (b *choiceBox) setLines(lines []string) {
	b.lines = lines
	b.clamp(len(lines))
}

func (b *choiceBox) clamp(height int) {
	maxOff := len(b.lines) - height
	if maxOff < 0 {
		maxOff = 0
	}
	if b.offset > maxOff {
		b.offset = maxOff
	}
	if b.offset < 0 {
		b.offset = 0
	}
}

// scrollable reports whether there is anything the developer has not seen.
func (b *choiceBox) scrollable(height int) bool { return len(b.lines) > height }

// scrollInfo describes the visible slice, and says nothing at all when the
// whole body is on screen. A counter that reads "1/13" while all thirteen lines
// are visible is a promise of more that does not exist — which is why j/k looks
// broken when it is working perfectly.
func (b *choiceBox) scrollInfo(height int) string {
	if !b.scrollable(height) {
		return ""
	}
	end := min(b.offset+height, len(b.lines))
	return itoa(b.offset+1) + b.theme.Symbols.Dash + itoa(end) + " of " + itoa(len(b.lines))
}

func (b *choiceBox) view(height int) []string {
	b.clamp(height)
	end := min(b.offset+height, len(b.lines))
	return b.lines[b.offset:end]
}

// cancelDecision is what dismissing the box means.
func (b *choiceBox) cancelDecision() string {
	for _, c := range b.choices {
		if c.Cancel {
			return c.Decision
		}
	}
	if len(b.choices) > 0 {
		return b.choices[len(b.choices)-1].Decision
	}
	return DecisionReject
}

// update handles a key. It returns a decision and true when the developer
// answered, or answered=false when the key was navigation.
func (b *choiceBox) update(msg tea.KeyMsg, height int) (decision string, answered bool) {
	// The application's own keys come first. They are the only keys in the box
	// that mean something specific, and a box whose "keep it" button cannot be
	// pressed because k also scrolls is a box that lies about its choices.
	// Scrolling keeps the arrows, page keys, home and end whatever happens.
	if len(msg.Runes) == 1 {
		for _, c := range b.choices {
			if c.Key == msg.Runes[0] {
				return c.Decision, true
			}
		}
	}

	switch msg.String() {
	case "left", "shift+tab":
		b.sel = (b.sel - 1 + len(b.choices)) % len(b.choices)
	case "right", "tab":
		b.sel = (b.sel + 1) % len(b.choices)
	case "enter":
		if b.sel < len(b.choices) {
			return b.choices[b.sel].Decision, true
		}
	case "down", "j":
		b.offset++
	case "up", "k":
		b.offset--
	case "ctrl+d", "pgdown":
		b.offset += height / 2
	case "ctrl+u", "pgup":
		b.offset -= height / 2
	case "g", "home":
		b.offset = 0
	case "G", "end":
		b.offset = len(b.lines)
	case "esc", "q":
		return b.cancelDecision(), true
	}
	b.clamp(height)
	return "", false
}

// buttons renders the choices as a row of buttons, the selected one filled.
//
// Every button stays on the row. When the labels are wider than the box, the
// longest gives way first — "only /lookup/github.com/…" — rather than the row
// running off the edge and taking the last button with it, which is the "no".
func (b *choiceBox) buttons(inner int) string {
	t := b.theme
	labels := make([]string, len(b.choices))
	styles := make([]lipgloss.Style, len(b.choices))
	total := 2 * (len(b.choices) - 1)
	for i, c := range b.choices {
		labels[i] = string(c.Key) + "  " + c.Label
		style := t.Button
		switch {
		case i == b.sel && c.Danger:
			style = t.ButtonDangerSel
		case i == b.sel:
			style = t.ButtonSelected
		case c.Danger:
			style = t.ButtonDanger
		}
		styles[i] = style
		total += visibleWidth(style.Render(labels[i]))
	}
	const shortest = 8 // the key, its gap, and enough of the label to read
	for over := total - inner; over > 0; {
		longest := 0
		for i := range labels {
			if visibleWidth(labels[i]) > visibleWidth(labels[longest]) {
				longest = i
			}
		}
		w := visibleWidth(labels[longest])
		cut := min(over, w-shortest)
		if cut <= 0 {
			break
		}
		labels[longest] = truncPlain(labels[longest], w-cut, t.Symbols.Ellipsis)
		over -= cut
	}
	parts := make([]string, len(labels))
	for i := range labels {
		parts[i] = styles[i].Render(labels[i])
	}
	return strings.Join(parts, "  ")
}

// hints describes the keys that act on the box. Esc is deliberately absent: it
// cancels, and a way out of a decision does not belong in the same breath as
// the decision itself.
func (b *choiceBox) hints(height int) string {
	t := b.theme
	parts := []string{
		t.HintKey.Render(t.Symbols.ArrowLR) + t.Hint.Render(" choose"),
		t.HintKey.Render(t.Symbols.Enter) + t.Hint.Render(" confirm"),
	}
	if b.scrollable(height) {
		parts = append(parts, t.HintKey.Render(t.Symbols.ArrowUD)+t.Hint.Render(" scroll"))
	}
	return strings.Join(parts, t.Hint.Render(" "+t.Symbols.Dot+" "))
}

// buttonRow puts the buttons and the keys that drive them on one row, dropping
// the keys when the terminal cannot hold both.
func (b *choiceBox) buttonRow(inner, height int) string {
	buttons := b.buttons(inner)
	hints := b.hints(height)
	gap := inner - visibleWidth(buttons) - visibleWidth(hints) - 2
	if gap < 2 {
		return buttons
	}
	return buttons + strings.Repeat(" ", gap+2) + hints
}
