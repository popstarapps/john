package client

import (
	"fmt"
	"os"
	"time"

	"github.com/popstarapps/john/terminalui"
)

// busySpinners and busyEffects map the -spinner and -effect flags. "random",
// the default for both, lets each turn pick its own.
var busySpinners = map[string]terminalui.BusySpinner{
	"random":   terminalui.SpinnerRandom,
	"braille":  terminalui.SpinnerBraille,
	"dots":     terminalui.SpinnerBrailleDots,
	"bar":      terminalui.SpinnerBar,
	"column":   terminalui.SpinnerColumn,
	"arc":      terminalui.SpinnerArc,
	"circle":   terminalui.SpinnerCircle,
	"dot":      terminalui.SpinnerDot,
	"quadrant": terminalui.SpinnerQuadrant,
	"bounce":   terminalui.SpinnerBounce,
	"triangle": terminalui.SpinnerTriangle,
	"halves":   terminalui.SpinnerHalves,
	"fill":     terminalui.SpinnerFill,
	"pipe":     terminalui.SpinnerPipe,
	"binary":   terminalui.SpinnerBinary,
}

var busyEffects = map[string]terminalui.BusyEffect{
	"random":  terminalui.EffectRandom,
	"none":    terminalui.EffectNone,
	"shimmer": terminalui.EffectShimmer,
	"pulse":   terminalui.EffectPulse,
	"flow":    terminalui.EffectFlow,
	"ripple":  terminalui.EffectRipple,
	"reveal":  terminalui.EffectReveal,
	"sparkle": terminalui.EffectSparkle,
	"scan":    terminalui.EffectScan,
}

// previewBusy animates every spinner and every effect, so the choice can be
// made by looking at it rather than by reading about it.
func previewBusy() {
	theme := terminalui.DefaultTheme(os.Stdout)

	spinners := []struct {
		name string
		s    terminalui.BusySpinner
	}{
		{"braille", terminalui.SpinnerBraille}, {"dots", terminalui.SpinnerBrailleDots},
		{"bar", terminalui.SpinnerBar}, {"column", terminalui.SpinnerColumn},
		{"arc", terminalui.SpinnerArc}, {"circle", terminalui.SpinnerCircle},
		{"dot", terminalui.SpinnerDot}, {"quadrant", terminalui.SpinnerQuadrant},
		{"bounce", terminalui.SpinnerBounce}, {"triangle", terminalui.SpinnerTriangle},
		{"halves", terminalui.SpinnerHalves}, {"fill", terminalui.SpinnerFill},
		{"pipe", terminalui.SpinnerPipe}, {"binary", terminalui.SpinnerBinary},
	}
	effects := []struct {
		name string
		e    terminalui.BusyEffect
	}{
		{"none", terminalui.EffectNone}, {"shimmer", terminalui.EffectShimmer},
		{"pulse", terminalui.EffectPulse}, {"flow", terminalui.EffectFlow},
		{"ripple", terminalui.EffectRipple}, {"reveal", terminalui.EffectReveal},
		{"sparkle", terminalui.EffectSparkle}, {"scan", terminalui.EffectScan},
	}

	rows := 2 + (len(spinners)+3)/4 + 1 + len(effects)
	fmt.Print("\n")
	for frame := 0; frame < 240; frame++ {
		if frame > 0 {
			fmt.Printf("\x1b[%dA", rows)
		}
		fmt.Printf("\r\x1b[K  spinners\n")
		for i := 0; i < len(spinners); i += 4 {
			fmt.Print("\r\x1b[K   ")
			for _, sp := range spinners[i:min(i+4, len(spinners))] {
				fmt.Printf(" %s %-10s", terminalui.BusyPreview(theme, sp.s, terminalui.EffectNone, "", frame), sp.name)
			}
			fmt.Print("\n")
		}
		fmt.Printf("\r\x1b[K\n\r\x1b[K  effects\n")
		for _, e := range effects {
			line := terminalui.BusyPreview(theme, terminalui.SpinnerBraille, e.e, "thinking", frame)
			fmt.Printf("\r\x1b[K    %-9s %s · 22s · esc to interrupt\n", e.name, line)
		}
		time.Sleep(90 * time.Millisecond)
	}
}
