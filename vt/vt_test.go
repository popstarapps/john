package vt

import (
	"strings"
	"testing"
)

// The emulator is what certifies the UI's central guarantee — that resizing
// does not strand copies of the input box — so it needs certifying itself. A
// terminal that does not re-flow the way a terminal does would prove the
// guarantee against a fiction.

func write(t *testing.T, term *Terminal, chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		if _, err := term.Write([]byte(c)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func TestPrintAndNewline(t *testing.T) {
	term := New(20)
	write(t, term, "hello\r\nworld")
	if got, want := term.String(), "hello\nworld"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCarriageReturnOverwrites(t *testing.T) {
	term := New(20)
	write(t, term, "hello\rbye")
	if got, want := term.String(), "byelo"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBackspaceAndBell(t *testing.T) {
	term := New(20)
	write(t, term, "ab\bc\a\a")
	if got, want := term.String(), "ac"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if term.Bells() != 2 {
		t.Fatalf("want 2 bells, got %d", term.Bells())
	}
}

// Writing past the last column wraps, and the row is marked as one the terminal
// broke — which is the flag re-flow depends on.
func TestAutowrapMarksTheRow(t *testing.T) {
	term := New(5)
	write(t, term, "abcdefgh")
	lines := term.Lines()
	if len(lines) != 2 || lines[0] != "abcde" || lines[1] != "fgh" {
		t.Fatalf("bad wrap: %q", lines)
	}
	if !term.rows[0].wrapped {
		t.Fatal("the wrapped row should be marked")
	}
	if term.rows[1].wrapped {
		t.Fatal("the last row was not wrapped by the terminal")
	}
}

func TestExplicitNewlineIsNotAWrap(t *testing.T) {
	term := New(5)
	write(t, term, "abc\r\nde")
	if term.rows[0].wrapped {
		t.Fatal("a row the program ended is not a wrapped row")
	}
}

func TestCursorMovementAndErase(t *testing.T) {
	term := New(20)
	write(t, term, "line one\r\nline two\r\nline three")

	// Up two, to the start, and erase to the end of the line.
	write(t, term, "\x1b[2A\r\x1b[K")
	lines := term.Lines()
	if lines[0] != "" || lines[1] != "line two" {
		t.Fatalf("erase-to-end went wrong: %q", lines)
	}

	// Erase the whole display below the cursor.
	write(t, term, "\x1b[2B\r\x1b[J")
	if got := len(term.Lines()); got > 2 {
		t.Fatalf("erase-below left %d lines: %q", got, term.Lines())
	}
}

func TestAbsolutePositioning(t *testing.T) {
	term := New(20)
	write(t, term, "aaaa\r\nbbbb\r\ncccc")
	write(t, term, "\x1b[2;2Hx")
	if got := term.Lines()[1]; got != "bxbb" {
		t.Fatalf("cursor addressing went wrong: %q", got)
	}
}

func TestPrivateModesAndOSCAreIgnored(t *testing.T) {
	term := New(20)
	write(t, term, "\x1b[?25l\x1b]0;a title\x07visible\x1b[?25h")
	if got, want := term.String(), "visible"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// An OSC terminated by BEL must not be counted as a bell.
	if term.Bells() != 0 {
		t.Fatalf("an OSC terminator is not a bell, got %d", term.Bells())
	}
}

func TestSGRIsDiscarded(t *testing.T) {
	term := New(20)
	write(t, term, "\x1b[1;38;2;1;2;3mbold\x1b[0m plain")
	if got, want := term.String(), "bold plain"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Output arrives in whatever chunks the kernel feels like. An escape sequence
// or a rune split across two reads must not corrupt the screen.
func TestSplitSequencesAcrossWrites(t *testing.T) {
	term := New(20)
	write(t, term, "ab\x1b", "[2Dx")
	if got, want := term.String(), "xb"; got != want {
		t.Fatalf("split CSI: got %q, want %q", got, want)
	}

	term = New(20)
	utf8 := []byte("é")
	write(t, term, "a"+string(utf8[:1]), string(utf8[1:])+"b")
	if got, want := term.String(), "aéb"; got != want {
		t.Fatalf("split rune: got %q, want %q", got, want)
	}
}

// The property the whole design rests on: a row the terminal wrapped is
// rejoined and laid out again at the new width, and a row the program ended is
// not.
func TestResizeRejoinsOnlyWrappedRows(t *testing.T) {
	term := New(10)
	write(t, term, "abcdefghijklmno") // wraps: "abcdefghij" + "klmno"
	write(t, term, "\r\nshort")

	term.Resize(20)
	lines := term.Lines()
	if len(lines) != 2 || lines[0] != "abcdefghijklmno" || lines[1] != "short" {
		t.Fatalf("widening should rejoin the wrapped row only: %q", lines)
	}

	term.Resize(5)
	lines = term.Lines()
	want := []string{"abcde", "fghij", "klmno", "short"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("narrowing should re-flow: %q", lines)
	}
}

// A line that stops short of the last column survives a shrink intact; one that
// fills it does not. This is exactly the rule the UI draws by, which is why the
// UI can promise no debris.
func TestResizeSplitsLinesLongerThanTheNewWidth(t *testing.T) {
	term := New(20)
	write(t, term, "0123456789\r\n0123456789012345678")

	term.Resize(12)
	lines := term.Lines()
	if lines[0] != "0123456789" {
		t.Fatalf("a line inside the new width should be untouched: %q", lines[0])
	}
	if len(lines) != 3 {
		t.Fatalf("a line longer than the new width should break: %q", lines)
	}
}

func TestResizeKeepsTheCursorOnItsCharacter(t *testing.T) {
	term := New(20)
	write(t, term, "hello\r\nworld")
	term.Resize(4)
	// The cursor was after "world"; whatever row that lands on, the next thing
	// written must follow it.
	write(t, term, "!")
	if !strings.Contains(term.String(), "world!") && !strings.Contains(term.String(), "d!") {
		t.Fatalf("the cursor did not follow its character: %q", term.Lines())
	}
}

func TestResizeToTheSameWidthIsANoop(t *testing.T) {
	term := New(10)
	write(t, term, "abcdefghijkl")
	before := term.String()
	term.Resize(10)
	if term.String() != before {
		t.Fatalf("no-op resize changed the screen: %q -> %q", before, term.String())
	}
}

func TestCountAndLinesTrimTrailingBlanks(t *testing.T) {
	term := New(20)
	write(t, term, "one\r\ntwo\r\n\r\n\r\n")
	if got := len(term.Lines()); got != 2 {
		t.Fatalf("trailing blank rows should be trimmed, got %d: %q", got, term.Lines())
	}
	write(t, term, "one more one")
	if got := term.Count("one"); got != 2 {
		t.Fatalf("Count is per line, got %d", got)
	}
}

func TestInsertAndDeleteLines(t *testing.T) {
	term := New(20)
	write(t, term, "a\r\nb\r\nc")
	write(t, term, "\x1b[2A\x1b[L") // insert a blank line at the top
	if got := term.Lines(); len(got) != 4 || got[0] != "" {
		t.Fatalf("insert-line: %q", got)
	}
	write(t, term, "\x1b[M\x1b[M") // delete two
	if got := term.Lines(); len(got) != 2 {
		t.Fatalf("delete-line: %q", got)
	}
}

func TestZeroAndNegativeWidths(t *testing.T) {
	term := New(0)
	write(t, term, "ab")
	if got := term.String(); got == "" {
		t.Fatal("a one-column terminal still holds text")
	}
	term.Resize(0) // must not panic or wipe the screen
	if term.String() == "" {
		t.Fatal("a bad resize should be ignored")
	}
}

// Windows' pseudo-console draws with absolute positions, which count from the
// top of the window, not the top of everything printed. With a window, a
// position after the output has scrolled lands where the program meant it.
func TestAbsolutePositionsCountFromTheWindow(t *testing.T) {
	term := NewSize(20, 3)
	write(t, term, "one\r\ntwo\r\nthree\r\nfour")
	// The window is now "two", "three", "four"; row 1 of it is "two".
	write(t, term, "\x1b[1;1HTWO")
	if got, want := term.String(), "one\nTWO\nthree\nfour"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Without a window, the same position is the first row ever printed.
	flat := New(20)
	write(t, flat, "one\r\ntwo\r\nthree\r\nfour", "\x1b[1;1HONE")
	if got, want := flat.String(), "ONE\ntwo\nthree\nfour"; got != want {
		t.Fatalf("without a window: got %q, want %q", got, want)
	}
}

// Erase characters blanks cells from the cursor and leaves it where it was:
// how the pseudo-console takes a box off the screen.
func TestEraseCharacters(t *testing.T) {
	term := NewSize(20, 5)
	write(t, term, "hello world", "\x1b[1;3H\x1b[3X", "!")
	if got, want := term.String(), "he!   world"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
