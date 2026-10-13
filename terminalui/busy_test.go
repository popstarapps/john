package terminalui

import (
	"io"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// An animated busy line is the only thing moving on screen during a long turn.
// It may be as lively as it likes; it may not change width, leak escape codes
// into a monochrome terminal, or stop being readable.

func animTheme() *Theme {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	r.SetHasDarkBackground(true)
	return themeFromRenderer(r)
}

var allSpinners = append([]BusySpinner{}, randomSpinners...)
var allEffects = append([]BusyEffect{}, randomEffects...)

// A spinner is one column, always. It sits where the tool glyph sits, and a
// spinner that is sometimes two columns wide makes the whole live region jump.
func TestSpinnersAreOneColumn(t *testing.T) {
	for _, ascii := range []bool{false, true} {
		th := animTheme()
		if ascii {
			th.SetASCII()
		}
		for _, sp := range allSpinners {
			for frame := 0; frame < 80; frame++ {
				got := th.busySpinner(sp, frame)
				if w := visibleWidth(got); w != 1 {
					t.Fatalf("spinner %d (ascii=%v) frame %d is %d columns: %q",
						sp, ascii, frame, w, stripANSI(got))
				}
			}
		}
	}
}

func TestSpinnersActuallySpin(t *testing.T) {
	th := animTheme()
	for _, sp := range allSpinners {
		seen := map[string]bool{}
		for frame := 0; frame < 80; frame++ {
			seen[stripANSI(th.busySpinner(sp, frame))] = true
		}
		if len(seen) < 2 {
			t.Fatalf("spinner %d never changes: %v", sp, seen)
		}
	}
}

func TestASCIISpinnersAreASCII(t *testing.T) {
	th := animTheme()
	th.SetASCII()
	for _, sp := range allSpinners {
		for frame := 0; frame < 40; frame++ {
			for _, r := range stripANSI(th.busySpinner(sp, frame)) {
				if r > 127 {
					t.Fatalf("spinner %d drew %q in ASCII mode", sp, r)
				}
			}
		}
	}
}

func TestUnknownSpinnerFallsBack(t *testing.T) {
	th := animTheme()
	if got := visibleWidth(th.busySpinner(BusySpinner(9999), 3)); got != 1 {
		t.Fatalf("an unknown spinner should still draw one column, got %d", got)
	}
}

// An effect may repaint the label as much as it likes. It may not resize it,
// reword it, or leak escapes into a terminal that cannot show colour.
func TestEffectsKeepTheLabelIntact(t *testing.T) {
	th := animTheme()
	const label = "thinking"
	for _, fx := range allEffects {
		for frame := 0; frame < 80; frame++ {
			got := th.busyLabel(fx, label, frame)
			if w := visibleWidth(got); w != len(label) {
				t.Fatalf("effect %d frame %d is %d columns, want %d", fx, frame, w, len(label))
			}
			if stripANSI(got) != label {
				t.Fatalf("effect %d frame %d changed the text: %q", fx, frame, stripANSI(got))
			}
		}
	}
}

func TestEffectsAnimate(t *testing.T) {
	th := animTheme()
	for _, fx := range allEffects {
		seen := map[string]bool{}
		for frame := 0; frame < 60; frame++ {
			seen[th.busyLabel(fx, "thinking", frame)] = true
		}
		switch fx {
		case EffectNone:
			if len(seen) != 1 {
				t.Fatalf("EffectNone should be still, got %d variants", len(seen))
			}
		default:
			if len(seen) < 5 {
				t.Fatalf("effect %d only produced %d frames", fx, len(seen))
			}
		}
	}
}

func TestEffectsFallBackWithoutColour(t *testing.T) {
	th := themeFromRenderer(lipgloss.NewRenderer(io.Discard))
	if th.HasColor {
		t.Fatal("a discarded writer has no colour")
	}
	want := th.Muted.Render("thinking")
	for _, fx := range allEffects {
		for frame := 0; frame < 10; frame++ {
			got := th.busyLabel(fx, "thinking", frame)
			if got != want {
				t.Fatalf("effect %d should fall back without colour: %q", fx, got)
			}
			if strings.Contains(got, "\x1b") {
				t.Fatalf("effect %d emitted an escape into a monochrome terminal", fx)
			}
		}
	}
	// Spinners still spin without colour.
	for _, sp := range allSpinners {
		if stripANSI(th.busySpinner(sp, 0)) == stripANSI(th.busySpinner(sp, 3)) &&
			stripANSI(th.busySpinner(sp, 0)) == stripANSI(th.busySpinner(sp, 7)) {
			t.Fatalf("spinner %d stopped moving without colour", sp)
		}
	}
}

// The per-character effects have to actually be per character.
func TestPerCharacterEffectsColourSeparately(t *testing.T) {
	th := animTheme()
	perRune := []BusyEffect{EffectShimmer, EffectRipple, EffectFlow, EffectReveal, EffectScan}
	for _, fx := range perRune {
		best := 0
		for frame := 0; frame < 30; frame++ {
			if n := strings.Count(th.busyLabel(fx, "thinking", frame), "38;2;"); n > best {
				best = n
			}
		}
		if best < 4 {
			t.Fatalf("effect %d never used more than %d colours at once", fx, best)
		}
	}
}

// Sparkle has to be stable: the same character lights on the same frame every
// time, or it flickers instead of twinkling.
func TestSparkleIsDeterministic(t *testing.T) {
	th := animTheme()
	for frame := 0; frame < 20; frame++ {
		a := th.busyLabel(EffectSparkle, "thinking", frame)
		b := th.busyLabel(EffectSparkle, "thinking", frame)
		if a != b {
			t.Fatalf("frame %d rendered differently twice", frame)
		}
	}
	// And it does light something, sometimes.
	lit := false
	for frame := 0; frame < 60 && !lit; frame++ {
		if strings.Count(th.busyLabel(EffectSparkle, "thinking", frame), "38;2;") > 1 {
			lit = true
		}
	}
	if !lit {
		t.Fatal("sparkle never sparkled")
	}
}

func TestLerpHex(t *testing.T) {
	cases := []struct {
		from, to string
		t        float64
		want     string
	}{
		{"#000000", "#ffffff", 0, "#000000"},
		{"#000000", "#ffffff", 1, "#ffffff"},
		{"#000000", "#ffffff", 0.5, "#808080"},
		{"#102030", "#405060", 0.5, "#283848"},
		{"not a colour", "#ffffff", 0.5, "not a colour"},
		{"#000000", "bad", 0.5, "#000000"},
		{"#000000", "#ffffff", -1, "#000000"},
		{"#000000", "#ffffff", 2, "#ffffff"},
	}
	for _, c := range cases {
		if got := lerpHex(c.from, c.to, c.t); got != c.want {
			t.Fatalf("lerpHex(%q, %q, %v) = %q, want %q", c.from, c.to, c.t, got, c.want)
		}
	}
}

func TestEmptyAndTinyLabels(t *testing.T) {
	th := animTheme()
	for _, fx := range allEffects {
		if got := th.busyLabel(fx, "", 3); got != "" {
			t.Fatalf("effect %d on an empty label: %q", fx, got)
		}
		if got := stripANSI(th.busyLabel(fx, "x", 3)); got != "x" {
			t.Fatalf("effect %d on a one character label: %q", fx, got)
		}
	}
}

// Random means random, it means both halves, and it means per turn.
func TestRandomPicksBothPerTurn(t *testing.T) {
	o := testOptions(io.Discard)
	o.Theme = animTheme()
	o.BusySpinner, o.BusyEffect = SpinnerRandom, EffectRandom
	q := newActionQueue(64)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	spins, fx := map[BusySpinner]int{}, map[BusyEffect]int{}
	for turn := 0; turn < 600; turn++ {
		send(m, Busy{Label: "thinking"})
		spins[m.busySpin]++
		fx[m.busyFx]++
		send(m, Idle{})
	}
	if len(spins) != len(randomSpinners) {
		t.Fatalf("600 turns should have shown all %d spinners, saw %d", len(randomSpinners), len(spins))
	}
	if len(fx) != len(randomEffects) {
		t.Fatalf("600 turns should have shown all %d effects, saw %d", len(randomEffects), len(fx))
	}

	// The pair is fixed for the length of a turn.
	send(m, Busy{Label: "thinking"})
	sp, e := m.busySpin, m.busyFx
	for i := 0; i < 10; i++ {
		m.Update(spinTickMsg{})
	}
	if m.busySpin != sp || m.busyFx != e {
		t.Fatal("the pair should not change under a turn's feet")
	}
}

func TestExplicitChoicesAreHonoured(t *testing.T) {
	o := testOptions(io.Discard)
	o.Theme = animTheme()
	o.BusySpinner = SpinnerCircle
	o.BusyEffect = EffectPulse
	q := newActionQueue(64)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for turn := 0; turn < 20; turn++ {
		send(m, Busy{Label: "thinking"})
		if m.busySpin != SpinnerCircle || m.busyFx != EffectPulse {
			t.Fatalf("turn %d: got spinner %d, effect %d", turn, m.busySpin, m.busyFx)
		}
		send(m, Idle{})
	}
}

func TestRandomIsTheDefault(t *testing.T) {
	o := Options{}
	o.applyDefaults()
	if o.BusySpinner != SpinnerRandom || o.BusyEffect != EffectRandom {
		t.Fatalf("both should default to random, got %d and %d", o.BusySpinner, o.BusyEffect)
	}
}

// A spinner beside a command means that command is running; a spinner beside
// the label means the agent is thinking. They are different animations because
// they say different things, and the tool one never joins the random pool.
func TestToolSpinnerIsReservedForWork(t *testing.T) {
	o := testOptions(io.Discard)
	o.Theme = animTheme()
	o.BusySpinner = SpinnerBinary // unmistakable, and not what tools use
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	dot := spinnerDefs[SpinnerDot].frames
	send(m, ToolStart{ID: "t", Verb: "run", Target: "go test"}, Busy{Label: "thinking"})

	// The tool keeps its own spinner while the turn runs its own.
	for frame := 0; frame < 16; frame++ {
		got := m.spinner()
		if !slices.Contains(dot, got) {
			t.Fatalf("frame %d: a running tool should show the work spinner, got %q", frame, got)
		}
		m.Update(spinTickMsg{})
	}
	if line := stripANSI(m.busyLine()); !strings.ContainsAny(line, "01") {
		t.Fatalf("the busy line should still draw the turn's own spinner: %q", line)
	}

	// And it is never drawn at random for the busy line.
	if slices.Contains(randomSpinners, SpinnerDot) {
		t.Fatal("the tool spinner must stay out of the random pool")
	}
	prev := SpinnerRandom
	for i := 0; i < 500; i++ {
		prev = pickSpinner(SpinnerRandom, prev)
		if prev == SpinnerDot {
			t.Fatal("random picked the tool spinner")
		}
	}
}

// It is what progress blocks use too: they are work happening, not thought.
func TestProgressUsesTheToolSpinner(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, Progress{ID: "p", Label: "building sandbox image", Percent: 40})
	dot := spinnerDefs[SpinnerDot].frames
	if !slices.Contains(dot, m.spinner()) {
		t.Fatalf("progress should spin with the work spinner, got %q", m.spinner())
	}
	if !strings.ContainsAny(stripANSI(m.View()), strings.Join(dot, "")) {
		t.Fatalf("the work spinner should be on screen:\n%s", stripANSI(m.View()))
	}
}

