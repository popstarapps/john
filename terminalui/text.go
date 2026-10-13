package terminalui

import (
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
	reflowansi "github.com/muesli/reflow/ansi"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"
)

// wrapPlain wraps unstyled text to width, breaking on words first and hard
// breaking anything still too long. Existing newlines are preserved.
func wrapPlain(s string, width int) []string {
	if width < 1 {
		width = 1
	}
	s = strings.ReplaceAll(s, "\t", "    ")
	wrapped := wrap.String(wordWrap(s, width), width)
	lines := strings.Split(wrapped, "\n")
	// Trailing whitespace shows up as selection artifacts when copied out of
	// scrollback, so it goes.
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// wordWrap breaks on spaces only. reflow also breaks after a hyphen by
// default, but leaves the hyphen out of its count of the line, so a line with
// a few `--flags` in it came out that many columns too long, and the hard wrap
// after it cut words in half: "a" at the end of one line, "nd" on the next.
func wordWrap(s string, width int) string {
	w := wordwrap.NewWriter(width)
	w.Breakpoints = nil
	w.Write([]byte(s))
	w.Close()
	return w.String()
}

// expandTabs is how a tab is drawn everywhere in the UI: four columns.
func expandTabs(s string) string { return strings.ReplaceAll(s, "\t", "    ") }

// truncPlain cuts unstyled text to width, appending the theme ellipsis when it
// had to cut. It is display-width aware, so CJK and emoji do not overflow.
func truncPlain(s string, width int, tail string) string {
	s = expandTabs(s)
	s = joinLines(s)
	s = sanitizeLine(s)
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= width {
		return s
	}
	tw := runewidth.StringWidth(tail)
	if width <= tw {
		return runewidth.Truncate(s, width, "")
	}
	return runewidth.Truncate(s, width, tail)
}

// joinLines puts text of several lines on one, the way a shell would read a
// multi-line command: lines joined by "; ", or by a space after a line that
// already ends in an operator. Left to sanitizeLine the newlines just vanished,
// and `cd / && serve &` then `sleep 2` read as `serve &sleep 2`.
func joinLines(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	var b strings.Builder
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			if prev := b.String(); strings.ContainsAny(prev[len(prev)-1:], ";&|{(") {
				b.WriteString(" ")
			} else {
				b.WriteString("; ")
			}
		}
		b.WriteString(line)
	}
	return b.String()
}

// sanitizeLine strips control characters that would corrupt the display. Tool
// output is untrusted: a test that prints an escape sequence must not be able
// to move the cursor, repaint the live region, or hide what it did.
//
// Invalid encodings go too. A lone 0x9b byte is not valid UTF-8, but a terminal
// in eight-bit mode reads it as CSI — an escape sequence that never had to
// contain an escape character.
func sanitizeLine(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, isControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case isControl(r):
			// Drop it. Replacing with a glyph would let output pad itself out
			// to hide following text.
		case r == utf8.RuneError:
			// Either an invalid byte or a literal replacement character; the
			// first is dangerous and the second carries no information.
			_ = i
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// sanitizeBlock sanitizes multi-line text, keeping newlines.
func sanitizeBlock(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = sanitizeLine(l)
	}
	return strings.Join(lines, "\n")
}

// padTo pads a rendered (possibly styled) string to width with spaces.
func padTo(s string, width int) string {
	w := reflowansi.PrintableRuneWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// visibleWidth is the display width of a rendered string, ignoring escapes.
func visibleWidth(s string) int { return reflowansi.PrintableRuneWidth(s) }

// tailLines returns the last n lines.
func tailLines(lines []string, n int) []string {
	if n <= 0 || len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// elideLeading shortens a path from the front, keeping whole trailing segments
// and marking what it dropped: /a/b/c/d becomes …/c/d.
//
// The front is what a path can afford to lose. The last segment or two say
// which project this is; everything before them is where the developer keeps
// their work, which they already know. A path that will not fit even one
// segment gives up entirely rather than becoming an ellipsis and three
// characters that could be any directory on the machine.
func elideLeading(p string, width int, ellipsis string) string {
	p = sanitizeLine(p)
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(p) <= width {
		return p
	}
	// Cut at separators, from the last one backwards, and take the longest
	// tail that fits.
	for i := 0; i < len(p); i++ {
		if p[i] != '/' && p[i] != '\\' {
			continue
		}
		if candidate := ellipsis + p[i:]; runewidth.StringWidth(candidate) <= width {
			return candidate
		}
	}
	// Not even the last segment fits beside the ellipsis. Half a segment names
	// nothing, so say nothing.
	return ""
}
