package terminalui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Box drawing for modal views. Every helper returns a line exactly width
// columns wide, so a box never disagrees with itself and never spills into the
// terminal's last column (see uiModel.frameWidth).
//
// Layout of a box of width w:
//
//	╭─ title ──────────── right ─╮
//	│ content                    │   inner width = w - 4
//	├────────────────────────────┤
//	│ buttons                    │
//	╰────────────────────────────╯

// boxInnerWidth is the room available for content inside a box.
func boxInnerWidth(width int) int {
	if width < 8 {
		return 1
	}
	return width - 4
}

// boxTop draws the top border with an optional title on the left and an
// optional marker (a scroll position, say) on the right.
func (t *Theme) boxTop(title, right string, width int) string {
	return t.boxEdge(t.Symbols.BoxTL, t.Symbols.BoxTR, title, right, width)
}

// boxBottom draws the bottom border.
func (t *Theme) boxBottom(title, right string, width int) string {
	return t.boxEdge(t.Symbols.BoxBL, t.Symbols.BoxBR, title, right, width)
}

// boxSep draws a divider across the box.
func (t *Theme) boxSep(width int) string {
	return t.boxEdge(t.Symbols.BoxLT, t.Symbols.BoxRT, "", "", width)
}

func (t *Theme) boxEdge(left, right, title, marker string, width int) string {
	h := t.Symbols.BoxH
	if width < 8 {
		return t.Border.Render(strings.Repeat(h, max(width, 1)))
	}

	var head, tail string
	used := 2 // the two corners
	if title != "" {
		title = ansi.Truncate(title, width-8, t.Symbols.Ellipsis)
		head = h + " " + title + " "
		used += visibleWidth(head)
	} else {
		head = h
		used++
	}
	if marker != "" && used+visibleWidth(marker)+4 <= width {
		tail = " " + marker + " " + h
		used += visibleWidth(tail)
	} else {
		tail = h
		used++
	}

	fill := width - used
	if fill < 0 {
		fill = 0
	}
	return t.Border.Render(left+head) + t.Border.Render(strings.Repeat(h, fill)) +
		t.Border.Render(tail+right)
}

// boxRow wraps one line of already-styled content in the box's side borders,
// padding or truncating it to the inner width.
func (t *Theme) boxRow(content string, width int) string {
	inner := boxInnerWidth(width)
	if visibleWidth(content) > inner {
		content = ansi.Truncate(content, inner, t.Symbols.Ellipsis)
	}
	v := t.Border.Render(t.Symbols.BoxV)
	return v + " " + padTo(content, inner) + " " + v
}

// progressBar draws a bar of the given width, filled to percent.
func (t *Theme) progressBar(percent, width int) string {
	if width < 4 {
		width = 4
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	full := percent * width / 100
	return t.BarFull.Render(strings.Repeat(t.Symbols.BarFull, full)) +
		t.BarEmpty.Render(strings.Repeat(t.Symbols.BarEmpty, width-full))
}

// boxHeader is the content of a box's title row: what is being asked, what it
// is about, and an optional marker on the right.
type boxHeader struct {
	Label  string // "APPROVAL REQUIRED"
	Detail string // "POST https://telemetry.vendor.io/v1/events"
	Right  string // "4–11 of 12"
}

// boxTitle draws the title row: the label, the detail, then a rule in the
// border's own colour out to the marker on the right.
func (t *Theme) boxTitle(h boxHeader, width int) string {
	inner := boxInnerWidth(width)
	v := t.Border.Render(t.Symbols.BoxV)

	// The detail is usually a URL the sandbox asked for, which is to say a
	// string somebody else may have chosen.
	left := t.TitleLabel.Render(sanitizeLine(h.Label))
	if h.Detail != "" {
		left += "  " + t.TitleDetail.Render(sanitizeLine(h.Detail))
	}
	// With nothing on the right, the rule runs all the way to the border
	// rather than stopping a space short of it.
	right, tail := "", ""
	if h.Right != "" {
		right = " " + t.TitleDim.Render(h.Right)
		tail = " "
	}

	// The detail is what gives when there is not enough room; the label is the
	// part that says what is being asked.
	if room := inner - visibleWidth(right) - 2; visibleWidth(left) > room {
		left = ansi.Truncate(left, max(room, 1), t.Symbols.Ellipsis)
	}
	fill := inner - visibleWidth(left) - visibleWidth(right) - 1 - visibleWidth(tail)
	if fill < 0 {
		fill = 0
	}
	rule := t.Border.Render(" " + strings.Repeat(t.Symbols.BoxH, fill))
	return v + " " + left + rule + right + tail + " " + v
}

// boxRowBar is boxRow with one column of scrollbar against the right border.
func (t *Theme) boxRowBar(content, bar string, width int) string {
	inner := boxInnerWidth(width) - 1
	if inner < 1 {
		return t.boxRow(content, width)
	}
	if visibleWidth(content) > inner {
		content = ansi.Truncate(content, inner, t.Symbols.Ellipsis)
	}
	v := t.Border.Render(t.Symbols.BoxV)
	return v + " " + padTo(content, inner) + " " + bar + v
}

// scrollbar returns one glyph per visible row, or nil when everything fits.
func (t *Theme) scrollbar(offset, height, total int) []string {
	if total <= height || height <= 0 {
		return nil
	}
	thumb := max(height*height/total, 1)
	start := offset * height / total
	if start+thumb > height {
		start = height - thumb
	}
	out := make([]string, height)
	for i := range out {
		if i >= start && i < start+thumb {
			out[i] = t.ScrollThumb.Render(t.Symbols.Thumb)
		} else {
			out[i] = t.ScrollTrack.Render(t.Symbols.Track)
		}
	}
	return out
}
