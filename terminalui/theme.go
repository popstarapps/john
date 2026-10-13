package terminalui

import (
	"io"
	"runtime"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Symbols are the glyphs the transcript is drawn with. The Unicode set is used
// by default; SetASCII swaps in a set that survives Windows consoles, PuTTY,
// and anything else with a narrow font.
type Symbols struct {
	User      string // prompt marker
	Assistant string // assistant message marker: what john says back
	Tool      string // completed tool call
	Fail      string // failed tool call
	Blocked   string // policy-blocked tool call
	Skipped   string // tool call that never ran
	TreeMid   string // detail line, more follow
	TreeEnd   string // last detail line
	Selected  string // checked box in the diff viewer
	Unchecked string // unchecked box in the diff viewer
	Cursor    string // row cursor in modal views
	Queued    string // marker on a prompt waiting for its turn
	Alert     string // degraded-state marker in the status line
	BarFull   string // progress bar, filled
	BarEmpty  string // progress bar, unfilled
	Backspace string // the backspace key, in hints
	Enter     string // the enter key, in hints
	Newline   string // the key that inserts a newline, in hints
	BoxTL     string
	BoxTR     string
	BoxBL     string
	BoxBR     string
	BoxH      string
	BoxV      string
	BoxLT     string
	BoxRT     string
	ArrowLR   string // left/right, in hints
	ArrowUD   string // up/down, in hints
	Thumb     string // scrollbar thumb
	Track     string // scrollbar track
	Ellipsis  string
	Dash      string // range separator, as in "4–11 of 12"
	Dot       string // separator between status fields
	Add       string // diff added line marker
	Remove    string // diff removed line marker
	Spinner   []string
}

// UnicodeSymbols is the default glyph set.
func UnicodeSymbols() Symbols {
	return Symbols{
		User:      "❯",
		Assistant: "→",
		Tool:      "●",
		Fail:      "✗",
		Blocked:   "✗",
		Skipped:   "○",
		TreeMid:   "├",
		TreeEnd:   "└",
		Selected:  "✓",
		Unchecked: " ",
		Cursor:    "▸",
		Queued:    "⇢",
		Alert:     "▲",
		BarFull:   "█",
		BarEmpty:  "░",
		Backspace: "⌫",
		Enter:     "⏎",
		Newline:   newlineKey("alt+⏎"),
		BoxTL:     "╭",
		BoxTR:     "╮",
		BoxBL:     "╰",
		BoxBR:     "╯",
		BoxH:      "─",
		BoxV:      "│",
		BoxLT:     "├",
		BoxRT:     "┤",
		ArrowLR:   "←/→",
		ArrowUD:   "↑/↓",
		Thumb:     "█",
		Track:     " ", // an empty track: next to the border, a drawn one reads as a second border
		Ellipsis:  "…",
		Dash:      "–",
		Dot:       "·",
		Add:       "+",
		Remove:    "−",
		// The spinner for work that is happening rather than being thought
		// about: tool calls and progress blocks. The busy line draws its own,
		// so the two are never mistaken for each other.
		Spinner: []string{"·", "•", "●", "•"},
	}
}

// ASCIISymbols is the fallback glyph set for terminals that cannot be trusted
// with box drawing or braille.
func ASCIISymbols() Symbols {
	return Symbols{
		User:      ">",
		Assistant: "->",
		Tool:      "*",
		Fail:      "x",
		Blocked:   "x",
		Skipped:   "o",
		TreeMid:   "+",
		TreeEnd:   "\\",
		Selected:  "x",
		Unchecked: " ",
		Cursor:    ">",
		Queued:    "->",
		Alert:     "!",
		BarFull:   "#",
		BarEmpty:  "-",
		Backspace: "bksp",
		Enter:     "ret",
		Newline:   newlineKey("alt+ret"),
		BoxTL:     "+",
		BoxTR:     "+",
		BoxBL:     "+",
		BoxBR:     "+",
		BoxH:      "-",
		BoxV:      "|",
		BoxLT:     "+",
		BoxRT:     "+",
		ArrowLR:   "left/right",
		ArrowUD:   "up/down",
		Thumb:     "#",
		Track:     " ",
		Ellipsis:  "...",
		Dash:      "-",
		Dot:       "-",
		Add:       "+",
		Remove:    "-",
		Spinner:   []string{".", "o", "O", "o"},
	}
}

// newlineKey is the binding the newline hint names.
//
// The input accepts alt+enter, ctrl+j and shift+enter everywhere — the keymap
// in input.go — but a hint has room for one of them, and on Windows alt+enter
// is the console's own fullscreen toggle and never reaches the program. So the
// hint names ctrl+j there and the spelling the caller asked for everywhere
// else.
//
// It sits with the glyphs rather than in the status line that draws it because
// it is the same kind of thing as Backspace and Enter — what this terminal
// calls a key — and because a view that reads runtime.GOOS renders one way on
// the machine that records a golden frame and another on the machine that
// checks it, which is a test that cannot be green everywhere.
func newlineKey(alt string) string {
	if runtime.GOOS == "windows" {
		return "ctrl+j"
	}
	return alt
}

// Theme is the complete visual configuration of the UI. Every style is
// exported so an application can override individual pieces of DefaultTheme
// without reconstructing it.
type Theme struct {
	Symbols Symbols

	// SyntaxStyle is the chroma style used to colour code in the diff viewer,
	// and SyntaxFormatter the chroma formatter to render it with. An empty
	// SyntaxStyle turns highlighting off, which is what a terminal with no
	// colour gets.
	SyntaxStyle     string
	SyntaxFormatter string

	// HasColor is false on a terminal that cannot show any, which is what the
	// animated busy line checks before animating anything.
	HasColor bool
	// BusyBase and BusyHigh are the ends of the busy line's colour ramp, as
	// "#rrggbb". They are blended per character, so they are kept as values
	// rather than styles.
	BusyBase string
	BusyHigh string

	// newStyle builds a style bound to the same renderer as everything else in
	// the theme. The busy animations mix colours per frame and cannot use
	// prebuilt styles; without this they would silently fall back to
	// lipgloss's global renderer, which is a different terminal.
	newStyle func() lipgloss.Style
	// asciiMode is set by SetASCII, so the spinners know to use their ASCII
	// frames rather than braille.
	asciiMode bool

	// Transcript.
	User      lipgloss.Style
	UserGlyph lipgloss.Style
	Assistant lipgloss.Style
	AssistGly lipgloss.Style
	Muted     lipgloss.Style
	Text      lipgloss.Style
	Warn      lipgloss.Style
	Error     lipgloss.Style
	Tree      lipgloss.Style

	// Tool calls. Verb holds per-verb styles; VerbDefault covers the rest.
	Verb        map[string]lipgloss.Style
	VerbDefault lipgloss.Style
	Target      lipgloss.Style
	Meta        lipgloss.Style
	StatusOK    lipgloss.Style
	StatusFail  lipgloss.Style
	StatusBlock lipgloss.Style
	StatusSkip  lipgloss.Style

	// Chrome. The status line paints no background of its own — it sits on the
	// terminal's, like the rule above it — so it reads as the last line of the
	// session rather than as a band stuck to the bottom of the window.
	Spinner     lipgloss.Style
	Prompt      lipgloss.Style
	StatusBar   lipgloss.Style
	StatusHint  lipgloss.Style
	StatusAlert lipgloss.Style
	BarFull     lipgloss.Style
	BarEmpty    lipgloss.Style
	Hint        lipgloss.Style
	HintKey     lipgloss.Style
	Border      lipgloss.Style
	Rule        lipgloss.Style

	// Title rows for modal views, and the scrollbar drawn inside them. The row
	// is ruled out to the edges in the border's own colour rather than painted
	// with a background: a filled bar inside a drawn box reads as two
	// competing frames.
	TitleLabel  lipgloss.Style
	TitleDetail lipgloss.Style
	TitleDim    lipgloss.Style
	ScrollThumb lipgloss.Style
	ScrollTrack lipgloss.Style

	// The logo.
	Logo       lipgloss.Style
	LogoMark   lipgloss.Style
	LogoAccent lipgloss.Style

	// Buttons in modal views.
	Button          lipgloss.Style
	ButtonMuted     lipgloss.Style
	ButtonSelected  lipgloss.Style
	ButtonDanger    lipgloss.Style
	ButtonDangerSel lipgloss.Style

	// Modals.
	PanelHead lipgloss.Style
	CursorRow lipgloss.Style
	Selected  lipgloss.Style
	Danger    lipgloss.Style

	// Diff.
	DiffAdd    lipgloss.Style
	DiffRemove lipgloss.Style
	// The part of a changed line that actually differs from its pair.
	DiffAddEmph    lipgloss.Style
	DiffRemoveEmph lipgloss.Style
	DiffCtx        lipgloss.Style
	DiffHunk       lipgloss.Style
	DiffPath       lipgloss.Style

	// Completion popup.
	PopupItem     lipgloss.Style
	PopupSelected lipgloss.Style
	PopupDetail   lipgloss.Style

	// Prompts waiting for the current turn to end.
	Queued      lipgloss.Style
	QueuedGlyph lipgloss.Style
}

func adaptive(light, dark string) lipgloss.TerminalColor {
	return lipgloss.AdaptiveColor{Light: light, Dark: dark}
}

// DefaultTheme builds the standard theme against a renderer bound to w. The
// renderer decides the color profile, so a redirected or NO_COLOR output
// degrades to plain text without any special casing here.
func DefaultTheme(w io.Writer) *Theme {
	r := lipgloss.NewRenderer(w)
	return themeFromRenderer(r)
}

func themeFromRenderer(r *lipgloss.Renderer) *Theme {
	s := r.NewStyle
	var (
		muted   = adaptive("#6c6f76", "#7c7f88")
		faint   = adaptive("#8a8d94", "#5f636b")
		fg      = adaptive("#1f2228", "#dfe2e8")
		blue    = adaptive("#0a67c2", "#63b3ff")
		green   = adaptive("#177245", "#5fd38d")
		yellow  = adaptive("#9a6700", "#e3b341")
		red     = adaptive("#b3261e", "#ff7b72")
		magenta = adaptive("#8250df", "#c297ff")
		cyan    = adaptive("#0b6e75", "#56d4dd")
		accent  = adaptive("#0a67c2", "#7aa2f7")

		barFG    = adaptive("#4a4e57", "#9aa0ab")
		btnBG    = adaptive("#dfe3ea", "#2c313b")
		btnOnFG  = adaptive("#ffffff", "#11141a")
		dangerBG = adaptive("#b3261e", "#ff7b72")
		brand    = adaptive("#1f9e2b", "#3ad13a") // john green
	)

	t := &Theme{
		Symbols:  UnicodeSymbols(),
		newStyle: r.NewStyle,

		SyntaxStyle:     syntaxStyleFor(r),
		SyntaxFormatter: syntaxFormatterFor(r),
		HasColor:        r.ColorProfile() != termenv.Ascii,
		BusyBase:        busyRamp(r, 0),
		BusyHigh:        busyRamp(r, 1),

		User:      s().Foreground(fg).Bold(true),
		UserGlyph: s().Foreground(accent).Bold(true),
		Assistant: s().Foreground(fg),
		// The answer is the point of the session, so its marker is the plain
		// foreground rather than a colour that categorises it.
		AssistGly: s().Foreground(fg),
		Muted:     s().Foreground(muted),
		Text:      s().Foreground(fg),
		Warn:      s().Foreground(yellow),
		Error:     s().Foreground(red),
		Tree:      s().Foreground(faint),

		Verb: map[string]lipgloss.Style{
			"read":   s().Foreground(blue),
			"edit":   s().Foreground(green),
			"write":  s().Foreground(green),
			"run":    s().Foreground(yellow),
			"test":   s().Foreground(yellow),
			"search": s().Foreground(blue),
			"diff":   s().Foreground(cyan),
			"apply":  s().Foreground(magenta),
			"proxy":  s().Foreground(magenta),
			"model":  s().Foreground(cyan),
		},
		VerbDefault: s().Foreground(accent),
		Target:      s().Foreground(fg),
		Meta:        s().Foreground(muted),
		StatusOK:    s().Foreground(green),
		StatusFail:  s().Foreground(red),
		StatusBlock: s().Foreground(red),
		StatusSkip:  s().Foreground(muted),

		Spinner:     s().Foreground(accent),
		Prompt:      s().Foreground(accent).Bold(true),
		StatusBar:   s().Foreground(barFG),
		StatusHint:  s().Foreground(barFG),
		StatusAlert: s().Foreground(red).Bold(true),
		BarFull:     s().Foreground(accent),
		BarEmpty:    s().Foreground(faint),
		Hint:        s().Foreground(faint),
		HintKey:     s().Foreground(muted).Bold(true),
		Border:      s().Foreground(faint),
		Rule:        s().Foreground(faint),

		TitleLabel:  s().Foreground(accent).Bold(true),
		TitleDetail: s().Foreground(fg).Bold(true),
		TitleDim:    s().Foreground(muted),
		ScrollThumb: s().Foreground(muted),
		ScrollTrack: s().Foreground(faint),

		Logo:       s().Foreground(fg).Bold(true),
		LogoMark:   s().Foreground(muted),
		LogoAccent: s().Foreground(brand).Bold(true),

		Button:          s().Foreground(fg).Background(btnBG).Padding(0, 1),
		ButtonMuted:     s().Foreground(faint).Background(btnBG).Padding(0, 1),
		ButtonSelected:  s().Foreground(btnOnFG).Background(accent).Bold(true).Padding(0, 1),
		ButtonDanger:    s().Foreground(red).Background(btnBG).Padding(0, 1),
		ButtonDangerSel: s().Foreground(btnOnFG).Background(dangerBG).Bold(true).Padding(0, 1),

		PanelHead: s().Foreground(accent).Bold(true),
		CursorRow: s().Foreground(fg).Bold(true),
		Selected:  s().Foreground(green),
		Danger:    s().Foreground(red).Bold(true),

		DiffAdd:        s().Foreground(green),
		DiffRemove:     s().Foreground(red),
		DiffAddEmph:    s().Foreground(btnOnFG).Background(green).Bold(true),
		DiffRemoveEmph: s().Foreground(btnOnFG).Background(dangerBG).Bold(true),
		DiffCtx:        s().Foreground(muted),
		DiffHunk:       s().Foreground(cyan),
		DiffPath:       s().Foreground(fg).Bold(true),

		PopupItem:     s().Foreground(fg),
		PopupSelected: s().Foreground(accent).Bold(true),
		PopupDetail:   s().Foreground(faint),

		Queued:      s().Foreground(muted),
		QueuedGlyph: s().Foreground(accent),
	}
	return t
}

// busyRamp is the colour ramp the busy line animates along: from something
// close to the muted text colour up to the accent, in whichever direction the
// terminal's background makes brighter.
func busyRamp(r *lipgloss.Renderer, end int) string {
	if r.HasDarkBackground() {
		if end == 0 {
			return "#4b5563"
		}
		return "#a5c8ff"
	}
	if end == 0 {
		return "#a8adb7"
	}
	return "#0a4fa0"
}

// syntaxStyleFor picks a chroma style to match the terminal, and none at all
// when the terminal has no colour to give.
func syntaxStyleFor(r *lipgloss.Renderer) string {
	if r.ColorProfile() == termenv.Ascii {
		return ""
	}
	if r.HasDarkBackground() {
		return "github-dark"
	}
	return "github"
}

func syntaxFormatterFor(r *lipgloss.Renderer) string {
	switch r.ColorProfile() {
	case termenv.Ascii:
		return ""
	case termenv.TrueColor:
		return "terminal16m"
	default:
		return "terminal256"
	}
}

// style builds an ad-hoc style on the theme's own renderer.
func (t *Theme) style() lipgloss.Style {
	if t.newStyle == nil {
		return lipgloss.NewStyle()
	}
	return t.newStyle()
}

// SetASCII swaps in the ASCII glyph set.
func (t *Theme) SetASCII() {
	t.Symbols = ASCIISymbols()
	t.asciiMode = true
}

// verbStyle returns the style for a tool verb.
func (t *Theme) verbStyle(verb string) lipgloss.Style {
	if st, ok := t.Verb[verb]; ok {
		return st
	}
	return t.VerbDefault
}

// toolGlyph returns the glyph and style for a tool call: the verb's own colour
// when it worked, so the dot belongs to the line it marks — blue beside read,
// orange beside run — and the alarm colours only when there is something to
// raise an alarm about.
func (t *Theme) toolGlyph(verb string, st ToolStatus) (string, lipgloss.Style) {
	if st == ToolOK {
		return t.Symbols.Tool, t.verbStyle(verb)
	}
	return t.statusGlyph(st)
}

// statusGlyph returns the glyph and style for a finished tool call.
func (t *Theme) statusGlyph(st ToolStatus) (string, lipgloss.Style) {
	switch st {
	case ToolFail:
		return t.Symbols.Fail, t.StatusFail
	case ToolBlocked:
		return t.Symbols.Blocked, t.StatusBlock
	case ToolSkipped:
		return t.Symbols.Skipped, t.StatusSkip
	default:
		return t.Symbols.Tool, t.StatusOK
	}
}
