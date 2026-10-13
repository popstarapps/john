package terminalui

import (
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Everything the UI draws passes through these. A wrap that overshoots by one
// column is the difference between a frame that survives a resize and one that
// strands a copy of itself; a truncation that counts a CJK glyph as one column
// is the same bug with a different alphabet.

func TestWrapPlain(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		width int
		want  []string
	}{
		{"short", "hello", 20, []string{"hello"}},
		{"exact", "hello", 5, []string{"hello"}},
		{"words", "the quick brown fox", 9, []string{"the quick", "brown fox"}},
		{"newlines kept", "one\ntwo", 20, []string{"one", "two"}},
		{"blank line kept", "one\n\ntwo", 20, []string{"one", "", "two"}},
		{"tabs expand", "a\tb", 20, []string{"a    b"}},
		{"empty", "", 20, []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrapPlain(c.in, c.width)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A word longer than the line still has to fit on the screen.
func TestWrapPlainBreaksLongWords(t *testing.T) {
	for _, width := range []int{1, 3, 8, 40} {
		for _, l := range wrapPlain(strings.Repeat("x", 200), width) {
			if visibleWidth(l) > width {
				t.Fatalf("@%d a line is %d columns: %q", width, visibleWidth(l), l)
			}
		}
	}
}

func TestWrapPlainHandlesSillyWidths(t *testing.T) {
	for _, width := range []int{0, -1, -100} {
		got := wrapPlain("hello world", width)
		if len(got) == 0 {
			t.Fatalf("@%d: wrapping should still produce something", width)
		}
		for _, l := range got {
			if visibleWidth(l) > 1 {
				t.Fatalf("@%d: %q", width, l)
			}
		}
	}
}

func TestWrapPlainTrimsTrailingSpace(t *testing.T) {
	for _, l := range wrapPlain("one two  three   four", 9) {
		if strings.HasSuffix(l, " ") {
			t.Fatalf("wrapped lines should not end in spaces: %q", l)
		}
	}
}

func TestTruncPlain(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		width      int
		tail, want string
	}{
		{"fits", "hello", 10, "…", "hello"},
		{"exact", "hello", 5, "…", "hello"},
		{"cuts", "hello world", 8, "…", "hello w…"},
		{"tail too wide", "hello", 2, "...", "he"},
		{"zero width", "hello", 0, "…", ""},
		{"negative width", "hello", -3, "…", ""},
		{"tabs first", "a\tb", 20, "…", "a    b"},
		{"lines joined", "cd / && serve &\nsleep 2\r\ncurl -s x", 60, "…", "cd / && serve & sleep 2; curl -s x"},
		{"blank lines dropped", "a\n\n  b\n", 20, "…", "a; b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncPlain(c.in, c.width, c.tail)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			if c.width > 0 && visibleWidth(got) > c.width {
				t.Fatalf("%q is %d columns, budget was %d", got, visibleWidth(got), c.width)
			}
		})
	}
}

// Two columns per glyph, and the budget is columns.
func TestWideRunesUseTheirRealWidth(t *testing.T) {
	const cjk = "変更されたファイル" // 9 glyphs, 18 columns
	if got := visibleWidth(cjk); got != 18 {
		t.Fatalf("CJK width: got %d, want 18", got)
	}
	for _, width := range []int{4, 7, 10, 17, 18, 40} {
		got := truncPlain(cjk, width, "…")
		if visibleWidth(got) > width {
			t.Fatalf("@%d truncation overflowed: %q is %d columns", width, got, visibleWidth(got))
		}
	}
	for _, width := range []int{4, 9, 20} {
		for _, l := range wrapPlain(cjk+" "+cjk, width) {
			if visibleWidth(l) > width {
				t.Fatalf("@%d wrap overflowed: %q is %d columns", width, l, visibleWidth(l))
			}
		}
	}
}

func TestEmojiDoesNotOverflow(t *testing.T) {
	const line = "shipped 🚀 and tested ✅ and done 🎉"
	for _, width := range []int{6, 12, 20, 33} {
		if got := truncPlain(line, width, "…"); visibleWidth(got) > width {
			t.Fatalf("@%d: %q is %d columns", width, got, visibleWidth(got))
		}
		for _, l := range wrapPlain(line, width) {
			if visibleWidth(l) > width {
				t.Fatalf("@%d wrap: %q is %d columns", width, l, visibleWidth(l))
			}
		}
	}
}