func TestBusyLineUsesBothHalves(t *testing.T) {
	o := testOptions(io.Discard)
	o.Theme = animTheme()
	o.BusySpinner = SpinnerBar
	o.BusyEffect = EffectShimmer
	q := newActionQueue(8)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	send(m, Busy{Label: "thinking"})

	first := m.busyLine()
	m.Update(spinTickMsg{})
	m.Update(spinTickMsg{})
	second := m.busyLine()
	if first == second {
		t.Fatal("the busy line should change as the spinner ticks")
	}
	if !strings.Contains(stripANSI(first), "thinking") {
		t.Fatalf("the label should survive: %q", stripANSI(first))
	}
	if !strings.ContainsAny(stripANSI(first), "▁▂▃▄▅▆▇█") {
		t.Fatalf("the chosen spinner should be drawn: %q", stripANSI(first))
	}
}

// The point of a random pair is that it changes. Drawing the same one twice
// running looks exactly like not changing it at all.
func TestPairNeverRepeatsBackToBack(t *testing.T) {
	o := testOptions(io.Discard)
	o.Theme = animTheme()
	o.BusySpinner, o.BusyEffect = SpinnerRandom, EffectRandom
	q := newActionQueue(64)
	m := newUIModel(&o, q, newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard)), 100, 30)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	prevSpin, prevFx := SpinnerRandom, EffectRandom
	for turn := 0; turn < 400; turn++ {
		send(m, Busy{Label: "thinking"})
		if turn > 0 {
			if m.busySpin == prevSpin {
				t.Fatalf("turn %d drew the same spinner twice running (%d)", turn, m.busySpin)
			}
			if m.busyFx == prevFx {
				t.Fatalf("turn %d drew the same effect twice running (%d)", turn, m.busyFx)
			}
		}
		prevSpin, prevFx = m.busySpin, m.busyFx
		send(m, Idle{})
	}
}

