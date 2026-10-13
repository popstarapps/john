package terminalui

import (
	xansi "github.com/charmbracelet/x/ansi"
	"regexp"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// markdownRenderer renders assistant messages. It caches one glamour renderer
// per width, because building one is expensive and the width only changes on
// resize.
type markdownRenderer struct {
	enabled bool
	style   ansi.StyleConfig
	cache   map[int]*glamour.TermRenderer
}

func newMarkdownRenderer(enabled, ascii bool, r *lipgloss.Renderer) *markdownRenderer {
	m := &markdownRenderer{enabled: enabled, cache: map[int]*glamour.TermRenderer{}}
	switch {
	case r.ColorProfile() == termenv.Ascii:
		m.style = styles.NoTTYStyleConfig
	case r.HasDarkBackground():
		m.style = styles.DarkStyleConfig
	default:
		m.style = styles.LightStyleConfig
	}
	if ascii {
		// ASCII is about glyphs, not colour: keep the palette, replace the
		// bullets glamour would otherwise draw with box-drawing characters.
		m.style.Item.BlockPrefix = "- "
		m.style.Task.Ticked = "[x] "
		m.style.Task.Unticked = "[ ] "
	}
	// Inline code keeps its colours and loses the space glamour pads it with
	// on each side. Copied out of scrollback, or on a terminal that drops the
	// background, the padding is just spaces: "run  npm install  and" — and
	// one before a full stop or a "!" that the agent never wrote.
	if m.style.Code.Prefix == " " {
		m.style.Code.Prefix = ""
	}
	if m.style.Code.Suffix == " " {
		m.style.Code.Suffix = ""
	}
	// The UI owns indentation and blank lines around a message, so the
	// document itself gets no margin and no leading newline.
	zero := uint(0)
	m.style.Document.Margin = &zero
	m.style.Document.BlockPrefix = ""
	m.style.Document.BlockSuffix = ""
	return m
}

// render turns markdown into styled lines wrapped to width. On any failure it
// falls back to plain wrapping, because a malformed fence must never cost the
// developer the message.
func (m *markdownRenderer) render(text string, width int) []string {
	text = strings.TrimRight(sanitizeBlock(text), "\n")
	if text == "" {
		return nil
	}
	if !m.enabled || width < 20 {
		return wrapPlain(text, max(width, 1))
	}
	text = chatLineBreaks(text)
	// Glamour lets the lines after an inline code span run a column or two
	// past the width it was given. The frame then wrapped them again, and a
	// paragraph ended "…Vite, and" / "the" / "Vite React plugin." So a
	// render that does not fit is redone a column narrower: a line a little
	// short of the edge is invisible, one broken twice is not.
	var lines []string
	for wrap := width; ; wrap-- {
		var ok bool
		if lines, ok = m.renderAt(text, wrap); !ok {
			return wrapPlain(unmarked(text), width)
		}
		if fits(lines, width) || wrap <= width-4 {
			break
		}
	}
	lines = collapseBlankRuns(lines)
	for i, l := range lines {
		lines[i] = unmarked(l)
	}
	// Glamour emits a leading blank line for some documents; drop leading and
	// trailing empties so blocks sit tight against their glyph.
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return wrapPlain(unmarked(text), width)
	}
	return lines
}

// codeBlank marks a blank line inside fenced code, put there by
// chatLineBreaks, so that collapseBlankRuns can tell the code's own blank
// lines from the renderer's. It is an invisible separator, of no width, and
// render takes it out again.
const codeBlank = "⁣"

func unmarked(s string) string { return strings.ReplaceAll(s, codeBlank, "") }

