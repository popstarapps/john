package terminalui

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const (
	inputMinHeight = 1
	inputMaxHeight = 8
)

// promptInput is the input box: a multi-line textarea with prompt history and
// an inline completion popup for slash commands and @-mentions.
type promptInput struct {
	ta    textarea.Model
	theme *Theme
	width int
	// hint follows the placeholder in an empty field; see view.
	hint string

	history []string
	histPos int // len(history) means "not browsing"
	draft   string

	// pastes holds the full text of anything too big to show in the box. The
	// box shows a placeholder; the application still gets every byte.
	pastes  map[int]string
	pasteNo int

	// reverse-incremental search over history
	searching bool
	query     string
	searchAt  int // index into history of the current match, len == none
	preSearch string

	pop popupState
}

// popupState is the completion popup. It is driven by what is typed before the
// cursor: "/" at the very start of the input, or "@" after whitespace.
type popupState struct {
	active bool
	kind   CompletionKind
	row    int // logical row of the trigger character
	col    int // rune column of the trigger character
	query  string
	items  []Completion
	sel    int
	// moved is set once the developer picks a candidate with the arrow keys.
	// Until then Enter submits what was typed rather than hijacking it.
	moved bool
	token uint64
}

func newPromptInput(o *Options) *promptInput {
	ta := textarea.New()
	ta.Placeholder = o.Placeholder
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = inputMaxHeight
	ta.SetHeight(inputMinHeight)
	ta.Prompt = ""
	ta.SetPromptFunc(2, func(i int) string {
		if i == 0 {
			return o.Theme.Prompt.Render(o.Theme.Symbols.User) + " "
		}
		return "  "
	})
	// Enter submits; newlines are deliberate.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("alt+enter", "ctrl+j", "shift+enter"),
		key.WithHelp("alt+enter", "newline"),
	)
	// The UI owns up/down so history and the popup can claim them first.
	ta.KeyMap.LinePrevious = key.NewBinding(key.WithKeys("ctrl+p"))
	ta.KeyMap.LineNext = key.NewBinding(key.WithKeys("ctrl+n"))
	// What is typed is drawn the way it will read once sent — the prompt line
	// in the transcript is Theme.User — so submitting a line does not change
	// how it looks. The cursor line is set per render in view, because the
	// textarea draws the placeholder inside it too.
	ta.FocusedStyle.CursorLine = o.Theme.Text
	ta.FocusedStyle.Text = o.Theme.User
	ta.FocusedStyle.Base = o.Theme.Text
	ta.FocusedStyle.Placeholder = o.Theme.Muted

	h := append([]string(nil), o.History...)
	if len(h) > o.HistoryLimit {
		h = h[len(h)-o.HistoryLimit:]
	}
	p := &promptInput{ta: ta, theme: o.Theme, history: h, histPos: len(h), pastes: map[int]string{}}
	// Focused where it lives, not before it is copied there: Focus keeps a
	// pointer to the textarea's own FocusedStyle, and one taken on the local
	// copy would never see a style set on p.ta afterwards.
	p.ta.Focus()
	return p
}

func (p *promptInput) setWidth(w int) {
	p.width = w
	p.ta.SetWidth(w)
}

func (p *promptInput) value() string { return p.ta.Value() }

// setPlaceholder changes the text shown in an empty box.
func (p *promptInput) setPlaceholder(s string) { p.ta.Placeholder = s }

// setValue replaces the input's contents, cursor at the end.
func (p *promptInput) setValue(s string) {
	p.ta.SetValue(s)
	p.syncHeight()
}

func (p *promptInput) reset() {
	p.ta.Reset()
	p.ta.SetHeight(inputMinHeight)
	p.pop = popupState{}
	p.histPos = len(p.history)
	p.draft = ""
	p.pastes = map[int]string{}
	p.searching, p.query, p.preSearch = false, "", ""
}

// collapsePaste replaces a large paste with a placeholder and remembers the
// text. A four-hundred line paste in an eight-line box tells the developer
// nothing except that they have lost sight of their own prompt.
func (p *promptInput) collapsePaste(text string, minLines int) bool {
	lines := strings.Count(text, "\n") + 1
	if lines < minLines {
		return false
	}
	p.pasteNo++
	p.pastes[p.pasteNo] = text
	p.ta.InsertString(pastePlaceholder(p.pasteNo, lines, len(text)))
	p.syncHeight()
	return true
}

func pastePlaceholder(n, lines, bytes int) string {
	return "[paste #" + itoa(n) + ": " + itoa(lines) + " lines, " + itoa(bytes) + " bytes]"
}

// expandPastes turns the placeholders back into the text they stand for. What
// is shown is a summary; what is sent is everything.
func (p *promptInput) expandPastes(text string) string {
	for n, full := range p.pastes {
		lines := strings.Count(full, "\n") + 1
		text = strings.ReplaceAll(text, pastePlaceholder(n, lines, len(full)), full)
	}
	return text
}

// startSearch begins a reverse-incremental search over the prompt history.
func (p *promptInput) startSearch() {
	if p.searching {
		p.nextMatch()
		return
	}
	p.searching = true
	p.query = ""
	p.preSearch = p.ta.Value()
	p.searchAt = len(p.history)
}

