//go:build windows

package terminalui

import (
	"os"
	"unicode/utf16"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/erikgeiser/coninput"
	"golang.org/x/sys/windows"
)

// A Windows console delivers keys as records rather than bytes, and Bubble Tea
// reads them itself — keeping each key-down's character and dropping every
// key-up. That loses text. A character the keyboard has no key for arrives the
// way Alt and the numeric keypad would type it: Alt down, the digits, and the
// character itself on the release of Alt. Windows Terminal and every other
// ConPTY host send typed or pasted text that way for anything outside the
// keyboard's layout, so "yet — Node" reached the model as "yet  Node". And a
// character beyond the first 65,536 comes as two records, each holding half
// of a UTF-16 surrogate pair, which Bubble Tea passes on as two broken runes.
//
// So on a console john reads the records itself, and Bubble Tea is given none.
// Bytes are no way around it: the byte reader Bubble Tea would use instead
// decodes everything above ASCII as Latin-1.

// consoleKeys reads a Windows console's input records into Bubble Tea
// messages.
type consoleKeys struct {
	in   windows.Handle
	out  any
	stop windows.Handle // set by close; run waits on it beside the console
	done chan struct{}  // closed when run has returned
	mode uint32

	high     rune // the first half of a surrogate pair, waiting for its second
	lastSize [2]int
}

// newConsoleKeys takes over input when in is a console, putting it in the
// raw mode Bubble Tea would have, and reports false when it is not one.
func newConsoleKeys(in, out any) (*consoleKeys, bool) {
	f, ok := in.(*os.File)
	if !ok {
		return nil, false
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return nil, false
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, false
	}
	// Keys as records, nothing echoed or interpreted — Ctrl+C is a key like
	// any other — and a record when the window changes size.
	raw := mode&^(windows.ENABLE_LINE_INPUT|windows.ENABLE_ECHO_INPUT|
		windows.ENABLE_PROCESSED_INPUT|windows.ENABLE_VIRTUAL_TERMINAL_INPUT) |
		windows.ENABLE_WINDOW_INPUT
	if windows.SetConsoleMode(h, raw) != nil {
		windows.CloseHandle(stop)
		return nil, false
	}
	return &consoleKeys{in: h, out: out, stop: stop, done: make(chan struct{}), mode: mode}, true
}

// run sends what is typed until close.
func (c *consoleKeys) run(send func(tea.Msg)) {
	defer close(c.done)
	for {
		r, err := windows.WaitForMultipleObjects([]windows.Handle{c.stop, c.in}, false, windows.INFINITE)
		if err != nil || r == windows.WAIT_OBJECT_0 {
			return
		}
		records, err := coninput.ReadNConsoleInputs(c.in, 32)
		if err != nil {
			return
		}
		for _, rec := range records {
			for _, msg := range c.translate(rec.Unwrap()) {
				send(msg)
			}
		}
	}
}

// close stops run and gives the console back the mode it had. The event is
// let go of only once run is no longer waiting on it.
func (c *consoleKeys) close() {
	windows.SetEvent(c.stop)
	<-c.done
	windows.SetConsoleMode(c.in, c.mode)
	windows.CloseHandle(c.stop)
}

// translate turns one record into what Bubble Tea would have made of it, plus
// the text Bubble Tea would have dropped.
func (c *consoleKeys) translate(ev coninput.EventRecord) []tea.Msg {
	switch e := ev.(type) {
	case coninput.KeyEventRecord:
		return c.key(e)
	case coninput.WindowBufferSizeEventRecord:
		// The record carries the buffer's size; what is drawn in is the
		// window, so that is what is measured.
		w, h := terminalSize(c.out)
		if w == 0 || [2]int{w, h} == c.lastSize {
			return nil
		}
		c.lastSize = [2]int{w, h}
		return []tea.Msg{tea.WindowSizeMsg{Width: w, Height: h}}
	}
	return nil
}

func (c *consoleKeys) key(e coninput.KeyEventRecord) []tea.Msg {
	altGr := e.ControlKeyState.Contains(coninput.LEFT_CTRL_PRESSED) &&
		e.ControlKeyState.Contains(coninput.RIGHT_ALT_PRESSED)
	alt := !altGr && e.ControlKeyState.Contains(coninput.LEFT_ALT_PRESSED|coninput.RIGHT_ALT_PRESSED)

	if !e.KeyDown {
		// The one release that carries text: the end of an Alt and keypad
		// sequence, which is how a character with no key arrives.
		if e.VirtualKeyCode == coninput.VK_MENU && e.Char != 0 {
			return c.text(e.Char, 1, false)
		}
		return nil
	}
	switch e.VirtualKeyCode {
	case coninput.VK_SHIFT, coninput.VK_CONTROL, coninput.VK_MENU, coninput.VK_CAPITAL,
		coninput.VK_LWIN, coninput.VK_RWIN:
		// A modifier on its own types nothing.
		return nil
	}
	kt := keyType(e)
	if kt == tea.KeyEnter && (e.Char == '\n' || e.ControlKeyState.Contains(coninput.SHIFT_PRESSED)) {
		// A newline in pasted text arrives as Enter with "\n" where a typed
		// Enter has "\r" — the console takes the paste's own markers out —
		// and sent as Enter it submitted each pasted line as a prompt of its
		// own. Shift+Enter is the newline the input box offers, which Bubble
		// Tea's mapping made Enter too. Both are ctrl+j, a newline.
		kt = tea.KeyCtrlJ
	}
	if kt != tea.KeyRunes {
		msgs := make([]tea.Msg, 0, e.RepeatCount)
		for i := 0; i < int(e.RepeatCount); i++ {
			msgs = append(msgs, tea.KeyMsg{Type: kt, Alt: alt})
		}
		return msgs
	}
	if e.Char == 0 {
		// The keypad digits of an Alt sequence, or a key with no character
		// in this layout. Neither is text.
		return nil
	}
	return c.text(e.Char, int(e.RepeatCount), alt)
}

