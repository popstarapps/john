package terminalui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type pickerRowKind int

const (
	pickerText pickerRowKind = iota
	pickerModel
)

type pickerRow struct {
	kind  pickerRowKind
	model int // index into req.Models, for pickerModel rows
	text  string
}

// modelPickerView asks which model the session continues on.
//
// It opens when a routing policy's keyword has been seen in the session and
// the set of permitted models has shrunk underneath the developer. Two things
// have to happen at once: the narrowing has to be stated — what was seen, and
// which policy acted on it — and a choice has to be made from what is left.
// Doing it in one box keeps the reason next to the decision it explains.
//
// The session never un-narrows, so this is not a preference the developer can
// come back and widen later. That is why it is a box rather than a notice.
type modelPickerView struct {
	req    ModelChoice
	theme  *Theme
	rows   []pickerRow
	cursor int // index into rows
	offset int
	width  int
	dirty  bool
}

func newModelPickerView(req ModelChoice, t *Theme) *modelPickerView {
	v := &modelPickerView{req: req, theme: t, dirty: true, cursor: -1}
	return v
}

func (v *modelPickerView) layout(width int) {
	if width == v.width && !v.dirty {
		return
	}
	v.width = width
	v.dirty = false
	t := v.theme
	w := max(width-2, 8)

	rows := make([]pickerRow, 0, len(v.req.Models)+6)
	add := func(s string) { rows = append(rows, pickerRow{kind: pickerText, text: s}) }

	if v.req.Reason != "" {
		for _, l := range wrapPlain(sanitizeBlock(v.req.Reason), w) {
			add(t.Text.Render(l))
		}
	}
	if v.req.Policy != "" {
		for _, l := range wrapPlain(sanitizeBlock(v.req.Policy), w) {
			add(t.Muted.Render(l))
		}
	}
	if len(rows) > 0 {
		add("")
	}

	for i, mo := range v.req.Models {
		rows = append(rows, pickerRow{kind: pickerModel, model: i, text: v.modelRowText(mo, i, w)})
	}
	v.rows = rows

	// The cursor starts on the selected model, if there is one.
	if v.cursor < 0 {
		for i, r := range rows {
			if r.kind == pickerModel && v.req.Models[r.model].ID == v.req.Selected {
				v.cursor = i
			}
		}
	}
	// The cursor lives on a model row, never on the prose above it.
	if v.rowKind(v.cursor) != pickerModel {
		v.cursor = v.firstModelRow()
	}
	v.clampCursor()
}

func (v *modelPickerView) modelRowText(mo ModelOption, i, w int) string {
	t := v.theme
	// A digit key per model, so a developer who knows the list does not have
	// to walk it. Past nine they scroll, like everywhere else.
	key := "  "
	if i < 9 {
		key = itoa(i+1) + " "
	}
	name := sanitizeLine(mo.Name)
	where := "external"
	style := t.Muted
	if mo.Internal {
		where, style = "internally hosted", t.StatusOK
	}
	// Every piece is truncated as plain text before it is styled, so a long
	// name cannot push the row past the box and take the border with it.
	line := t.HintKey.Render(key) +
		t.Target.Render(truncPlain(name, w/2, t.Symbols.Ellipsis)) + " " +
		style.Render(t.Symbols.Dot+" "+where)
	if mo.Detail != "" {
		rest := w - visibleWidth(line) - 4
		if rest > 6 {
			line += " " + t.Meta.Render(t.Symbols.Dot+" "+truncPlain(sanitizeLine(mo.Detail), rest, t.Symbols.Ellipsis))
		}
	}
	return line
}

func (v *modelPickerView) rowKind(i int) pickerRowKind {
	if i < 0 || i >= len(v.rows) {
		return pickerText
	}
	return v.rows[i].kind
}

func (v *modelPickerView) firstModelRow() int {
	for i, r := range v.rows {
		if r.kind == pickerModel {
			return i
		}
	}
	return 0
}

func (v *modelPickerView) lastModelRow() int {
	for i := len(v.rows) - 1; i >= 0; i-- {
		if v.rows[i].kind == pickerModel {
			return i
		}
	}
	return 0
}

// moveCursor walks to the next model row in the given direction, staying put
// at either end rather than wrapping: a list that wraps makes "am I at the
// bottom?" a question you cannot answer by pressing a key.
func (v *modelPickerView) moveCursor(delta int) {
	for i := v.cursor + delta; i >= 0 && i < len(v.rows); i += delta {
		if v.rows[i].kind == pickerModel {
			v.cursor = i
			return
		}
	}
}

func (v *modelPickerView) clampCursor() {
	if v.cursor < 0 {
		v.cursor = v.firstModelRow()
	}
	if v.cursor >= len(v.rows) {
		v.cursor = v.lastModelRow()
	}
}