// endSearch leaves search mode, keeping the match or restoring the draft.
func (p *promptInput) endSearch(keep bool) {
	if !keep {
		p.setValue(p.preSearch)
	}
	p.searching, p.query, p.preSearch = false, "", ""
}

// searchKey handles a key in search mode. It reports whether the search is
// still running.
func (p *promptInput) searchKey(msg tea.KeyMsg) bool {
	switch msg.Type {
	case tea.KeyEsc:
		p.endSearch(false)
		return false
	case tea.KeyEnter, tea.KeyRight:
		p.endSearch(true)
		return false
	case tea.KeyCtrlR:
		p.nextMatch()
	case tea.KeyBackspace:
		if r := []rune(p.query); len(r) > 0 {
			p.query = string(r[:len(r)-1])
			p.searchAt = len(p.history)
			p.findMatch()
		}
	case tea.KeySpace:
		p.query += " "
		p.searchAt = len(p.history)
		p.findMatch()
	case tea.KeyRunes:
		p.query += string(msg.Runes)
		p.searchAt = len(p.history)
		p.findMatch()
	}
	return true
}

// findMatch looks backwards from searchAt for the newest entry containing the
// query.
func (p *promptInput) findMatch() {
	if p.query == "" {
		p.setValue(p.preSearch)
		return
	}
	q := strings.ToLower(p.query)
	for i := min(p.searchAt, len(p.history)) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(p.history[i]), q) {
			p.searchAt = i
			p.setValue(p.history[i])
			return
		}
	}
}

// nextMatch steps to the next older match. findMatch always searches strictly
// below searchAt, so calling it again walks backwards through history.
func (p *promptInput) nextMatch() {
	if p.query == "" || p.searchAt <= 0 {
		return
	}
	p.findMatch()
}

// searchLine renders the search prompt shown above the input box.
func (p *promptInput) searchLine(width int) string {
	t := p.theme
	label := t.HintKey.Render("search") + t.Hint.Render(": ")
	q := p.query
	if q == "" {
		q = t.Symbols.Ellipsis
	}
	line := "  " + label + t.Text.Render(q)
	hints := t.HintKey.Render("ctrl+r") + t.Hint.Render(" next") +
		t.Hint.Render(" "+t.Symbols.Dot+" ") + t.HintKey.Render(t.Symbols.Enter) + t.Hint.Render(" accept") +
		t.Hint.Render(" "+t.Symbols.Dot+" ") + t.HintKey.Render("esc") + t.Hint.Render(" cancel")
	if gap := width - visibleWidth(line) - visibleWidth(hints) - 2; gap > 2 {
		line += strings.Repeat(" ", gap) + hints
	}
	return line
}

func (p *promptInput) pushHistory(s string, limit int) {
	if s == "" {
		return
	}
	if n := len(p.history); n > 0 && p.history[n-1] == s {
		p.histPos = len(p.history)
		return
	}
	p.history = append(p.history, s)
	if len(p.history) > limit {
		p.history = p.history[len(p.history)-limit:]
	}
	p.histPos = len(p.history)
}

// cursorRowCol returns the cursor position in logical (unwrapped) coordinates.
func (p *promptInput) cursorRowCol() (int, int) {
	li := p.ta.LineInfo()
	return p.ta.Line(), li.StartColumn + li.ColumnOffset
}

func (p *promptInput) rows() []string {
	return strings.Split(p.ta.Value(), "\n")
}

// syncHeight grows the box with the text, up to inputMaxHeight.
func (p *promptInput) syncHeight() {
	n := p.ta.LineCount()
	if n < inputMinHeight {
		n = inputMinHeight
	}
	if n > inputMaxHeight {
		n = inputMaxHeight
	}
	if p.ta.Height() != n {
		p.ta.SetHeight(n)
	}
}

// detectTrigger recomputes the completion trigger from the text before the
// cursor. It returns whether the popup should be open, and for what.
func (p *promptInput) detectTrigger(o *Options) (kind CompletionKind, row, col int, query string, ok bool) {
	row, col = p.cursorRowCol()
	rows := p.rows()
	if row >= len(rows) {
		return 0, 0, 0, "", false
	}
	line := []rune(rows[row])
	if col > len(line) {
		col = len(line)
	}
	before := line[:col]

	// Slash commands: only at the very beginning of the first line.
	if row == 0 && len(before) > 0 && before[0] == '/' {
		q := string(before[1:])
		if !strings.ContainsAny(q, " \t") {
			return CompletionSlash, 0, 0, q, true
		}
	}
	if !o.FileCompletion {
		return 0, 0, 0, "", false
	}
	// @-mentions: an "@" at the start of a word.
	for i := len(before) - 1; i >= 0; i-- {
		r := before[i]
		if r == ' ' || r == '\t' {
			return 0, 0, 0, "", false
		}
		if r == '@' {
			if i > 0 && before[i-1] != ' ' && before[i-1] != '\t' {
				return 0, 0, 0, "", false
			}
			return CompletionFile, row, i, string(before[i+1:]), true
		}
	}
	return 0, 0, 0, "", false
}