// collapseBlankRuns leaves one blank line where the renderer put several. A
// list nested in another ends both, and glamour closes each with a blank line
// of its own: "1. a" / "   - b" then a paragraph came out two blank lines
// away from it, where a flat list is one. Blank lines in code are marked with
// codeBlank and are not blank here, since two may be what the code says. They
// used to be kept by not collapsing any message with code in it, which left
// every nested list in such a message two lines from what followed it.
func collapseBlankRuns(lines []string) []string {
	out := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(xansi.Strip(l)) == "" && len(out) > 0 &&
			strings.TrimSpace(xansi.Strip(out[len(out)-1])) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// renderAt is one glamour render wrapped at width, split into lines.
func (m *markdownRenderer) renderAt(text string, width int) ([]string, bool) {
	r, ok := m.cache[width]
	if !ok {
		var err error
		r, err = glamour.NewTermRenderer(
			glamour.WithStyles(m.style),
			glamour.WithWordWrap(width),
			glamour.WithEmoji(),
		)
		if err != nil {
			return nil, false
		}
		m.cache[width] = r
	}
	out, err := r.Render(text)
	if err != nil {
		return nil, false
	}
	lines := closeJoined(rewrapParagraphs(strings.Split(strings.Trim(out, "\n"), "\n"), width))
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return hangIndent(lines, width), true
}

// rewrapParagraphs wraps each paragraph of plain text again, with styling
// kept. The renderer loses count of a line's width after an inline code span,
// and breaks where nothing needed breaking: "…vite, and" / "@vitejs/plugin-" /
// "react." at a width where "@vitejs/plugin-react." fitted on the second line
// whole. Only plain text is touched — lines that start at the margin and are
// not list items, quotes, tables or code, which the renderer indents or draws
// with rules of their own.
func rewrapParagraphs(lines []string, width int) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i
		for j < len(lines) && plainLine(lines[j]) {
			j++
		}
		switch {
		case j == i:
			out = append(out, lines[i])
			i++
		case j-i == 1:
			out = append(out, lines[i])
			i = j
		default:
			text := trimVisible(lines[i])
			for _, l := range lines[i+1 : j] {
				// A line the renderer ended at a hyphen inside a word is
				// that word, carried on: no space goes back in.
				if plain := xansi.Strip(text); !hyphenBreak.MatchString(plain) {
					text += " "
				}
				text += trimVisible(l)
			}
			// The wrapper breaks at every hyphen, and "@vitejs/plugin-" on a
			// line with "react." under it is a name taken apart. Hyphens
			// are held together while it wraps, so a word goes whole to the
			// next line, and only a word longer than the line is cut.
			wrapped := xansi.Wrap(strings.ReplaceAll(text, "-", heldHyphen), width, "")
			out = append(out, strings.Split(strings.ReplaceAll(wrapped, heldHyphen, "-"), "\n")...)
			i = j
		}
	}
	return out
}

// heldHyphen is a non-breaking hyphen, as wide as the one it stands in for.
const heldHyphen = "‑"

// hyphenBreak is a line the renderer broke at a hyphen within a word.
var hyphenBreak = regexp.MustCompile(`[\p{L}\p{N}]-$`)

func plainLine(l string) bool {
	p := xansi.Strip(l)
	return strings.TrimSpace(p) != "" && !strings.HasPrefix(p, " ") && !renderedItem.MatchString(p) &&
		!strings.ContainsAny(p, "│┃─┼")
}

// renderedItem is the start of a list item as the renderer draws it: the
// indentation, then the bullet, number or task box, then a space.
var renderedItem = regexp.MustCompile(`^(\s*)(?:[•*-]|\d+\.|\[[ x]\])\s+`)

