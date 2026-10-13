//go:build windows

package terminalui

import (
	"os"

	"golang.org/x/sys/windows"
)

// cpUTF8 is the Windows code page for UTF-8.
const cpUTF8 = 65001

// prepareConsole puts the Windows console into a state this package can draw
// in, and returns a function that puts it back.
//
// Bubble Tea already enables virtual terminal processing, which is what makes
// escape sequences work at all. What nothing enables is the UTF-8 output code
// page: the console decodes whatever bytes it is given using the process code
// page, which on a default installation is 437 or 850, so every box-drawing
// character, glyph and spinner frame arrives as mojibake. Setting it is the
// difference between a drawn box and a screen of Ãs.
//
// ok reports whether the console can be trusted with UTF-8 at all. When it
// cannot, the caller falls back to the ASCII glyph set rather than drawing
// something unreadable.
func prepareConsole(out any) (restore func(), ok bool) {
	noop := func() {}
	if _, isFile := out.(*os.File); !isFile {
		return noop, true
	}
	prev, err := windows.GetConsoleOutputCP()
	if err != nil {
		// No console attached (a pipe, a service). The bytes are ours.
		return noop, true
	}
	if prev == cpUTF8 {
		return noop, true
	}
	if err := windows.SetConsoleOutputCP(cpUTF8); err != nil {
		return noop, false
	}
	return func() { _ = windows.SetConsoleOutputCP(prev) }, true
}

// legacyConsole reports whether this looks like the old console host rather
// than a modern terminal. Windows Terminal, ConEmu, VS Code and the ports of
// unix terminals all announce themselves; conhost with a raster or Consolas
// font announces nothing, and lacks the glyphs for ❯, ⏺ and braille spinners.
//
// It is a heuristic, and it only ever downgrades the built-in theme: an
// application that sets Options.Theme explicitly gets exactly what it asked
// for.
func legacyConsole() bool {
	for _, key := range []string{"WT_SESSION", "TERM_PROGRAM", "ConEmuANSI", "ANSICON", "ALACRITTY_WINDOW_ID", "WEZTERM_EXECUTABLE"} {
		if os.Getenv(key) != "" {
			return false
		}
	}
	if t := os.Getenv("TERM"); t != "" && t != "dumb" {
		return false
	}
	return true
}