// Tool output, headers and bodies are untrusted. Nothing that could move the
// cursor may survive.
func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"plain":                  "plain",
		"clear\x1b[2Jscreen":     "clear[2Jscreen",
		"bell\a":                 "bell",
		"null\x00byte":           "nullbyte",
		"del\x7fete":             "delete",
		"c1\u009bcontrol":        "c1control", // the C1 CSI rune
		"c1\x9bcontrol":          "c1control", // the same thing as a raw byte
		"invalid \xff byte":      "invalid  byte",
		"tab\there":              "tab    here",
		"carriage\rreturn":       "carriagereturn",
		"vertical\vtab":          "verticaltab",
		"backspace\bhiding":      "backspacehiding",
		"combining é is fine":    "combining é is fine",
		"変更 is fine":             "変更 is fine",
		"\x1b]0;title\x07prompt": "]0;titleprompt",
	}
	for in, want := range cases {
		if got := sanitizeLine(in); got != want {
			t.Fatalf("sanitizeLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeBlockKeepsNewlines(t *testing.T) {
	got := sanitizeBlock("one\r\ntwo\nthree\x1b[Afour")
	if got != "one\ntwo\nthree[Afour" {
		t.Fatalf("got %q", got)
	}
}

func TestPadToAndVisibleWidth(t *testing.T) {
	if got := padTo("abc", 6); got != "abc   " {
		t.Fatalf("padTo: %q", got)
	}
	if got := padTo("abcdef", 3); got != "abcdef" {
		t.Fatalf("padTo must never shorten: %q", got)
	}
	// Styling is not width.
	styled := "\x1b[31mabc\x1b[0m"
	if got := visibleWidth(styled); got != 3 {
		t.Fatalf("visibleWidth of a styled string: got %d, want 3", got)
	}
	if got := padTo(styled, 5); visibleWidth(got) != 5 {
		t.Fatalf("padding a styled string: %d columns", visibleWidth(got))
	}
}

func TestTailLines(t *testing.T) {
	in := []string{"a", "b", "c", "d"}
	if got := tailLines(in, 2); strings.Join(got, "") != "cd" {
		t.Fatalf("got %q", got)
	}
	if got := tailLines(in, 10); len(got) != 4 {
		t.Fatalf("asking for more than there is returns all: %q", got)
	}
	if got := tailLines(in, 0); len(got) != 4 {
		t.Fatalf("a zero cap is no cap: %q", got)
	}
}

func TestItoa(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 42: "42", 1000: "1000", -5: "-5"}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Fatalf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// A long working directory must not push the rest of the status line off the
// screen. The end of a path is what identifies a project; the beginning is
// where the developer keeps their work, and they already know it.
func TestALongPathGivesUpItsHeadAndNotItsTail(t *testing.T) {
	const p = "/var/folders/0r/rfsc0k654jj0w5g640335m180000gn/T/TestSession123/004"
	for _, c := range []struct {
		width int
		want  string
	}{
		{80, p},
		{30, "…/T/TestSession123/004"},
		{20, "…/TestSession123/004"},
		{19, "…/004"},
		{5, "…/004"},
		{4, ""},
	} {
		if got := elideLeading(p, c.width, "…"); got != c.want {
			t.Errorf("elideLeading(width %d) = %q, want %q", c.width, got, c.want)
		}
	}
}

// Hyphens are not where lines break, and a line with them in it is no wider
// than any other: the word wrapper used to leave each one out of its count,
// and the hard wrap after it split words — "a" on one line, "nd" on the next.
func TestHyphensDoNotSplitWords(t *testing.T) {
	text := "Install Node.js and npm in the sandbox: run `apt-get update` and then `apt-get install -y nodejs npm`. " +
		"If a download is blocked by network policy, stop and tell me which host. When it's installed, run " +
		"`node --version` and `npm --version`."
	for width := 30; width <= 118; width++ {
		lines := wrapPlain(text, width)
		for _, l := range lines {
			if visibleWidth(l) > width {
				t.Fatalf("width %d: %q is %d columns", width, l, visibleWidth(l))
			}
		}
		words := strings.Fields(text)
		got := strings.Fields(strings.Join(lines, " "))
		if strings.Join(got, " ") != strings.Join(words, " ") {
			t.Fatalf("width %d split a word:\n%s", width, strings.Join(lines, "\n"))
		}
	}
}

// Inline code is coloured, not padded: glamour put a space on each side of it,
// which copied out of scrollback — or seen without the background colour —
// is "run  npm install  and" and a space before the full stop.
func TestInlineCodeIsNotPaddedWithSpaces(t *testing.T) {
	for _, dark := range []bool{true, false} {
		r := lipgloss.NewRenderer(io.Discard)
		r.SetColorProfile(termenv.TrueColor)
		r.SetHasDarkBackground(dark)
		lines := newMarkdownRenderer(true, false, r).render("Then run `npm install` and `npm run dev`.", 80)
		got := strings.TrimSpace(stripANSI(strings.Join(lines, "\n")))
		if want := "Then run npm install and npm run dev."; got != want {
			t.Errorf("dark=%v: %q, want %q", dark, got, want)
		}
	}
}

// A model's one-item-per-line list stays one item per line. Markdown joins
// single newlines into a paragraph, so these ran together, and the over-long
// line that made was wrapped mid-name: "src/App." on one line, "jsx" on the
// next. The text is from an end-to-end run.
func TestOneItemPerLineStaysOneItemPerLine(t *testing.T) {
	text := "## Summary of what was created:\n\n" +
		"✅ **package.json** - Project configuration with React, React-DOM, Vite, and build script\n" +
		"✅ **vite.config.js** - Vite configuration with React plugin\n" +
		"✅ **index.html** - HTML entry point\n" +
		"✅ **src/main.jsx** - React app entry point\n" +
		"✅ **src/App.jsx** - Complete Tic Tac Toe game component with:\n" +
		"   - Game board state management\n" +
		"   - Reset button\n" +
		"   - Status display\n" +
		"✅ **src/index.css** - Beautiful styling with gradient background and smooth interactions\n" +
		"✅ **dist/** - Production build ready to deploy\n\n" +
		"The game is complete and fully functional!"
	for _, width := range []int{80, 116} {
		r := lipgloss.NewRenderer(io.Discard)
		r.SetColorProfile(termenv.TrueColor)
		r.SetHasDarkBackground(true)
		lines := newMarkdownRenderer(true, false, r).render(text, width)
		plain := stripANSI(strings.Join(lines, "\n"))
		starts := 0
		for _, l := range strings.Split(plain, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "✅") {
				starts++
			}
			if strings.HasSuffix(strings.TrimSpace(l), "src/App.") {
				t.Fatalf("width %d split a file name:\n%s", width, plain)
			}
		}
		if starts != 7 {
			t.Fatalf("width %d: %d lines start with ✅, want 7:\n%s", width, starts, plain)
		}
		if !strings.Contains(plain, "Status display") || strings.Contains(plain, "Status display ✅") {
			t.Fatalf("width %d: the list's last item took the next line:\n%s", width, plain)
		}
	}
	// Code keeps its own lines and gets nothing added to them.
	code := "```\na\nb\n```"
	if got := chatLineBreaks(code); got != code {
		t.Fatalf("fenced code changed: %q", got)
	}
}

// A list nested in another is followed by one blank line, as a flat one is;
// it was two, glamour ending each list with its own. Code keeps the blank
// lines it has.
func TestANestedListEndsWithOneBlankLine(t *testing.T) {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	md := newMarkdownRenderer(true, false, r)
	blankBefore := func(text, line string) int {
		lines := strings.Split(stripANSI(strings.Join(md.render(text, 80), "\n")), "\n")
		for i, l := range lines {
			if strings.TrimSpace(l) == line {
				n := 0
				for j := i - 1; j >= 0 && strings.TrimSpace(lines[j]) == ""; j-- {
					n++
				}
				return n
			}
		}
		t.Fatalf("%q not rendered:\n%s", line, strings.Join(lines, "\n"))
		return 0
	}
	if n := blankBefore("1. **a** - x:\n   - y\n   - z\n\nThe end.", "The end."); n != 1 {
		t.Fatalf("%d blank lines after a nested list, want 1", n)
	}
	if n := blankBefore("```python\ndef a():\n    pass\n\n\ndef b():\n    pass\n```", "def b():"); n != 2 {
		t.Fatalf("%d blank lines between the functions, want the code's 2", n)
	}
	// Both in one message: code anywhere in it used to leave every nested
	// list in it two lines from what followed.
	both := "```\ndist/\n\n\n  index.html\n```\n\n- assets:\n  - index.js\n  - index.css\n\nThe hash is for caching."
	if n := blankBefore(both, "The hash is for caching."); n != 1 {
		t.Fatalf("%d blank lines after a nested list in a message with code, want 1", n)
	}
	if n := blankBefore(both, "index.html"); n != 2 {
		t.Fatalf("%d blank lines in the code, want its 2", n)
	}
	if got := strings.Join(md.render(both, 80), ""); strings.Contains(got, codeBlank) {
		t.Fatal("the code's blank-line mark was left in the output")
	}
}

// A paragraph with inline code fits the width it is drawn at. Glamour ran the
// lines after a code span a column or two long, and the frame broke them
// again: "…React-DOM, Vite, and" / "the" / "Vite React plugin." The text is
// from an end-to-end run.
func TestInlineCodeDoesNotPushALinePastTheWidth(t *testing.T) {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	r.SetHasDarkBackground(true)
	md := newMarkdownRenderer(true, false, r)
	text := "Please ask an administrator to approve access to `registry.npmjs.org` so we can " +
		"download React, React-DOM, Vite, and the Vite React plugin."
	for width := 30; width <= 140; width++ {
		for _, l := range md.render(text, width) {
			if visibleWidth(l) > width {
				t.Fatalf("width %d: %q is %d columns", width, stripANSI(l), visibleWidth(l))
			}
		}
	}
}

// A list item that wraps carries on under its text, not under its bullet,
// where its second line read as a paragraph of its own. Nothing of it is lost
// on the way, an emoji's width included, and it still fits.
func TestAWrappedListItemHangsUnderItsText(t *testing.T) {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	md := newMarkdownRenderer(true, false, r)
	src := "- ℹ️ 2 vulnerabilities reported (1 moderate, 1 high) — these are from **transitive** " +
		"dependencies used by `Vite`'s build tools and don't affect the game itself at all\n" +
		"- short\n  - a nested item that is also long enough to wrap onto a second line at sixty columns\n\n" +
		"1. first numbered item that is long enough to wrap past the edge of a sixty column terminal\n2. two"
	lines := strings.Split(stripANSI(strings.Join(md.render(src, 60), "\n")), "\n")
	hangs := map[string]string{ // a word on a wrapped line, and what that line must start with
		"tools":  "  ",
		"second": "    ",
		"edge":   "   ",
	}
	for word, indent := range hangs {
		found := false
		for _, l := range lines {
			if strings.Contains(l, word) && !renderedItem.MatchString(l) {
				found = true
				if !strings.HasPrefix(l, indent) || strings.HasPrefix(l, indent+" ") {
					t.Errorf("the line with %q is indented %q, want exactly %q:\n%s",
						word, l[:len(l)-len(strings.TrimLeft(l, " "))], indent, strings.Join(lines, "\n"))
				}
			}
		}
		if !found {
			t.Errorf("%q is not on a wrapped line:\n%s", word, strings.Join(lines, "\n"))
		}
	}
	words := strings.Fields("ℹ️ 2 vulnerabilities reported (1 moderate, 1 high) — these are from transitive " +
		"dependencies used by Vite's build tools and don't affect the game itself at all")
	if got := strings.Fields(strings.Join(lines, " ")); strings.Join(got[1:1+len(words)], " ") != strings.Join(words, " ") {
		t.Errorf("the first item's text changed:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range lines {
		if visibleWidth(l) > 60 {
			t.Errorf("a line is wider than 60: %q", l)
		}
	}
}

// A paragraph with inline code in it wraps where the width says, and nowhere
// else. After a code span the renderer lost count of the line and broke a
// word that fitted: "…vite, and" / "@vitejs/plugin-" / "react.".
func TestAParagraphWithInlineCodeWrapsOnlyWhereItMust(t *testing.T) {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	md := newMarkdownRenderer(true, false, r)
	src := "Perfect! `npm install` completed successfully. It added 63 packages including react, react-dom, " +
		"vite, and @vitejs/plugin-react."
	for width := 90; width <= 130; width++ {
		lines := strings.Split(stripANSI(strings.Join(md.render(src, width), "\n")), "\n")
		if len(lines) > 2 {
			t.Errorf("at width %d the paragraph took %d lines:\n%s", width, len(lines), strings.Join(lines, "\n"))
		}
		if !strings.Contains(strings.Join(lines, "\n"), "@vitejs/plugin-react.") {
			t.Errorf("at width %d @vitejs/plugin-react. was broken:\n%s", width, strings.Join(lines, "\n"))
		}
		for _, l := range lines {
			if visibleWidth(l) > width {
				t.Errorf("at width %d a line is %d wide: %q", width, visibleWidth(l), l)
			}
		}
	}
}