// text sends a character, putting surrogate halves back together.
func (c *consoleKeys) text(r rune, repeat int, alt bool) []tea.Msg {
	switch {
	case utf16.IsSurrogate(r) && r < 0xDC00:
		c.high = r
		return nil
	case utf16.IsSurrogate(r):
		if c.high == 0 {
			return nil
		}
		r, c.high = utf16.DecodeRune(c.high, r), 0
	default:
		c.high = 0
	}
	msgs := make([]tea.Msg, 0, repeat)
	for i := 0; i < repeat; i++ {
		msgs = append(msgs, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: alt})
	}
	return msgs
}

// keyType is Bubble Tea's own mapping from a key record to a key, as its
// Windows reader has it (key_windows.go, MIT licence), so that every key but
// the ones this file exists for behaves exactly as it did.
func keyType(e coninput.KeyEventRecord) tea.KeyType {
	shift := e.ControlKeyState.Contains(coninput.SHIFT_PRESSED)
	ctrl := e.ControlKeyState.Contains(coninput.LEFT_CTRL_PRESSED | coninput.RIGHT_CTRL_PRESSED)
	// The four ways each arrow-like key can be held.
	pick := func(plain, s, c, cs tea.KeyType) tea.KeyType {
		switch {
		case shift && ctrl:
			return cs
		case shift:
			return s
		case ctrl:
			return c
		}
		return plain
	}

	switch e.VirtualKeyCode {
	case coninput.VK_RETURN:
		return tea.KeyEnter
	case coninput.VK_BACK:
		return tea.KeyBackspace
	case coninput.VK_TAB:
		if shift {
			return tea.KeyShiftTab
		}
		return tea.KeyTab
	case coninput.VK_SPACE:
		return tea.KeyRunes // as on unix, where space is a rune too
	case coninput.VK_ESCAPE:
		return tea.KeyEscape
	case coninput.VK_UP:
		return pick(tea.KeyUp, tea.KeyShiftUp, tea.KeyCtrlUp, tea.KeyCtrlShiftUp)
	case coninput.VK_DOWN:
		return pick(tea.KeyDown, tea.KeyShiftDown, tea.KeyCtrlDown, tea.KeyCtrlShiftDown)
	case coninput.VK_RIGHT:
		return pick(tea.KeyRight, tea.KeyShiftRight, tea.KeyCtrlRight, tea.KeyCtrlShiftRight)
	case coninput.VK_LEFT:
		return pick(tea.KeyLeft, tea.KeyShiftLeft, tea.KeyCtrlLeft, tea.KeyCtrlShiftLeft)
	case coninput.VK_HOME:
		return pick(tea.KeyHome, tea.KeyShiftHome, tea.KeyCtrlHome, tea.KeyCtrlShiftHome)
	case coninput.VK_END:
		return pick(tea.KeyEnd, tea.KeyShiftEnd, tea.KeyCtrlEnd, tea.KeyCtrlShiftEnd)
	case coninput.VK_PRIOR:
		return tea.KeyPgUp
	case coninput.VK_NEXT:
		return tea.KeyPgDown
	case coninput.VK_DELETE:
		return tea.KeyDelete
	}
	if k := e.VirtualKeyCode; k >= coninput.VK_F1 && k <= coninput.VK_F20 {
		return functionKeys[k-coninput.VK_F1]
	}

	// AltGr is Ctrl and right Alt together, and what it makes is a character.
	if !ctrl || e.ControlKeyState.Contains(coninput.LEFT_CTRL_PRESSED) &&
		e.ControlKeyState.Contains(coninput.RIGHT_ALT_PRESSED) {
		return tea.KeyRunes
	}
	switch {
	case e.Char == '@':
		return tea.KeyCtrlAt
	case e.Char >= 0x01 && e.Char <= 0x1c, e.Char == 0x1f:
		// Ctrl and a letter is that letter's control character, and Bubble
		// Tea's ctrl keys are those characters' values.
		return tea.KeyType(e.Char)
	}
	switch e.VirtualKeyCode {
	case coninput.VK_OEM_4:
		return tea.KeyCtrlOpenBracket
	case coninput.VK_OEM_6:
		return tea.KeyCtrlCloseBracket
	}
	return tea.KeyRunes
}

var functionKeys = [...]tea.KeyType{
	tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4, tea.KeyF5, tea.KeyF6, tea.KeyF7,
	tea.KeyF8, tea.KeyF9, tea.KeyF10, tea.KeyF11, tea.KeyF12, tea.KeyF13, tea.KeyF14,
	tea.KeyF15, tea.KeyF16, tea.KeyF17, tea.KeyF18, tea.KeyF19, tea.KeyF20,
}
