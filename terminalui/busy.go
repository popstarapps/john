package terminalui

import (
	"math"
	"math/rand/v2"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The busy line is two independent things: one character that animates, and an
// effect on the label beside it. They are chosen separately, so a session mixes
// fourteen spinners with eight effects rather than picking from one list of
// prearranged pairs.
//
// Rules both halves keep: the spinner is always exactly one column, no effect
// changes the width of the label, and on a terminal without colour every effect
// degrades to plain dim text while the spinners carry on spinning.

// BusySpinner selects the one-character animation.
type BusySpinner int

const (
	// SpinnerRandom picks one for each turn. This is the default.
	SpinnerRandom BusySpinner = iota
	// SpinnerBraille is the classic braille wheel.
	SpinnerBraille
	// SpinnerBrailleDots is the fuller braille wheel, with a hole that orbits.
	SpinnerBrailleDots
	// SpinnerBar is a bar that rises and falls in place, brightening as it
	// grows.
	SpinnerBar
	// SpinnerColumn fills the cell from the left instead of from the bottom.
	SpinnerColumn
	// SpinnerArc is a rotating arc.
	SpinnerArc
	// SpinnerCircle is a filled circle turning through quarters.
	SpinnerCircle
	// SpinnerDot is a dot that swells and shrinks. It is what running tool
	// calls and progress blocks spin with, so it is deliberately kept out of
	// the random pool: on screen it means work is happening, not that the agent
	// is thinking about it.
	SpinnerDot
	// SpinnerQuadrant walks a small block around the corners of its cell.
	SpinnerQuadrant
	// SpinnerBounce sends a single braille dot around the rim.
	SpinnerBounce
	// SpinnerTriangle turns a solid triangle through four corners.
	SpinnerTriangle
	// SpinnerHalves flips between the halves of the cell.
	SpinnerHalves
	// SpinnerFill fills the cell up with braille dots and empties it again.
	SpinnerFill
	// SpinnerPipe is the oldest spinner there is.
	SpinnerPipe
	// SpinnerBinary counts, which is either charming or annoying.
	SpinnerBinary
)

// BusyEffect selects what happens to the label.
type BusyEffect int

const (
	// EffectRandom picks one for each turn. This is the default.
	EffectRandom BusyEffect = iota
	// EffectNone leaves the label dim and still.
	EffectNone
	// EffectShimmer sweeps a bright band along it, the way a loading skeleton
	// does.
	EffectShimmer
	// EffectPulse breathes the whole label between dim and bright.
	EffectPulse
	// EffectFlow runs a colour ramp through the text, continuously.
	EffectFlow
	// EffectRipple is a standing wave: several soft crests travelling at once.
	EffectRipple
	// EffectReveal brightens the label a character at a time, holds it, and
	// starts again.
	EffectReveal
	// EffectSparkle lights a few characters at random and lets them fade.
	EffectSparkle
	// EffectScan is a shimmer inverted: a dim band travelling over a bright
	// label.
	EffectScan
)

// spinnerDef is one spinner: its frames, and how to walk them.
type spinnerDef struct {
	frames []string
	ascii  []string
	// sine walks the frames up and down instead of round and round, for
	// spinners whose frames are a scale rather than a cycle.
	sine bool
	// ramp colours the frame along the busy ramp by its place in the scale, so
	// a taller bar is a brighter one.
	ramp bool
	// slow divides the frame rate, for spinners that look frantic at 10fps.
	slow int
}

var spinnerDefs = map[BusySpinner]spinnerDef{
	SpinnerBraille: {
		frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		ascii:  []string{"|", "/", "-", "\\"},
	},
	SpinnerBrailleDots: {
		frames: []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"},
		ascii:  []string{"*", "+", "x", "+"},
	},
	SpinnerBar: {
		frames: []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"},
		ascii:  []string{".", ":", "|", "I"},
		sine:   true,
		ramp:   true,
	},
	SpinnerColumn: {
		frames: []string{"▏", "▎", "▍", "▌", "▋", "▊", "▉", "█"},
		ascii:  []string{".", ":", "|", "I"},
		sine:   true,
		ramp:   true,
	},
	SpinnerArc: {
		frames: []string{"◜", "◠", "◝", "◞", "◡", "◟"},
		ascii:  []string{"'", "-", ".", "_", ".", "-"},
	},
	SpinnerCircle: {
		frames: []string{"◐", "◓", "◑", "◒"},
		ascii:  []string{"(", "^", ")", "v"},
		slow:   2,
	},
	SpinnerDot: {
		frames: []string{"·", "•", "●", "•"},
		ascii:  []string{".", "o", "O", "o"},
		sine:   true,
		ramp:   true,
	},
	SpinnerQuadrant: {
		frames: []string{"▖", "▘", "▝", "▗"},
		ascii:  []string{",", "'", "`", "."},
		slow:   2,
	},
	SpinnerBounce: {
		frames: []string{"⠁", "⠂", "⠄", "⠠", "⠐", "⠈"},
		ascii:  []string{"'", "-", ".", ".", "-", "'"},
	},
	SpinnerTriangle: {
		frames: []string{"◢", "◣", "◤", "◥"},
		ascii:  []string{"v", "<", "^", ">"},
		slow:   2,
	},
	SpinnerHalves: {
		frames: []string{"▌", "▀", "▐", "▄"},
		ascii:  []string{"[", "\"", "]", "_"},
		slow:   2,
	},
	SpinnerFill: {
		frames: []string{"⡀", "⡄", "⡆", "⡇", "⡏", "⡟", "⡿", "⣿"},
		ascii:  []string{".", ":", "i", "I"},
		sine:   true,
		ramp:   true,
	},
	SpinnerPipe: {
		frames: []string{"|", "/", "-", "\\"},
		ascii:  []string{"|", "/", "-", "\\"},
		slow:   2,
	},
	SpinnerBinary: {
		frames: []string{"0", "1"},
		ascii:  []string{"0", "1"},
		slow:   3,
	},
}

// randomSpinners and randomEffects are what the random settings draw from.
var (
	// SpinnerDot is absent on purpose: it belongs to tool calls.
	randomSpinners = []BusySpinner{
		SpinnerBraille, SpinnerBrailleDots, SpinnerBar, SpinnerColumn, SpinnerArc,
		SpinnerCircle, SpinnerQuadrant, SpinnerBounce, SpinnerTriangle,
		SpinnerHalves, SpinnerFill, SpinnerPipe, SpinnerBinary,
	}
	randomEffects = []BusyEffect{
		EffectNone, EffectShimmer, EffectPulse, EffectFlow,
		EffectRipple, EffectReveal, EffectSparkle, EffectScan,
	}
)

func pickSpinner(chosen, previous BusySpinner) BusySpinner {
	if chosen != SpinnerRandom {
		return chosen
	}
	return pickDifferent(randomSpinners, previous)
}

func pickEffect(chosen, previous BusyEffect) BusyEffect {
	if chosen != EffectRandom {
		return chosen
	}
	return pickDifferent(randomEffects, previous)
}

// pickDifferent draws from pool, never the same thing twice running. Turn to
// turn is the only place the change is visible, and a draw that repeats itself
// looks like nothing changed at all.
func pickDifferent[T comparable](pool []T, previous T) T {
	switch len(pool) {
	case 0:
		var zero T
		return zero
	case 1:
		return pool[0]
	}
	for {
		if v := pool[rand.IntN(len(pool))]; v != previous {
			return v
		}
	}
}

// BusyPreview renders one frame of a busy line the way a session draws it: the
// spinner, a space, and the label. It exists so an application can show the
// combinations side by side and pick one — see `john -busy-preview`.
func BusyPreview(t *Theme, spinner BusySpinner, effect BusyEffect, label string, frame int) string {
	return t.busySpinner(spinner, frame) + " " + t.busyLabel(effect, label, frame)
}

// busySpinner renders the spinner column. Always exactly one column wide.
func (t *Theme) busySpinner(spinner BusySpinner, frame int) string {
	def, ok := spinnerDefs[spinner]
	if !ok {
		def = spinnerDefs[SpinnerBraille]
	}
	frames := def.frames
	if t.asciiMode {
		frames = def.ascii
	}
	if len(frames) == 0 {
		return t.Spinner.Render("*")
	}
	if def.slow > 1 {
		frame /= def.slow
	}

	// A cycle goes round; a scale goes up and down.
	var idx int
	var heat float64
	if def.sine {
		heat = (math.Sin(float64(frame)/2.2) + 1) / 2
		idx = int(heat * float64(len(frames)-1))
	} else {
		idx = ((frame % len(frames)) + len(frames)) % len(frames)
		heat = float64(idx) / float64(max(len(frames)-1, 1))
	}

	if !def.ramp || !t.HasColor {
		return t.Spinner.Render(frames[idx])
	}
	return t.style().
		Foreground(lipgloss.Color(lerpHex(t.BusyBase, t.BusyHigh, 0.35+heat*0.65))).
		Render(frames[idx])
}

// busyLabel renders the label for the given effect and frame.
func (t *Theme) busyLabel(effect BusyEffect, label string, frame int) string {
	if label == "" {
		return ""
	}
	if !t.HasColor || effect == EffectNone {
		return t.Muted.Render(label)
	}

	switch effect {
	case EffectShimmer:
		// One bright band sweeping along, with a pause between sweeps.
		return t.perRune(label, func(i, n int) float64 {
			const band = 4.0
			head := float64(frame%(n+10)) - band
			heat := math.Max(0, 1-math.Abs(float64(i)-head)/band)
			return heat * heat // sharpen the falloff
		})

	case EffectScan:
		// The same band, inverted: a shadow passing over a lit label.
		return t.perRune(label, func(i, n int) float64 {
			const band = 3.0
			head := float64(frame%(n+8)) - band
			dip := math.Max(0, 1-math.Abs(float64(i)-head)/band)
			return 0.85 - dip*dip*0.75
		})

	case EffectRipple:
		// Several soft crests travelling at once.
		return t.perRune(label, func(i, n int) float64 {
			return (math.Sin(float64(i)/1.6-float64(frame)/2.4) + 1) / 2
		})

	case EffectFlow:
		// A ramp running through the text and wrapping around. Triangular, so
		// the wrap does not snap.
		return t.perRune(label, func(i, n int) float64 {
			f := math.Mod(float64(i)/float64(max(n-1, 1))+float64(frame)/24, 1)
			return 1 - math.Abs(f*2-1)
		})

	case EffectReveal:
		// Brightens a character at a time, holds the finished label, restarts.
		return t.perRune(label, func(i, n int) float64 {
			pos := float64(frame % (n + 12))
			if float64(i) < pos {
				return 0.9
			}
			if float64(i) < pos+1 {
				return 0.6
			}
			return 0.1
		})

	case EffectSparkle:
		// A few characters lit at a time, chosen by a hash so they twinkle
		// rather than march.
		return t.perRune(label, func(i, n int) float64 {
			h := sparkle(i, frame/2)
			if h < 0.86 {
				return 0.15
			}
			// Fade in over the two frames the sparkle lasts.
			if frame%2 == 0 {
				return 1
			}
			return 0.6
		})

	case EffectPulse:
		// Never fully dark: it should read as alive, not as broken.
		heat := (math.Sin(float64(frame)/3.2) + 1) / 2
		return t.style().
			Foreground(lipgloss.Color(lerpHex(t.BusyBase, t.BusyHigh, 0.15+heat*0.85))).
			Render(label)
	}
	return t.Muted.Render(label)
}

// perRune colours each character of the label by a heat function.
func (t *Theme) perRune(label string, heat func(i, n int) float64) string {
	runes := []rune(label)
	var b strings.Builder
	for i, r := range runes {
		h := math.Max(0, math.Min(1, heat(i, len(runes))))
		b.WriteString(t.style().
			Foreground(lipgloss.Color(lerpHex(t.BusyBase, t.BusyHigh, h))).
			Render(string(r)))
	}
	return b.String()
}

// sparkle is a cheap deterministic hash in [0,1): the same character sparkles
// on the same frame every time, which is what stops it from flickering.
func sparkle(i, step int) float64 {
	h := uint32(i*2654435761) ^ uint32(step*40503)
	h ^= h >> 13
	h *= 0x5bd1e995
	h ^= h >> 15
	return float64(h%1000) / 1000
}

// lerpHex blends two "#rrggbb" colours. Anything it cannot parse comes back as
// the first colour, so a bad theme is dull rather than broken.
func lerpHex(from, to string, t float64) string {
	if t <= 0 {
		return from
	}
	if t >= 1 {
		return to
	}
	fr, fg, fb, ok1 := parseHex(from)
	tr, tg, tb, ok2 := parseHex(to)
	if !ok1 || !ok2 {
		return from
	}
	mix := func(a, b int) int { return a + int(math.Round(float64(b-a)*t)) }
	return "#" + hex2(mix(fr, tr)) + hex2(mix(fg, tg)) + hex2(mix(fb, tb))
}

func parseHex(s string) (r, g, b int, ok bool) {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v>>16) & 0xff, int(v>>8) & 0xff, int(v) & 0xff, true
}

func hex2(v int) string {
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	const digits = "0123456789abcdef"
	return string([]byte{digits[v>>4], digits[v&0xf]})
}
