package terminalui

import (
	"io"
	"os"
	"time"
)

// Mode selects the renderer.
type Mode int

const (
	// ModeAuto uses the interactive renderer when the output is a terminal and
	// the plain renderer otherwise. This is the default.
	ModeAuto Mode = iota
	// ModeInteractive always uses the interactive renderer. Useful in tests
	// with a pty, and to override a bad TTY detection.
	ModeInteractive
	// ModePlain always uses the plain, line-oriented renderer: no live region,
	// no input, no cursor movement.
	ModePlain
)

// NonInteractivePolicy decides what the plain renderer does when the
// application asks a question it cannot ask.
type NonInteractivePolicy int

const (
	// NonInteractiveReject answers approvals with DecisionReject and diff
	// reviews with Apply=false, each carrying a Reason. This is the default,
	// and it matches John's fail-closed posture.
	NonInteractiveReject NonInteractivePolicy = iota
	// NonInteractiveError makes Run return ErrNotInteractive when an
	// interactive decision is required.
	NonInteractiveError
)

// PromptQueueMode decides what happens when the developer sends a prompt while
// the agent is still working.
type PromptQueueMode int

const (
	// QueueWhileBusy holds prompts submitted during a turn and releases them
	// one at a time, each time the application goes Idle. The developer can
	// keep typing without interrupting the work or racing it. This is the
	// default.
	QueueWhileBusy PromptQueueMode = iota
	// DeliverImmediately emits every prompt as soon as it is submitted, busy
	// or not, and leaves the consequences to the application.
	DeliverImmediately
)

// BellMode decides when the UI rings the terminal bell as a turn ends.
type BellMode int

const (
	// BellWhenUnfocused rings only when the terminal does not have focus and
	// the turn took long enough to have walked away from. This is the default.
	BellWhenUnfocused BellMode = iota
	// BellNever never rings.
	BellNever
	// BellAlways rings at the end of every turn that took longer than
	// BellAfter, focused or not.
	BellAlways
)

// Command is a slash command offered in the completion popup. The UI only
// completes commands; running them is the application's job, via the
// SlashCommand action.
type Command struct {
	// Name is the command without its leading slash.
	Name string
	// Args is a short usage hint, e.g. "<path>".
	Args string
	// Description is one line of help.
	Description string
}

// Options configures a UI. The zero value is usable: it drives a session on
// stdin/stdout with the default theme.
type Options struct {
	// Input is the key source. Defaults to os.Stdin.
	Input io.Reader
	// Output is where the UI draws. Defaults to os.Stdout.
	Output io.Writer

	// Mode selects the renderer. Defaults to ModeAuto.
	Mode Mode
	// NonInteractive decides how the plain renderer answers interactive
	// events.
	NonInteractive NonInteractivePolicy

	// Placeholder is the hint shown in an empty input box.
	Placeholder string
	// BusyPlaceholder replaces it while the agent is working and prompts are
	// being queued, so the box says what Enter will actually do. Ignored under
	// DeliverImmediately.
	BusyPlaceholder string
	// Commands are the slash commands offered by the completion popup.
	Commands []Command
	// CompleteSlashCommands sends CompletionRequest for slash commands as well,
	// so an application can offer dynamic ones. Static Commands are always
	// included.
	CompleteSlashCommands bool
	// FileCompletion enables @-mentions, which emit CompletionRequest with
	// CompletionFile. Without it, "@" is an ordinary character.
	FileCompletion bool

	// PromptQueue decides what happens to a prompt submitted mid-turn.
	// Slash commands are never queued: they are instructions to the
	// application, not messages to the model, and things like /quit have to
	// work while the agent is busy.
	PromptQueue PromptQueueMode

	// BusySpinner is the one-character animation beside the busy label, and
	// BusyEffect is what happens to the label itself. They are chosen
	// independently, and both default to picking at random for each turn.
	// Effects fall back to plain text on a terminal without colour.
	BusySpinner BusySpinner
	BusyEffect  BusyEffect

	// Bell decides when the terminal bell rings as a turn ends. Agent turns
	// run for minutes; the developer is usually somewhere else by then.
	Bell BellMode
	// BellAfter is how long a turn must have run to be worth ringing about.
	// Defaults to 10s.
	BellAfter time.Duration

	// PasteLines is how many lines a paste must have before it collapses to a
	// placeholder in the input box. Defaults to 4. The full text is still what
	// gets sent.
	PasteLines int

	// History seeds the up/down prompt history, oldest first.
	History []string
	// HistoryLimit caps the in-memory history. Defaults to 500.
	HistoryLimit int

	// Theme overrides the default theme. Nil builds DefaultTheme(Output).
	Theme *Theme
	// ASCII forces the ASCII glyph set.
	ASCII bool
	// PlainDiff turns off syntax highlighting in the diff viewer, leaving the
	// green/red of added and removed lines.
	PlainDiff bool
	// Logo opens the session with the john logo. It is drawn once, into
	// scrollback, above everything else — and never in the plain renderer,
	// where the output is a log rather than a session.
	Logo bool
	// Markdown renders assistant messages as markdown. Defaults to true; set
	// PlainAssistantText to turn it off.
	PlainAssistantText bool

	// MaxModalPercent caps how much of the terminal height a modal view (the
	// approval inspector, the diff viewer) may take. Defaults to 60, clamped
	// to [20, 90].
	MaxModalPercent int
	// MaxLiveLines caps how many lines of in-flight transcript (running tools,
	// a streaming message) stay in the live region before older ones scroll
	// out of it. Defaults to 12.
	MaxLiveLines int

	// ActionBuffer is the capacity of the actions channel. Defaults to 64.
	ActionBuffer int
}

