package terminalui

// Action is something the UI tells the application. Actions flow in one
// direction only: UI -> application, through the channel returned by
// UI.Actions.
//
// The channel is closed when Run returns, so `for a := range ui.Actions()` is
// the intended way to drive an application loop.
type Action interface{ isAction() }

// UserPrompt is a line (or block) the developer submitted.
type UserPrompt struct {
	Text string
}

// SlashCommand is a submitted line that began with "/". The UI parses the name
// and arguments but assigns no meaning to either: every command is the
// application's to implement, including /quit.
type SlashCommand struct {
	// Name is the command without its leading slash.
	Name string
	// Args is the rest of the line, trimmed.
	Args string
	// Raw is the full submitted line.
	Raw string
}

// Interrupt is Esc or Ctrl+C pressed while the UI is busy. The application is
// expected to stop the current turn and send Idle.
type Interrupt struct{}

// ApprovalDecision answers an ApprovalRequest.
type ApprovalDecision struct {
	ID string
	// Decision is the chosen ApprovalOption.Decision, or DecisionReject when
	// the developer dismissed the inspector or the UI is non-interactive.
	Decision string
	// Reason is set when the UI decided on its own behalf, e.g.
	// "no tty: rejected by policy NonInteractiveReject".
	Reason string
}

// Standard decision strings used by DefaultApprovalOptions and by the
// non-interactive renderer. Applications may define their own instead.
const (
	DecisionAllowOnce = "allow-once"
	DecisionReject    = "reject"

	// Standard answers for a Confirm.
	DecisionYes = "yes"
	DecisionNo  = "no"
)

// ConfirmDecision answers a Confirm.
type ConfirmDecision struct {
	ID string
	// Decision is the chosen Choice.Decision, or the cancelling choice when
	// the developer dismissed the box or the UI is non-interactive.
	Decision string
	// Reason is set when the UI decided on its own behalf.
	Reason string
}

// DiffDecision answers a DiffReview.
type DiffDecision struct {
	ID string
	// Apply is false when the developer cancelled.
	Apply bool
	// Paths are the selected files, in the order they were presented. Empty
	// when Apply is false.
	Paths []string
}

// ModelDecision answers a ModelChoice.
type ModelDecision struct {
	ID string
	// ModelID is the chosen ModelOption.ID. It is empty when Cancelled.
	ModelID string
	// Cancelled means the developer declined to pick one. There is no safe
	// default to fall back on — the narrowing has already happened — so the
	// application should abandon the turn rather than choose for them.
	Cancelled bool
	// Reason is set when the UI decided on its own behalf, e.g.
	// "no tty: no model chosen".
	Reason string
}

// CompletionKind says what a CompletionRequest is completing.
type CompletionKind int

const (
	// CompletionFile is an @-mention of a workspace path.
	CompletionFile CompletionKind = iota
	// CompletionSlash is a slash command. The UI answers these itself from
	// Options.Commands and only asks the application when
	// Options.CompleteSlashCommands is set.
	CompletionSlash
)

// CompletionRequest asks the application for completion candidates. The
// application answers with a Completions event carrying the same Token. It is
// free to answer late, or not at all; the UI drops answers whose token no
// longer matches what is typed.
type CompletionRequest struct {
	Token uint64
	Kind  CompletionKind
	// Query is the text after the trigger character.
	Query string
}

func (UserPrompt) isAction()        {}
func (SlashCommand) isAction()      {}
func (Interrupt) isAction()         {}
func (ApprovalDecision) isAction()  {}
func (ConfirmDecision) isAction()   {}
func (DiffDecision) isAction()      {}
func (ModelDecision) isAction()     {}
func (CompletionRequest) isAction() {}