func TestPickDifferentEdgeCases(t *testing.T) {
	if got := pickDifferent([]int{7}, 7); got != 7 {
		t.Fatalf("a pool of one has nothing else to offer, got %d", got)
	}
	if got := pickDifferent([]int{}, 3); got != 0 {
		t.Fatalf("an empty pool should give the zero value, got %d", got)
	}
	for i := 0; i < 100; i++ {
		if got := pickDifferent([]int{1, 2}, 1); got != 2 {
			t.Fatalf("got %d", got)
		}
	}
}

// The dot that spins is the dot that stays, and it belongs to the line it
// marks: blue beside read, orange beside run. Nothing jumps or changes colour
// when a call finishes — it just stops pulsing.
func TestRunningGlyphWearsItsFinalColour(t *testing.T) {
	th := animTheme()
	md := newMarkdownRenderer(false, false, lipgloss.NewRenderer(io.Discard))
	colour := func(s string) string {
		if i := strings.Index(s, "m"); i > 0 {
			return s[:i]
		}
		return ""
	}
	glyphColour := func(verb string, done bool, st ToolStatus) string {
		b := &block{kind: kindTool, verb: verb, target: "x", done: done, status: st}
		return colour(b.render(th, md, 80, th.Symbols.Tool)[0])
	}

	for _, verb := range []string{"read", "run", "edit", "apply"} {
		running := glyphColour(verb, false, ToolOK)
		finished := glyphColour(verb, true, ToolOK)
		if running == "" {
			t.Fatalf("%s: expected a styled glyph", verb)
		}
		if running != finished {
			t.Fatalf("%s: running glyph %q, finished glyph %q", verb, running, finished)
		}
		// And it is the verb's colour, not one colour for every line.
		want := colour(th.verbStyle(verb).Render("x"))
		if running != want {
			t.Fatalf("%s: glyph is %q, the verb is %q", verb, running, want)
		}
	}

	// Different verbs, different dots.
	if glyphColour("read", true, ToolOK) == glyphColour("run", true, ToolOK) {
		t.Fatal("read and run should not share a dot colour")
	}

	// A failure recolours at the end, because that is news.
	for _, st := range []ToolStatus{ToolFail, ToolBlocked, ToolSkipped} {
		if glyphColour("run", true, st) == glyphColour("run", false, ToolOK) {
			t.Fatalf("status %d should not look like a healthy run", st)
		}
	}
}