func (v *modelPickerView) clampOffset(height int) {
	maxOff := len(v.rows) - height
	if maxOff < 0 {
		maxOff = 0
	}
	if v.offset > maxOff {
		v.offset = maxOff
	}
	if v.cursor < v.offset {
		v.offset = v.cursor
	}
	if v.cursor >= v.offset+height {
		v.offset = v.cursor - height + 1
	}
	if v.offset < 0 {
		v.offset = 0
	}
}

// selected is the model the cursor is on.
func (v *modelPickerView) selected() int {
	if v.rowKind(v.cursor) == pickerModel {
		return v.rows[v.cursor].model
	}
	return -1
}

func (v *modelPickerView) decide(i int) *ModelDecision {
	if i < 0 || i >= len(v.req.Models) {
		return nil
	}
	return &ModelDecision{ID: v.req.ID, ModelID: v.req.Models[i].ID}
}

// update handles a key, returning a decision once one has been made.
func (v *modelPickerView) update(msg tea.KeyMsg, height int) *ModelDecision {
	if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
		if d := v.decide(int(msg.Runes[0] - '1')); d != nil {
			return d
		}
	}

	switch msg.String() {
	case "down", "j", "tab":
		v.moveCursor(1)
	case "up", "k", "shift+tab":
		v.moveCursor(-1)
	case "g", "home":
		v.cursor = v.firstModelRow()
	case "G", "end":
		v.cursor = v.lastModelRow()
	case "enter", " ":
		if d := v.decide(v.selected()); d != nil {
			return d
		}
	case "esc":
		// There is no safe model to fall back to — the narrowing already
		// happened — so declining means the turn stops. Only Esc declines:
		// it cannot be taken back, and "q" was the second letter of a /quit
		// typed just as the box opened.
		return &ModelDecision{ID: v.req.ID, Cancelled: true}
	}
	v.clampCursor()
	v.clampOffset(height)
	return nil
}

func (v *modelPickerView) header(height int) boxHeader {
	label := v.req.Title
	if label == "" {
		label = "CONTINUE ON"
	}
	return boxHeader{Label: label, Detail: v.req.Detail, Right: v.scrollInfo(height)}
}

func (v *modelPickerView) scrollable(height int) bool { return len(v.rows) > height }

func (v *modelPickerView) scrollInfo(height int) string {
	if !v.scrollable(height) {
		return ""
	}
	end := min(v.offset+height, len(v.rows))
	return itoa(v.offset+1) + v.theme.Symbols.Dash + itoa(end) + " of " + itoa(len(v.rows))
}

// buttonRow labels the keys, the way the diff viewer does: Enter picks what
// the cursor is on, so nothing here is a focused button competing with it.
// Cancel names Esc, the one key that does it: the row still said "q" after q
// stopped cancelling, so the key it offered did nothing and the real one was
// never shown.
func (v *modelPickerView) buttonRow(inner, height int) string {
	t := v.theme
	choose, decline := "continue", "cancel turn"
	if v.req.Preference {
		choose, decline = "switch", "keep current"
	}
	buttons := t.Button.Render(t.Symbols.Enter+"  "+choose) + "  " + t.Button.Render("esc  "+decline)
	for _, hints := range v.hints(height) {
		gap := inner - visibleWidth(buttons) - visibleWidth(hints) - 2
		if gap >= 2 {
			return buttons + strings.Repeat(" ", gap+2) + hints
		}
	}
	return buttons
}

// hints returns the navigation hints, widest first. The scroll hint appears
// only when the list is taller than the box, like everywhere else: offering a
// key for movement that is not possible makes a working key look broken.
func (v *modelPickerView) hints(height int) []string {
	t := v.theme
	key := func(k, label string) string {
		return t.HintKey.Render(k) + t.Hint.Render(" "+label)
	}
	join := func(parts ...string) string {
		return strings.Join(parts, t.Hint.Render(" "+t.Symbols.Dot+" "))
	}
	move := key(t.Symbols.ArrowUD, "move")
	num := key("1-9", "pick")
	if v.scrollable(height) {
		return []string{join(move, num, key("g/G", "ends")), join(move, num), move}
	}
	return []string{join(move, num), move}
}

func (v *modelPickerView) view(height int) []string {
	v.clampOffset(height)
	end := min(v.offset+height, len(v.rows))
	out := make([]string, 0, end-v.offset)
	for i := v.offset; i < end; i++ {
		marker := "  "
		if i == v.cursor && v.rows[i].kind == pickerModel {
			marker = v.theme.CursorRow.Render(v.theme.Symbols.Cursor) + " "
		}
		out = append(out, marker+v.rows[i].text)
	}
	return out
}
