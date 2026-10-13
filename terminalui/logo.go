package terminalui

// The john badge treats the logo as a compact terminal window. Only the shell
// prompt and the full stop are green; the frame uses the terminal's own
// foreground, because navy on a dark terminal is a rumour.

const logoWidth = 15

// LogoArt renders the john logo for a terminal of the given width. Below the
// width it needs, it degrades to a single line rather than wrapping into
// nonsense.
//
// Applications can draw it themselves — `john --version`, say — or set
// Options.Logo and let the session open with it.
func LogoArt(t *Theme, width int) []string {
	name := t.Logo.Render("john") + t.LogoAccent.Render(".")
	if width < logoWidth+2 {
		return []string{" " + name}
	}

	top, side, bottom := "╭─────────────╮", "│", "╰─────────────╯"
	if t.Symbols.User == ASCIISymbols().User {
		top, side, bottom = "+-------------+", "|", "+-------------+"
	}

	return []string{
		" " + t.LogoMark.Render(top),
		" " + t.LogoMark.Render(side+"  ") +
			t.LogoAccent.Render(">_") +
			t.LogoMark.Render("  ") +
			t.Logo.Render("john") +
			t.LogoAccent.Render(".") +
			t.LogoMark.Render("  "+side),
		" " + t.LogoMark.Render(bottom),
	}
}
