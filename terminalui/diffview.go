package terminalui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type diffRowKind int

const (
	rowFile diffRowKind = iota
	rowHunk
	rowLine
)

type diffRow struct {
	kind diffRowKind
	file int
	text string // pre-rendered, without the cursor marker
}

// diffView is the review step: the whole set of sandbox changes, per-file
// selection, and one decision at the end. It is the only place in the UI where
// something leaves the sandbox and reaches the developer's working directory,
// which is why it works this hard to make a change legible rather than merely
// visible.
type diffView struct {
	rev   DiffReview
	theme *Theme
	hl    *highlighter

	sel      []bool
	expanded []bool

	// filter narrows the review to matching paths. A hundred-file review that
	// cannot be narrowed is a review that gets skimmed.
	filter    string
	filtering bool
	visible   []int // file indices passing the filter

	rows   []diffRow
	cursor int
	offset int
	width  int
	dirty  bool
}

func newDiffView(rev DiffReview, t *Theme, hl *highlighter) *diffView {
	v := &diffView{
		rev:      rev,
		theme:    t,
		hl:       hl,
		sel:      make([]bool, len(rev.Files)),
		expanded: make([]bool, len(rev.Files)),
	}
	for i, f := range rev.Files {
		v.sel[i] = f.Selected
	}
	// A single-file review is expanded by default: there is nothing to choose
	// between, so the diff itself is the point.
	if len(rev.Files) == 1 {
		v.expanded[0] = true
	}
	v.dirty = true
	return v
}

func (v *diffView) layout(width int) {
	if width == v.width && !v.dirty {
		return
	}
	v.width = width
	v.dirty = false
	t := v.theme
	w := max(width-2, 8)

	v.visible = v.visible[:0]
	rows := make([]diffRow, 0, 64)
	for i, f := range v.rev.Files {
		if !v.matches(f.Path) {
			continue
		}
		v.visible = append(v.visible, i)
		rows = append(rows, diffRow{kind: rowFile, file: i, text: v.fileRowText(f, i, w)})
		if !v.expanded[i] {
			continue
		}
		for _, h := range f.Hunks {
			rows = append(rows, diffRow{
				kind: rowHunk, file: i,
				text: "    " + t.DiffHunk.Render(truncPlain(h.Header, w-4, t.Symbols.Ellipsis)),
			})
			rows = append(rows, v.hunkRows(f, h, i, w)...)
		}
	}
	v.rows = rows
	v.clampCursor()
}

func (v *diffView) fileRowText(f DiffFile, i, w int) string {
	t := v.theme
	box, boxStyle := t.Symbols.Unchecked, t.Muted
	if v.sel[i] {
		box, boxStyle = t.Symbols.Selected, t.Selected
	}
	stat := ""
	switch {
	case f.Binary:
		stat = "binary"
	case f.Added > 0 && f.Removed > 0:
		stat = t.Symbols.Add + itoa(f.Added) + " " + t.Symbols.Remove + itoa(f.Removed)
	case f.Added > 0:
		stat = t.Symbols.Add + itoa(f.Added)
	case f.Removed > 0:
		stat = t.Symbols.Remove + itoa(f.Removed)
	}
	marker := " "
	if len(f.Hunks) > 0 {
		marker = "+"
		if v.expanded[i] {
			marker = "-"
		}
	}
	head := boxStyle.Render("["+box+"] ") + t.Muted.Render(marker+" ") +
		t.DiffPath.Render(truncPlain(f.Path, w-10-len(stat), t.Symbols.Ellipsis))
	if stat != "" {
		head += "  " + t.Meta.Render(stat)
	}
	return head
}

// hunkRows renders one hunk, pairing removed lines with the added lines that
// replaced them so the part that actually changed can be picked out.
func (v *diffView) hunkRows(f DiffFile, h DiffHunk, file, w int) []diffRow {
	spans := pairedSpans(h.Lines)
	out := make([]diffRow, 0, len(h.Lines))
	for i, l := range h.Lines {
		out = append(out, diffRow{
			kind: rowLine, file: file,
			text: "    " + v.lineText(f.Path, l, spans[i], w-6),
		})
	}
	return out
}

// pairedSpans walks a hunk, matches each run of removed lines against the run
// of added lines that follows it, and returns the changed span of every line.
func pairedSpans(lines []DiffLine) []wordSpan {
	spans := make([]wordSpan, len(lines))
	for i := 0; i < len(lines); {
		if lines[i].Kind != DiffRemove {
			i++
			continue
		}
		rem := i
		for i < len(lines) && lines[i].Kind == DiffRemove {
			i++
		}
		add := i
		for i < len(lines) && lines[i].Kind == DiffAdd {
			i++
		}
		for j := 0; rem+j < add && add+j < i; j++ {
			// On the text as it is drawn, not as it was sent: lineText
			// expands tabs before it lays the span over the line, so a span
			// counted on the raw text fell three columns short per tab.
			b, a := changedSpan(expandTabs(lines[rem+j].Text), expandTabs(lines[add+j].Text))
			spans[rem+j], spans[add+j] = b, a
		}
	}
	return spans
}