// hangIndent lines a list item's wrapped lines up under its text rather than
// under its bullet. The renderer starts them at the item's indentation, so
// the second line of a long item — "build tools and don't affect the game",
// under "2 vulnerabilities reported" — read as a paragraph of its own.
//
// An item that wraps is wrapped again, narrower by its bullet, with styling
// kept, and its later lines indented to where its text starts.
func hangIndent(lines []string, width int) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		m := renderedItem.FindStringSubmatch(xansi.Strip(lines[i]))
		if m == nil {
			out = append(out, lines[i])
			continue
		}
		indent := xansi.StringWidth(m[0])
		// The item's own wrapped lines: what follows it, up to a blank line,
		// another item, or something indented as far as its text (a nested
		// block of its own).
		end := i + 1
		for ; end < len(lines); end++ {
			plain := xansi.Strip(lines[end])
			lead := len(plain) - len(strings.TrimLeft(plain, " "))
			if strings.TrimSpace(plain) == "" || renderedItem.MatchString(plain) || lead >= indent {
				break
			}
		}
		if end == i+1 || width-indent < 10 {
			out = append(out, lines[i])
			continue
		}
		text := trimVisible(xansi.TruncateLeft(lines[i], indent, ""))
		for _, l := range lines[i+1 : end] {
			plain := xansi.Strip(l)
			lead := len(plain) - len(strings.TrimLeft(plain, " "))
			text += " " + trimVisible(xansi.TruncateLeft(l, lead, ""))
		}
		bullet := xansi.Truncate(lines[i], indent, "")
		for j, w := range strings.Split(xansi.Wrap(text, width-indent, ""), "\n") {
			if j == 0 {
				out = append(out, bullet+w)
			} else {
				out = append(out, strings.Repeat(" ", indent)+w)
			}
		}
		i = end - 1
	}
	return out
}

// trimVisible drops the spaces at the end of a styled line, which the
// renderer pads to its width with.
func trimVisible(s string) string {
	plain := xansi.Strip(s)
	return xansi.Truncate(s, xansi.StringWidth(strings.TrimRight(plain, " ")), "")
}

func fits(lines []string, width int) bool {
	for _, l := range lines {
		if visibleWidth(l) > width {
			return false
		}
	}
	return true
}

var listItem = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s`)

// joined marks a line chatLineBreaks split off into a paragraph of its own,
// so the blank line the renderer puts between paragraphs can be taken out
// again. It is a word joiner: invisible, and no width.
const joined = "\u2060"

// chatLineBreaks keeps the line breaks a model writes. In Markdown a single
// newline inside a paragraph is a space, but a model writing one item per
// line — "✅ package.json …" then "✅ vite.config.js …" — means a new line,
// and joined into one paragraph the items ran together; the over-long line
// that made was then wrapped mid-word ("src/App." / "jsx"). The renderer
// honours no hard line break, so each such line becomes a paragraph of its
// own, marked, and closeJoined removes the blank lines between them after.
// An unindented line after a list ends the list rather than continuing its
// last item. Fenced code, headings and table rows are left as they are.
func chatLineBreaks(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	fence := false
	inList := false
	prevText := false // the line before was text a single newline would join
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fence = !fence
			out = append(out, line)
			prevText = false
			continue
		}
		if fence {
			if trimmed == "" {
				line = codeBlank // the code's own blank line; see collapseBlankRuns
			}
			out = append(out, line)
			continue
		}
		if trimmed == "" {
			inList, prevText = false, false
			out = append(out, line)
			continue
		}
		isItem := listItem.MatchString(line)
		indented := len(line) > len(strings.TrimLeft(line, " \t"))
		plain := !isItem && !indented && !strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "|")
		switch {
		case inList && plain:
			out = append(out, "") // the list is over
			inList = false
		case prevText && plain:
			out = append(out, "") // its own line, and marked to stay close
			line = joined + line
		}
		if isItem {
			inList = true
		}
		prevText = plain
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// closeJoined takes out the blank line before each line chatLineBreaks
// marked, and the mark.
func closeJoined(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, joined) {
			if n := len(out); n > 0 && strings.TrimSpace(xansi.Strip(out[n-1])) == "" {
				out = out[:n-1]
			}
			l = strings.ReplaceAll(l, joined, "")
		}
		out = append(out, l)
	}
	return out
}
