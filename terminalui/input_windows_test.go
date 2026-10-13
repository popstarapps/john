//go:build windows

package terminalui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erikgeiser/coninput"
)

// The records a console actually produced when "a—b😀c" was written to it
// through ConPTY, the way Windows Terminal delivers typed and pasted text: the
// em dash and both halves of the emoji each arrive as an Alt and keypad
// sequence, with the character on the release of Alt.
func TestConsoleKeysKeepTextTypedAsAnAltSequence(t *testing.T) {
	down := func(vk coninput.VirtualKeyCode, ch rune, state coninput.ControlKeyState) coninput.KeyEventRecord {
		return coninput.KeyEventRecord{KeyDown: true, RepeatCount: 1, VirtualKeyCode: vk, Char: ch, ControlKeyState: state}
	}
	up := func(vk coninput.VirtualKeyCode, ch rune, state coninput.ControlKeyState) coninput.KeyEventRecord {
		return coninput.KeyEventRecord{RepeatCount: 1, VirtualKeyCode: vk, Char: ch, ControlKeyState: state}
	}
	alt := coninput.LEFT_ALT_PRESSED
	altSequence := func(digits []coninput.VirtualKeyCode, ch rune) []coninput.KeyEventRecord {
		recs := []coninput.KeyEventRecord{down(coninput.VK_MENU, 0, alt)}
		for _, d := range digits {
			recs = append(recs, down(d, 0, alt), up(d, 0, alt))
		}
		return append(recs, up(coninput.VK_MENU, ch, 0))
	}
	var recs []coninput.KeyEventRecord
	recs = append(recs, down(0x41, 'a', 0), up(0x41, 'a', 0))
	recs = append(recs, altSequence([]coninput.VirtualKeyCode{0x64, 0x65}, '—')...)
	recs = append(recs, down(0x42, 'b', 0), up(0x42, 'b', 0))
	recs = append(recs, altSequence([]coninput.VirtualKeyCode{0x66, 0x63}, 0xD83D)...)
	recs = append(recs, altSequence([]coninput.VirtualKeyCode{0x66, 0x63}, 0xDE00)...)
	recs = append(recs, down(0x43, 'c', 0), up(0x43, 'c', 0))
	recs = append(recs, down(coninput.VK_RETURN, '\r', 0))

	c := &consoleKeys{}
	var got string
	var enter bool
	for _, r := range recs {
		for _, msg := range c.translate(r) {
			k := msg.(tea.KeyMsg)
			switch k.Type {
			case tea.KeyRunes:
				got += string(k.Runes)
			case tea.KeyEnter:
				enter = true
			default:
				t.Errorf("unexpected key %v", k)
			}
		}
	}
	if got != "a—b😀c" || !enter {
		t.Fatalf("typed %q (enter %v), want %q and enter", got, enter, "a—b😀c")
	}
}

// Everything else reads as Bubble Tea read it.
func TestConsoleKeysMapKeysAsBubbleTeaDoes(t *testing.T) {
	ctrl := coninput.LEFT_CTRL_PRESSED
	for _, tc := range []struct {
		rec  coninput.KeyEventRecord
		want tea.KeyType
	}{
		{coninput.KeyEventRecord{VirtualKeyCode: 0x43, Char: 0x03, ControlKeyState: ctrl}, tea.KeyCtrlC},
		{coninput.KeyEventRecord{VirtualKeyCode: coninput.VK_UP}, tea.KeyUp},
		{coninput.KeyEventRecord{VirtualKeyCode: coninput.VK_LEFT, ControlKeyState: ctrl}, tea.KeyCtrlLeft},
		{coninput.KeyEventRecord{VirtualKeyCode: coninput.VK_TAB, ControlKeyState: coninput.SHIFT_PRESSED}, tea.KeyShiftTab},
		{coninput.KeyEventRecord{VirtualKeyCode: coninput.VK_BACK, Char: '\b'}, tea.KeyBackspace},
		{coninput.KeyEventRecord{VirtualKeyCode: coninput.VK_ESCAPE, Char: 0x1b}, tea.KeyEscape},
		{coninput.KeyEventRecord{VirtualKeyCode: coninput.VK_F5}, tea.KeyF5},
		{coninput.KeyEventRecord{VirtualKeyCode: coninput.VK_F12}, tea.KeyF12},
		// AltGr is Ctrl and right Alt, and makes a character: "@" on a German
		// keyboard.
		{coninput.KeyEventRecord{VirtualKeyCode: 0x51, Char: '@',
			ControlKeyState: ctrl | coninput.RIGHT_ALT_PRESSED}, tea.KeyRunes},
	} {
		tc.rec.KeyDown, tc.rec.RepeatCount = true, 1
		msgs := (&consoleKeys{}).translate(tc.rec)
		if len(msgs) != 1 || msgs[0].(tea.KeyMsg).Type != tc.want {
			t.Errorf("%v: got %v, want %v", tc.rec, msgs, tc.want)
		}
	}
	// A pasted newline, as the console delivers it — Enter with "\n" and Ctrl
	// held — and Shift+Enter, are newlines; a typed Enter is still Enter.
	for name, rec := range map[string]coninput.KeyEventRecord{
		"pasted newline": {VirtualKeyCode: coninput.VK_RETURN, Char: '\n', ControlKeyState: ctrl},
		"shift+enter":    {VirtualKeyCode: coninput.VK_RETURN, Char: '\r', ControlKeyState: coninput.SHIFT_PRESSED},
	} {
		rec.KeyDown, rec.RepeatCount = true, 1
		if msgs := (&consoleKeys{}).translate(rec); len(msgs) != 1 || msgs[0].(tea.KeyMsg).Type != tea.KeyCtrlJ {
			t.Errorf("%s: got %v, want a newline (ctrl+j)", name, msgs)
		}
	}
	enter := coninput.KeyEventRecord{KeyDown: true, RepeatCount: 1, VirtualKeyCode: coninput.VK_RETURN, Char: '\r'}
	if msgs := (&consoleKeys{}).translate(enter); len(msgs) != 1 || msgs[0].(tea.KeyMsg).Type != tea.KeyEnter {
		t.Errorf("enter: got %v, want enter", msgs)
	}

	// A modifier pressed on its own types nothing.
	for _, vk := range []coninput.VirtualKeyCode{coninput.VK_SHIFT, coninput.VK_CONTROL, coninput.VK_MENU} {
		if msgs := (&consoleKeys{}).translate(coninput.KeyEventRecord{KeyDown: true, RepeatCount: 1,
			VirtualKeyCode: vk}); len(msgs) != 0 {
			t.Errorf("modifier %v typed %v", vk, msgs)
		}
	}
}
