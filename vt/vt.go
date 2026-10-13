// Package vt is a small terminal emulator, enough of one to test a terminal UI
// against.
//
// It exists for one guarantee in particular: that resizing the window does not
// leave copies of the input box stranded on screen. That failure is a
// disagreement between what the program believes it drew and what the terminal
// did with it, so testing it needs a terminal that behaves like one — autowrap
// with wrap flags, and re-flow on resize. Nothing here is faithful beyond what
// that requires: colour is discarded, and the grid grows without bound instead
// of scrolling.
//
// A terminal made with a height also has a window: the last rows of the grid,
// which is what an absolute cursor position counts from. Programs that only
// move relative to where they are never notice, but Windows' pseudo-console
// redraws everything with absolute positions, and against a grid with no
// window it draws over the wrong rows.
package vt

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	csiRe = regexp.MustCompile(`^\x1b\[([0-9;?]*)([@-~])`)
	oscRe = regexp.MustCompile(`^\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
)

// row is one physical line of the screen.
type row struct {
	cells []rune
	// wrapped marks a row the terminal broke itself, rather than one the
	// program ended with a newline. Only wrapped rows are rejoined on resize —
	// which is exactly why a program that fills the last column gets re-flowed
	// and one that stops a column short does not.
	wrapped bool
}

// Terminal is an emulated screen.
type Terminal struct {
	width  int
	rows   []row
	cy, cx int
	bells  int
	// height is the window's, and top the grid row it starts at. A height
	// of zero is no window at all: positions count from the first row.
	height, top int
	// pending is a partial escape sequence held back between writes.
	pending []byte
}

// New returns an emulator of the given width, with no window.
func New(width int) *Terminal {
	return NewSize(width, 0)
}

// NewSize returns an emulator with a window of the given size.
func NewSize(width, height int) *Terminal {
	if width < 1 {
		width = 1
	}
	return &Terminal{width: width, height: max(height, 0), rows: []row{{}}}
}

// follow scrolls the window down to the cursor, the way a terminal scrolls
// when output runs off its last row.
func (t *Terminal) follow() {
	if t.height > 0 && t.cy >= t.top+t.height {
		t.top = t.cy - t.height + 1
	}
}

// Bells is how many times the program rang the terminal bell.
func (t *Terminal) Bells() int { return t.bells }

// Lines returns every line drawn so far, trailing blanks trimmed.
func (t *Terminal) Lines() []string {
	out := make([]string, 0, len(t.rows))
	for _, r := range t.rows {
		out = append(out, strings.TrimRight(string(r.cells), " "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// String is every line joined, which is what most assertions want.
func (t *Terminal) String() string { return strings.Join(t.Lines(), "\n") }

// Count is how many lines contain s.
func (t *Terminal) Count(s string) int {
	n := 0
	for _, l := range t.Lines() {
		if strings.Contains(l, s) {
			n++
		}
	}
	return n
}

// Write feeds terminal output in. Partial escape sequences at the end of p are
// held until the next call.
func (t *Terminal) Write(p []byte) (int, error) {
	n := len(p)
	data := append(t.pending, p...)
	t.pending = nil

	for len(data) > 0 {
		switch {
		case data[0] == 0x1b:
			if m := csiRe.FindSubmatch(data); m != nil {
				t.csi(string(m[1]), rune(m[2][0]))
				data = data[len(m[0]):]
				continue
			}
			if m := oscRe.Find(data); m != nil {
				data = data[len(m):]
				continue
			}
			// An escape sequence split across writes, or one we do not know.
			if len(data) < 24 {
				t.pending = append(t.pending[:0], data...)
				return n, nil
			}
			data = data[1:]

		case data[0] == '\r':
			t.cx = 0
			data = data[1:]

		case data[0] == '\n':
			t.cy++
			t.grow(t.cy)
			t.follow()
			data = data[1:]

		case data[0] == '\b':
			if t.cx > 0 {
				t.cx--
			}
			data = data[1:]

		case data[0] == 0x07:
			t.bells++
			data = data[1:]

		default:
			r, size := decodeRune(data)
			if size == 0 {
				// An incomplete multi-byte rune at the end of the buffer.
				t.pending = append(t.pending[:0], data...)
				return n, nil
			}
			t.put(r)
			data = data[size:]
		}
	}
	return n, nil
}

func decodeRune(p []byte) (rune, int) {
	need := 1
	switch {
	case p[0] < 0x80:
		return rune(p[0]), 1
	case p[0]&0xE0 == 0xC0:
		need = 2
	case p[0]&0xF0 == 0xE0:
		need = 3
	case p[0]&0xF8 == 0xF0:
		need = 4
	default:
		return '?', 1
	}
	if len(p) < need {
		return 0, 0
	}
	return []rune(string(p[:need]))[0], need
}

func (t *Terminal) put(r rune) {
	if t.cx >= t.width {
		// Autowrap: the row the terminal broke is marked, so a later resize
		// knows it may rejoin it.
		t.rows[t.cy].wrapped = true
		t.cy++
		t.cx = 0
		t.grow(t.cy)
		t.follow()
	}
	t.grow(t.cy)
	line := t.rows[t.cy].cells
	for len(line) <= t.cx {
		line = append(line, ' ')
	}
	line[t.cx] = r
	t.rows[t.cy].cells = line
	t.cx++
}

func (t *Terminal) grow(y int) {
	for len(t.rows) <= y {
		t.rows = append(t.rows, row{})
	}
}

func (t *Terminal) csi(params string, final rune) {
	if strings.HasPrefix(params, "?") {
		return // private modes: cursor visibility, bracketed paste, focus
	}
	args := []int{}
	for _, p := range strings.Split(params, ";") {
		if p == "" {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			args = append(args, n)
		}
	}
	arg := func(i, def int) int {
		if i < len(args) && args[i] > 0 {
			return args[i]
		}
		return def
	}

	switch final {
	case 'A':
		t.cy = max(t.top, t.cy-arg(0, 1))
	case 'B':
		t.cy += arg(0, 1)
		if t.height > 0 {
			t.cy = min(t.cy, t.top+t.height-1)
		}
		t.grow(t.cy)
	case 'C':
		t.cx += arg(0, 1)
	case 'D':
		t.cx = max(0, t.cx-arg(0, 1))
	case 'G':
		t.cx = max(0, arg(0, 1)-1)
	case 'H', 'f':
		row := arg(0, 1) - 1
		if t.height > 0 {
			row = min(row, t.height-1)
		}
		t.cy = t.top + row
		t.cx = max(0, arg(1, 1)-1)
		t.grow(t.cy)
	case 'X':
		// Erase characters: blanks from the cursor on, which stays put.
		t.grow(t.cy)
		line := t.rows[t.cy].cells
		for i := t.cx; i < min(t.cx+arg(0, 1), len(line)); i++ {
			line[i] = ' '
		}
	case 'K':
		t.grow(t.cy)
		line := t.rows[t.cy].cells
		switch arg(0, 0) {
		case 0:
			if t.cx < len(line) {
				t.rows[t.cy].cells = line[:t.cx]
			}
			t.rows[t.cy].wrapped = false
		case 1:
			for i := 0; i < min(t.cx, len(line)); i++ {
				line[i] = ' '
			}
		default:
			t.rows[t.cy].cells = nil
			t.rows[t.cy].wrapped = false
		}
	case 'J':
		switch arg(0, 0) {
		case 0:
			t.grow(t.cy)
			if t.cx < len(t.rows[t.cy].cells) {
				t.rows[t.cy].cells = t.rows[t.cy].cells[:t.cx]
			}
			t.rows = t.rows[:t.cy+1]
		case 2:
			if t.height > 0 {
				// The window is cleared and the scrollback above it kept;
				// the cursor does not move.
				t.rows = t.rows[:min(t.top, len(t.rows))]
				t.grow(t.cy)
				break
			}
			t.rows = []row{{}}
			t.cy, t.cx = 0, 0
		}
	case 'L':
		for i := 0; i < arg(0, 1); i++ {
			t.rows = append(t.rows[:t.cy], append([]row{{}}, t.rows[t.cy:]...)...)
		}
	case 'M':
		for i := 0; i < arg(0, 1) && t.cy < len(t.rows); i++ {
			t.rows = append(t.rows[:t.cy], t.rows[t.cy+1:]...)
		}
	}
}

// Resize re-flows the screen the way a terminal does: rows the terminal wrapped
// itself are rejoined into logical lines and laid out again at the new width.
// Rows the program ended deliberately are never rejoined — but a row longer
// than the new width is still broken up, which is what strands output when a
// program draws into the last column.
func (t *Terminal) Resize(width int) {
	if width < 1 || width == t.width {
		return
	}

	// Cursor position, in logical coordinates, so it can be restored after the
	// re-flow.
	logical, offset := 0, 0
	var logicals []string
	cur := strings.Builder{}
	for i, r := range t.rows {
		if i == t.cy {
			logical, offset = len(logicals), cur.Len()+t.cx
		}
		cur.WriteString(strings.TrimRight(string(r.cells), " "))
		if !r.wrapped {
			logicals = append(logicals, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		logicals = append(logicals, cur.String())
	}

	t.width = width
	t.rows = nil
	t.cy, t.cx = 0, 0
	for i, l := range logicals {
		start := len(t.rows)
		for _, chunk := range chunks(l, width) {
			t.rows = append(t.rows, row{cells: []rune(chunk), wrapped: true})
		}
		if len(t.rows) == start {
			t.rows = append(t.rows, row{})
		}
		t.rows[len(t.rows)-1].wrapped = false

		if i == logical {
			t.cy = start + offset/width
			t.cx = offset % width
		}
	}
	if len(t.rows) == 0 {
		t.rows = []row{{}}
	}
	t.grow(t.cy)
	if t.height > 0 {
		// The window keeps to the bottom of what was re-flowed, and keeps
		// the cursor in it.
		t.top = min(max(0, len(t.rows)-t.height), t.cy)
		t.follow()
	}
}

func chunks(s string, width int) []string {
	r := []rune(s)
	var out []string
	for len(r) > width {
		out = append(out, string(r[:width]))
		r = r[width:]
	}
	if len(r) > 0 || len(out) == 0 {
		out = append(out, string(r))
	}
	return out
}