// filterCommands answers slash completion locally from the configured commands.
func filterCommands(cmds []Command, query string) []Completion {
	out := make([]Completion, 0, len(cmds))
	q := strings.ToLower(query)
	for _, c := range cmds {
		if q != "" && !strings.HasPrefix(strings.ToLower(c.Name), q) {
			continue
		}
		display := "/" + c.Name
		if c.Args != "" {
			display += " " + c.Args
		}
		out = append(out, Completion{Value: "/" + c.Name, Display: display, Detail: c.Description})
	}
	return out
}

// accept inserts the selected completion, replacing everything from the trigger
// to the cursor.
func (p *promptInput) accept() {
	if !p.pop.active || len(p.pop.items) == 0 {
		return
	}
	item := p.pop.items[p.pop.sel]
	rows := p.rows()
	if p.pop.row >= len(rows) {
		return
	}
	_, col := p.cursorRowCol()
	line := []rune(rows[p.pop.row])
	if col > len(line) {
		col = len(line)
	}
	insert := item.Value
	if p.pop.kind == CompletionFile {
		insert = "@" + item.Value
	}
	newLine := string(line[:p.pop.col]) + insert + " " + string(line[col:])
	newCol := len([]rune(string(line[:p.pop.col]) + insert + " "))
	rows[p.pop.row] = newLine

	p.ta.SetValue(strings.Join(rows, "\n"))
	// SetValue leaves the cursor at the end of the buffer; walk back to where
	// the user was actually typing.
	for i := len(rows) - 1; i > p.pop.row; i-- {
		p.ta.CursorUp()
	}
	p.ta.SetCursor(newCol)
	p.pop = popupState{}
	p.syncHeight()
}

// update feeds a key to the textarea and refreshes derived state.
func (p *promptInput) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	p.ta, cmd = p.ta.Update(msg)
	p.syncHeight()
	return cmd
}

// historyPrev moves back through history, saving the current draft first.
func (p *promptInput) historyPrev() bool {
	if len(p.history) == 0 || p.histPos == 0 {
		return false
	}
	if p.histPos == len(p.history) {
		p.draft = p.ta.Value()
	}
	p.histPos--
	p.ta.SetValue(p.history[p.histPos])
	p.syncHeight()
	return true
}

// historyNext moves forward, ending at the saved draft.
func (p *promptInput) historyNext() bool {
	if p.histPos >= len(p.history) {
		return false
	}
	p.histPos++
	if p.histPos == len(p.history) {
		p.ta.SetValue(p.draft)
	} else {
		p.ta.SetValue(p.history[p.histPos])
	}
	p.syncHeight()
	return true
}

// view draws the box. An empty box shows the placeholder on the cursor line,
// and a bold cursor line would make it bold as well, so the typed text's
// style applies only once there is typed text.
//
// hint, when set, follows the placeholder on its line, in the hint colour.
func (p *promptInput) view() string {
	if p.ta.Value() != "" {
		p.ta.FocusedStyle.CursorLine = p.theme.User
		return p.ta.View()
	}
	p.ta.FocusedStyle.CursorLine = p.theme.Text
	v := p.ta.View()
	if p.hint == "" || p.ta.Placeholder == "" {
		return v
	}
	first, rest, _ := strings.Cut(v, "\n")
	used := visibleWidth(strings.TrimRight(ansi.Strip(first), " "))
	if used+2+visibleWidth(p.hint) > p.width {
		return v
	}
	first = ansi.Truncate(first, used, "") + "  " + p.hint
	if rest == "" {
		return first
	}
	return first + "\n" + rest
}

// popupView renders the completion list above the input.
func (p *promptInput) popupView(width int) []string {
	if !p.pop.active || len(p.pop.items) == 0 {
		return nil
	}
	const maxRows = 8
	items := p.pop.items
	start := 0
	if len(items) > maxRows {
		start = p.pop.sel - maxRows/2
		if start < 0 {
			start = 0
		}
		if start > len(items)-maxRows {
			start = len(items) - maxRows
		}
	}
	end := min(start+maxRows, len(items))

	out := make([]string, 0, end-start+1)
	for i := start; i < end; i++ {
		it := items[i]
		label := it.Display
		if label == "" {
			label = it.Value
		}
		style := p.theme.PopupItem
		marker := "  "
		if i == p.pop.sel {
			style = p.theme.PopupSelected
			marker = p.theme.PopupSelected.Render(p.theme.Symbols.Cursor) + " "
		}
		line := marker + style.Render(truncPlain(label, width-4, p.theme.Symbols.Ellipsis))
		if it.Detail != "" {
			room := width - visibleWidth(line) - 2
			if room > 6 {
				line += "  " + p.theme.PopupDetail.Render(truncPlain(it.Detail, room, p.theme.Symbols.Ellipsis))
			}
		}
		out = append(out, line)
	}
	if len(items) > maxRows {
		out = append(out, p.theme.Hint.Render("  "+itoa(p.pop.sel+1)+"/"+itoa(len(items))))
	}
	return out
}