func (v *diffView) lineText(path string, l DiffLine, span wordSpan, w int) string {
	t := v.theme
	style, emph, sign := t.DiffCtx, t.DiffCtx, " "
	switch l.Kind {
	case DiffAdd:
		style, emph, sign = t.DiffAdd, t.DiffAddEmph, "+"
	case DiffRemove:
		style, emph, sign = t.DiffRemove, t.DiffRemoveEmph, "-"
	case DiffMeta:
		style, emph = t.Muted, t.Muted
	}

	text := truncPlain(l.Text, w, t.Symbols.Ellipsis)
	runes := []rune(text)
	code := func(s string) string {
		if v.hl != nil {
			return v.hl.line(path, s)
		}
		return style.Render(s)
	}

	// Syntax colour carries the code; the sign column and the emphasis carry
	// the change. Without highlighting, the whole line takes the diff colour.
	var body string
	if span.empty() || span.start >= len(runes) {
		body = code(text)
	} else {
		end := min(span.end, len(runes))
		body = code(string(runes[:span.start])) +
			emph.Render(string(runes[span.start:end])) +
			code(string(runes[end:]))
	}
	return style.Render(sign+" ") + body
}

// matches reports whether a path survives the current filter.
func (v *diffView) matches(path string) bool {
	if v.filter == "" {
		return true
	}
	return strings.Contains(strings.ToLower(path), strings.ToLower(v.filter))
}

func (v *diffView) clampCursor() {
	if v.cursor >= len(v.rows) {
		v.cursor = len(v.rows) - 1
	}
	if v.cursor < 0 {
		v.cursor = 0
	}
}

func (v *diffView) clampOffset(height int) {
	if v.cursor < v.offset {
		v.offset = v.cursor
	}
	if v.cursor >= v.offset+height {
		v.offset = v.cursor - height + 1
	}
	maxOff := len(v.rows) - height
	if maxOff < 0 {
		maxOff = 0
	}
	if v.offset > maxOff {
		v.offset = maxOff
	}
	if v.offset < 0 {
		v.offset = 0
	}
}

func (v *diffView) currentFile() int {
	if v.cursor >= 0 && v.cursor < len(v.rows) {
		return v.rows[v.cursor].file
	}
	return -1
}

func (v *diffView) selectedPaths() []string {
	var out []string
	for i, f := range v.rev.Files {
		if v.sel[i] {
			out = append(out, f.Path)
		}
	}
	return out
}

func (v *diffView) anySelected() bool {
	for _, s := range v.sel {
		if s {
			return true
		}
	}
	return false
}

// update handles a key. It returns a decision when the review is over.
func (v *diffView) update(msg tea.KeyMsg, height int) *DiffDecision {
	if v.filtering {
		v.updateFilter(msg)
		v.clampCursor()
		v.clampOffset(height)
		return nil
	}

	switch msg.String() {
	case "j", "down", "ctrl+n":
		v.cursor++
	case "k", "up", "ctrl+p":
		v.cursor--
	case "ctrl+d", "pgdown":
		v.cursor += height / 2
	case "ctrl+u", "pgup":
		v.cursor -= height / 2
	case "g", "home":
		v.cursor = 0
	case "G", "end":
		v.cursor = len(v.rows) - 1
	case "]":
		v.jumpFile(1)
	case "[":
		v.jumpFile(-1)
	case "/":
		v.filtering = true
	case " ":
		if f := v.currentFile(); f >= 0 {
			v.sel[f] = !v.sel[f]
			v.dirty = true
		}
	case "s":
		// Only what is on screen: selecting files the filter is hiding would
		// be selecting files the developer has not looked at.
		all := true
		for _, i := range v.visible {
			all = all && v.sel[i]
		}
		for _, i := range v.visible {
			v.sel[i] = !all
		}
		v.dirty = true
	case "enter", "tab", "right", "left":
		if f := v.currentFile(); f >= 0 && len(v.rev.Files[f].Hunks) > 0 {
			v.expanded[f] = !v.expanded[f]
			// Keep the cursor on the file row so collapsing never strands it.
			v.cursor = v.fileRow(f)
			v.dirty = true
		}
	case "a":
		if v.anySelected() {
			return &DiffDecision{ID: v.rev.ID, Apply: true, Paths: v.selectedPaths()}
		}
	case "esc", "q":
		return &DiffDecision{ID: v.rev.ID, Apply: false}
	}
	v.clampCursor()
	v.clampOffset(height)
	return nil
}

