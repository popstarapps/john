package terminalui

import (
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// highlighter colours source lines in the diff viewer.
//
// The review step is the security control: a developer who cannot read a diff
// quickly is a developer who approves it without reading. Syntax colour is not
// decoration here, it is what makes a changed string literal in a build script
// look different from a reindented line.
type highlighter struct {
	styleName string
	formatter chroma.Formatter
	style     *chroma.Style
	cache     map[string]string
	lexers    map[string]chroma.Lexer
}

func newHighlighter(styleName, formatterName string) *highlighter {
	if styleName == "" || formatterName == "" {
		return nil
	}
	st := styles.Get(styleName)
	if st == nil {
		return nil
	}
	f := formatters.Get(formatterName)
	if f == nil {
		return nil
	}
	return &highlighter{
		styleName: styleName,
		formatter: f,
		style:     st,
		cache:     map[string]string{},
		lexers:    map[string]chroma.Lexer{},
	}
}

// line highlights one line of a file. Lines are highlighted independently — a
// diff has no context to give a lexer anyway — and the result is cached,
// because the viewer re-lays out on every selection change.
func (h *highlighter) line(path, text string) string {
	if h == nil || strings.TrimSpace(text) == "" {
		return text
	}
	key := path + "\x00" + text
	if got, ok := h.cache[key]; ok {
		return got
	}
	out := h.render(path, text)
	// A pathological review should not turn into a memory leak.
	if len(h.cache) > 20000 {
		h.cache = map[string]string{}
	}
	h.cache[key] = out
	return out
}

func (h *highlighter) render(path, text string) string {
	lexer, ok := h.lexers[path]
	if !ok {
		lexer = lexers.Match(path)
		if lexer == nil {
			lexer = lexers.Fallback
		}
		lexer = chroma.Coalesce(lexer)
		h.lexers[path] = lexer
	}
	it, err := lexer.Tokenise(nil, text)
	if err != nil {
		return text
	}
	var sb strings.Builder
	if err := h.formatter.Format(&sb, h.style, it); err != nil {
		return text
	}
	return strings.TrimRight(sb.String(), "\n")
}

// wordSpan is the part of a line that actually changed, as rune offsets into
// the line.
type wordSpan struct{ start, end int }

// changedSpan compares a removed line with the added line that replaced it and
// returns the span of each that differs, by trimming the common prefix and
// suffix at token boundaries.
//
// This is not a full word diff, and it does not need to be: what it catches is
// the one-character change in the middle of a long line — a flipped comparison,
// an edited URL in a build script — which is exactly what a reviewer's eye
// slides past.
func changedSpan(before, after string) (wordSpan, wordSpan) {
	// Indentation is not code: the lines are compared after it, so a
	// re-indented line still lines up with what it was, and the emphasis never
	// lands on a block of leading whitespace.
	b, a := []rune(before), []rune(after)
	ib, ia := indent(b), indent(a)
	bs, as := codeSpan(b[ib:], a[ia:])
	if bs.empty() && as.empty() {
		return wordSpan{}, wordSpan{}
	}
	return wordSpan{bs.start + ib, bs.end + ib}, wordSpan{as.start + ia, as.end + ia}
}

// codeSpan is changedSpan on two lines without their indentation.
func codeSpan(b, a []rune) (wordSpan, wordSpan) {
	pre := 0
	for pre < len(b) && pre < len(a) && b[pre] == a[pre] {
		pre++
	}
	suf := 0
	for suf < len(b)-pre && suf < len(a)-pre && b[len(b)-1-suf] == a[len(a)-1-suf] {
		suf++
	}
	// Back the boundaries out to whole tokens, so half-words are not
	// highlighted in the middle of an identifier.
	for pre > 0 && isWordRune(b[pre-1]) && pre < len(b) && isWordRune(b[pre]) {
		pre--
	}
	for pre > 0 && pre <= len(a)-1 && isWordRune(a[pre-1]) && isWordRune(a[pre]) {
		pre--
	}
	for suf > 0 && suf < len(b)-pre && isWordRune(b[len(b)-suf]) && isWordRune(b[len(b)-suf-1]) {
		suf--
	}
	for suf > 0 && suf < len(a)-pre && isWordRune(a[len(a)-suf]) && isWordRune(a[len(a)-suf-1]) {
		suf--
	}

	bs := wordSpan{start: pre, end: len(b) - suf}
	as := wordSpan{start: pre, end: len(a) - suf}
	// Nothing in common, or everything: no point emphasising the whole line.
	if bs.start <= 0 && bs.end >= len(b) && as.start <= 0 && as.end >= len(a) {
		return wordSpan{}, wordSpan{}
	}
	// A removal and an addition that happen to sit side by side are usually two
	// different lines, not one line edited: `return resp, nil` and
	// `delay := retryAfter(h)` share nothing. Emphasis is for a line that
	// mostly stayed the same, so a pair that shares less than half of its code
	// gets none, and reads as what it is — one line out, another in.
	if 2*(pre+suf) < max(len(b), len(a)) {
		return wordSpan{}, wordSpan{}
	}
	return bs, as
}

// indent is how many leading spaces and tabs a line has.
func indent(r []rune) int {
	n := 0
	for n < len(r) && (r[n] == ' ' || r[n] == '\t') {
		n++
	}
	return n
}

func isWordRune(r rune) bool {
	return r == '_' || r == '$' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r > 127
}

func (s wordSpan) empty() bool { return s.end <= s.start }