func (o *Options) applyDefaults() {
	if o.Input == nil {
		o.Input = os.Stdin
	}
	if o.Output == nil {
		o.Output = os.Stdout
	}
	if o.Placeholder == "" {
		o.Placeholder = "ask john to do something…"
	}
	if o.BusyPlaceholder == "" {
		o.BusyPlaceholder = "queue a follow-up…"
	}
	if o.BellAfter <= 0 {
		o.BellAfter = 10 * time.Second
	}
	if o.PasteLines <= 0 {
		o.PasteLines = 4
	}
	if o.HistoryLimit <= 0 {
		o.HistoryLimit = 500
	}
	if o.MaxModalPercent == 0 {
		o.MaxModalPercent = 60
	}
	if o.MaxModalPercent < 20 {
		o.MaxModalPercent = 20
	}
	if o.MaxModalPercent > 90 {
		o.MaxModalPercent = 90
	}
	if o.MaxLiveLines <= 0 {
		o.MaxLiveLines = 12
	}
	if o.ActionBuffer <= 0 {
		o.ActionBuffer = 64
	}
	// The theme is deliberately NOT built here. Building one asks the terminal
	// for its background colour, and New must not touch the terminal: the
	// answer would arrive while the application is still starting up and be
	// read as keystrokes. Run builds it, once it owns the terminal.
	if o.Theme != nil && o.ASCII {
		o.Theme.SetASCII()
	}
}

// DefaultApprovalOptions are used when an ApprovalRequest carries no Options.
//
// The box asks about the request in front of the developer and nothing else:
// allow this one, or refuse it. Asking for a rule that would cover future
// requests is a different question with a different answer, so it belongs in a
// different box.
func DefaultApprovalOptions() []Choice {
	return []Choice{
		{Key: 'a', Label: "allow once", Decision: DecisionAllowOnce},
		{Key: 'r', Label: "reject", Decision: DecisionReject, Danger: true, Cancel: true},
	}
}

// DefaultConfirmChoices are used when a Confirm carries no Choices. Declining
// is the cancelling choice, so the box opens on it.
func DefaultConfirmChoices() []Choice {
	return []Choice{
		{Key: 'y', Label: "yes", Decision: DecisionYes},
		{Key: 'n', Label: "no", Decision: DecisionNo, Cancel: true},
	}
}