func (v *diffView) updateFilter(msg tea.KeyMsg) {
	switch msg.Type {
	case tea.KeyEsc:
		v.filter, v.filtering, v.dirty = "", false, true
	case tea.KeyEnter:
		v.filtering = false
	case tea.KeyBackspace:
		if r := []rune(v.filter); len(r) > 0 {
			v.filter = string(r[:len(r)-1])
			v.dirty = true
		}
	case tea.KeySpace:
		v.filter += " "
		v.dirty = true
	case tea.KeyRunes:
		v.filter += string(msg.Runes)
		v.dirty = true
	}
}

func (v *diffView) fileRow(file int) int {
	for i, r := range v.rows {
		if r.kind == rowFile && r.file == file {
			return i
		}
	}
	return v.cursor
}

func (v *diffView) jumpFile(delta int) {
	f := v.currentFile()
	if f < 0 {
		return
	}
	// Move within the visible set, not the underlying file list.
	pos := -1
	for i, idx := range v.visible {
		if idx == f {
			pos = i
			break
		}
	}
	if pos < 0 {
		return
	}
	next := pos + delta
	if next < 0 || next >= len(v.visible) {
		if delta < 0 {
			v.cursor = v.fileRow(f)
		}
		return
	}
	v.cursor = v.fileRow(v.visible[next])
}

// header is the box's title row: what this is, and what it is about.
func (v *diffView) header(height int) boxHeader {
	detail := v.rev.Title
	if detail == "" {
		detail = "the sandbox against your working directory"
	}
	if v.filtering || v.filter != "" {
		detail = "filter: " + v.filter
		if v.filtering {
			detail += "_"
		}
	}
	return boxHeader{Label: "REVIEW CHANGES", Detail: detail, Right: v.scrollInfo(height)}
}

// summary rides on the bottom border, so the totals stay in view while the
// hunks scroll past.
func (v *diffView) summary() string {
	t := v.theme
	added, removed := 0, 0
	for _, f := range v.rev.Files {
		added += f.Added
		removed += f.Removed
	}
	files := itoa(len(v.rev.Files)) + " files"
	if v.filter != "" {
		files = itoa(len(v.visible)) + " of " + itoa(len(v.rev.Files)) + " files"
	}
	return files + " " + t.Symbols.Dot + " " +
		t.Symbols.Add + itoa(added) + " " + t.Symbols.Remove + itoa(removed) + " " +
		t.Symbols.Dot + " " + itoa(len(v.selectedPaths())) + " selected"
}

func (v *diffView) scrollable(height int) bool { return len(v.rows) > height }

func (v *diffView) scrollInfo(height int) string {
	if !v.scrollable(height) {
		return ""
	}
	end := min(v.offset+height, len(v.rows))
	return itoa(v.offset+1) + v.theme.Symbols.Dash + itoa(end) + " of " + itoa(len(v.rows))
}

// buttonRow renders the actions as buttons with the keys that fire them, and
// the navigation keys beside them when there is room.
//
// Unlike the approval box, nothing here is "focused": Enter expands a file, so
// a filled button would promise something Enter does not do. The buttons are
// labels for keys, and the apply button greys out while nothing is selected —
// which is also when the key does nothing.
func (v *diffView) buttonRow(inner, height int) string {
	t := v.theme
	apply := t.Button.Render("a  apply selected")
	if !v.anySelected() {
		apply = t.ButtonMuted.Render("a  apply selected")
	}
	buttons := apply + "  " + t.Button.Render("q  cancel")

	for _, hints := range v.hints(height) {
		gap := inner - visibleWidth(buttons) - visibleWidth(hints) - 2
		if gap >= 2 {
			return buttons + strings.Repeat(" ", gap+2) + hints
		}
	}
	return buttons
}

// hints returns the navigation hints, widest first.
func (v *diffView) hints(height int) []string {
	t := v.theme
	key := func(k, label string) string {
		return t.HintKey.Render(k) + t.Hint.Render(" "+label)
	}
	join := func(parts ...string) string {
		return strings.Join(parts, t.Hint.Render(" "+t.Symbols.Dot+" "))
	}
	if v.filtering {
		return []string{join(key(t.Symbols.Enter, "apply filter"), key("esc", "clear")), key("esc", "clear")}
	}
	move := key(t.Symbols.ArrowUD, "move")
	sel := key("space", "select")
	expand := key(t.Symbols.Enter, "expand")
	filter := key("/", "filter")
	return []string{
		join(move, sel, expand, filter),
		join(move, sel, expand),
		join(move, sel),
		sel,
	}
}

func (v *diffView) view(height int) []string {
	v.clampOffset(height)
	end := min(v.offset+height, len(v.rows))
	out := make([]string, 0, end-v.offset)
	for i := v.offset; i < end; i++ {
		marker := "  "
		if i == v.cursor {
			marker = v.theme.CursorRow.Render(v.theme.Symbols.Cursor) + " "
		}
		out = append(out, marker+v.rows[i].text)
	}
	if len(out) == 0 && v.filter != "" {
		out = append(out, "  "+v.theme.Muted.Render("no files match "+v.filter))
	}
	return out
}
