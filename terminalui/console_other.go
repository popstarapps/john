//go:build !windows

package terminalui

import tea "github.com/charmbracelet/bubbletea"

// prepareConsole is a Windows concern: every other platform's terminal is
// already a UTF-8 byte stream.
func prepareConsole(any) (restore func(), ok bool) { return func() {}, true }

// legacyConsole is likewise Windows-only.
func legacyConsole() bool { return false }

// consoleKeys is the Windows console's key reader. Everywhere else the
// terminal sends bytes, and Bubble Tea reads them.
type consoleKeys struct{}

func newConsoleKeys(in, out any) (*consoleKeys, bool) { return nil, false }
func (*consoleKeys) run(func(tea.Msg))                {}
func (*consoleKeys) close()                           {}
