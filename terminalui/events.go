package terminalui

// Event is something the application tells the UI. Events flow in one
// direction only: application -> UI, through UI.Send.
//
// Every event type in this file is a plain value. The UI never hands one back,
// never mutates one, and never blocks the caller on rendering it, so an
// application can Send from any goroutine at any time.
type Event interface{ isEvent() }

// NoticeLevel controls how a Notice is drawn.
type NoticeLevel int

const (
	// NoticeInfo is dim, unobtrusive session chatter: sandbox resumed, model
	// selected, policy cache refreshed.
	NoticeInfo NoticeLevel = iota
	// NoticeWarn is something the developer should read but that did not stop
	// the turn.
	NoticeWarn
	// NoticeError is a failure. It survives in scrollback like everything else.
	NoticeError
)

// Notice is a standalone line in the transcript. Boot lines, policy refreshes
// and errors are all notices.
type Notice struct {
	Level NoticeLevel
	Text  string
	// Prominent is a line the developer should not miss, such as which
	// conversation they have walked back into: it is drawn in the colour of
	// what they type and set apart by a blank line, where an ordinary notice
	// is muted and stacks with the lines around it.
	Prominent bool
}

// Status replaces the state shown on the input box's edges. It is
// state, not history: it is never committed to scrollback, and each Status
// event fully replaces the previous one.
type Status struct {
	// Workdir is the project directory, shown as-is. Callers usually pass a
	// home-relative path such as "~/src/payments".
	Workdir string
	// Model is the model the backend routed this session to.
	Model string
	// Sandbox describes sandbox state, e.g. "sandbox ready" or "building image".
	Sandbox string
	// Tokens, when non-zero, is how much of the model's context the
	// conversation fills, and Window, when known, how much there is.
	Tokens int
	Window int
	// Extra is appended verbatim, for anything the application wants pinned.
	Extra string
	// Alert states that the session is degraded: the backend is unreachable,
	// the policy cache has expired, operations are blocked. It is drawn first
	// and in the alert colour, and it is the last thing dropped when the
	// terminal is too narrow — john fails closed, and the UI should say so
	// before it says anything else.
	Alert string
}

// Busy puts the UI in "the agent is working" mode: a spinner with Label
// appears above the input and Esc emits Interrupt.
type Busy struct {
	Label string
}

// BusyLabel changes what the busy line says while it is up, without
// restarting its clock: "running" while a command runs, then "thinking"
// again. Outside a Busy it does nothing.
type BusyLabel struct {
	Label string
}

// Idle ends the state started by Busy.
type Idle struct{}

// Thinking shows transient reasoning text in the live region. It is deliberately
// never committed to scrollback: the last Thinking text is replaced by the next
// one and discarded when the turn ends.
type Thinking struct {
	Text string
}

// AssistantStart opens a streaming assistant message. ID ties the deltas and
// the end together; concurrent messages with different IDs are supported and
// committed in the order they started.
type AssistantStart struct {
	ID string
}

// AssistantDelta appends to an open assistant message. Text is a fragment, not
// a line: the UI accumulates and re-wraps it.
type AssistantDelta struct {
	ID   string
	Text string
}

// AssistantEnd closes a streaming assistant message. At this point the message
// is rendered as markdown, wrapped to the current terminal width, and committed
// to scrollback.
type AssistantEnd struct {
	ID string
}

// AssistantMessage is a complete message in one event, equivalent to a Start,
// one Delta and an End.
type AssistantMessage struct {
	Text string
}

// ToolStatus is the outcome of a tool call.
type ToolStatus int

const (
	// ToolOK is a call that did what it was asked.
	ToolOK ToolStatus = iota
	// ToolFail is a call that ran and failed: a non-zero exit, a failing test.
	ToolFail
	// ToolBlocked is a call a policy refused.
	ToolBlocked
	// ToolSkipped is a call that never ran.
	ToolSkipped
)

// ToolStart opens a tool call. While it is open, it renders in the live region
// with a spinner; when ToolEnd arrives, it is committed to scrollback.
type ToolStart struct {
	// ID correlates ToolDetail and ToolEnd with this call.
	ID string
	// Verb is the short action word: read, edit, run, diff, apply, proxy,
	// search. Unknown verbs render in the default accent color.
	Verb string
	// Target is what the verb acts on: a path, a command, a URL.
	Target string
	// Targets is a list of things the verb acts on, such as the files an
	// apply writes, used in place of Target. They are shown whole, as many as
	// fit, and then "+N more": joined into one Target, the line was cut
	// wherever it ran out of room, and "src/main.ts" came out as "sr…".
	Targets []string
	// Meta is dim trailing detail, e.g. "+34 -6".
	Meta string
}

// ToolDetail appends a child line under an open tool call. Details are one line
// each and are truncated to the terminal width.
type ToolDetail struct {
	ID   string
	Text string
}

// ToolEnd closes a tool call and commits it to scrollback.
type ToolEnd struct {
	ID     string
	Status ToolStatus
	// Meta replaces the Meta given in ToolStart when non-empty.
	Meta string
}

// Progress reports long, staged work: provisioning a sandbox, building its
// image, pulling layers. Send it repeatedly with the same ID; each one replaces
// the last. It renders as a live block with a bar and a running clock, and
// commits to a single transcript line when Done.
//
// It exists because the first run of john in a project is minutes of dense
// output, and a spinner that says nothing for four minutes is indistinguishable
// from a hang.
type Progress struct {
	// ID identifies this piece of work across updates.
	ID string
	// Label is what is being done, e.g. "building sandbox image".
	Label string
	// Step and Steps render as "step 3/12" when Steps is non-zero.
	Step, Steps int
	// Percent draws a bar when it is 1..100. Zero draws none: work that cannot
	// say how far along it is should not pretend.
	Percent int
	// Detail is the current line of output. It replaces the previous one
	// rather than accumulating.
	Detail string
	// Done finishes the block and commits it.
	Done bool
	// Status is the outcome, once Done.
	Status ToolStatus
	// Meta is dim trailing detail on the committed line, e.g. "cached · 4m saved".
	Meta string
}

// ProxyDecision reports a network proxy verdict that needed no interaction. It
// is sugar for a complete tool call with the "proxy" verb.
type ProxyDecision struct {
	Method  string
	URL     string
	Verdict string // "always allow", "blocked · no policy", ...
	Allowed bool
	Error   string // an allowed request that failed all the same, and why
}

// Header is one HTTP header in an ApprovalRequest.
type Header struct {
	Name  string
	Value string
}

// Choice is one option offered by an interactive box.
type Choice struct {
	// Key is the single character that picks this option.
	Key rune
	// Label is shown on the button, e.g. "allow once".
	Label string
	// Decision is echoed back in the answering action. The UI does not
	// interpret it.
	Decision string
	// Danger renders the option in the warning color.
	Danger bool
	// Cancel marks the safe way out: the cursor starts on it, and Esc and
	// Ctrl+C pick it. Exactly one choice should have it.
	Cancel bool
	// Default puts the cursor here instead, so Enter picks it; Esc and
	// Ctrl+C still pick Cancel. For a question whose usual answer commits
	// nothing dangerous — asking for a policy is not granting one.
	Default bool
}

// ApprovalOption is the name Choice goes by in an ApprovalRequest.
type ApprovalOption = Choice

// ApprovalRequest asks the developer to decide on a buffered proxy request. It
// opens the approval inspector: a scrollable view of the full request pinned
// above the input box.
//
// Exactly one ApprovalDecision or CancelApproval follows every ApprovalRequest.
type ApprovalRequest struct {
	ID      string
	Method  string
	URL     string
	Headers []Header
	Body    string
	// BodyTruncated marks that Body is only the head of the real body.
	BodyTruncated bool
	// Verdict is the policy engine's classification, e.g. "ask developer".
	Verdict string
	// Policy explains the verdict, e.g. "policy group staging-writes".
	Policy string
	// Options are the choices offered. When empty, DefaultApprovalOptions is
	// used.
	Options []ApprovalOption
}

// Confirm asks a plain yes/no question in the same box the proxy uses for an
// approval. It is for decisions that are irreversible in a way no policy covers
// — resetting a sandbox with unapplied work in it, overwriting files the
// developer has open.
//
// Exactly one ConfirmDecision or CancelConfirm follows every Confirm.
type Confirm struct {
	ID string
	// Title is the label in the box's title row, e.g. "DISCARD SANDBOX".
	Title string
	// Question is the question itself.
	Question string
	// Detail is optional supporting context, one entry per line.
	Detail []string
	// Choices are the options offered. When empty, DefaultConfirmChoices is
	// used.
	Choices []Choice
}

// CancelConfirm withdraws a pending Confirm.
type CancelConfirm struct {
	ID     string
	Reason string
}

// CancelApproval withdraws a pending ApprovalRequest, for instance because the
// upstream request timed out. No ApprovalDecision is emitted for it.
type CancelApproval struct {
	ID     string
	Reason string
}

// DiffLineKind classifies a line inside a hunk.
type DiffLineKind int

const (
	// DiffContext is an unchanged line.
	DiffContext DiffLineKind = iota
	// DiffAdd is an added line.
	DiffAdd
	// DiffRemove is a removed line.
	DiffRemove
	// DiffMeta is a file-level marker such as "Binary files differ".
	DiffMeta
)

// DiffLine is one line inside a hunk. Text excludes the leading +/-/space.
type DiffLine struct {
	Kind DiffLineKind
	Text string
}

// DiffHunk is a contiguous run of changes.
type DiffHunk struct {
	// Header is the @@ line, without trailing context.
	Header string
	Lines  []DiffLine
}

// DiffFile is one changed path in a review.
type DiffFile struct {
	Path    string
	Added   int
	Removed int
	// Binary marks a file whose contents are not shown.
	Binary bool
	// Selected is the initial checkbox state.
	Selected bool
	Hunks    []DiffHunk
}

// DiffReview opens the diff review viewer: the file list with expandable hunks,
// per-path selection, and an apply/cancel decision.
//
// Exactly one DiffDecision or CancelDiffReview follows every DiffReview.
type DiffReview struct {
	ID string
	// Title heads the viewer, e.g. "apply to ~/src/payments".
	Title string
	Files []DiffFile
}

// CancelDiffReview withdraws a pending DiffReview.
type CancelDiffReview struct {
	ID     string
	Reason string
}

// ModelOption is one model a ModelChoice offers.
type ModelOption struct {
	// ID is echoed back in the answering ModelDecision. The UI does not
	// interpret it.
	ID string
	// Name is what the developer sees, e.g. "internal/codegen-l".
	Name string
	// Detail is dim trailing text: the provider, a context window, why this
	// one survived the narrowing.
	Detail string
	// Internal marks a model hosted inside the organization. It is drawn in
	// the ok colour, because on this particular decision that is the
	// reassuring answer.
	Internal bool
}

// ModelChoice asks which model the session continues on, and says why it is
// being asked.
//
// It exists for one moment in john: a routing policy's keyword has been seen
// somewhere in the session — the prompt, a file the agent read, command
// output, a diff — and the models the session may use have been narrowed to
// what that policy permits. A session never un-narrows, so this is not a
// setting the developer can widen again afterwards.
//
// Exactly one ModelDecision or CancelModelChoice follows every ModelChoice.
type ModelChoice struct {
	ID string
	// Title is the label in the box's title row. Defaults to "CONTINUE ON".
	Title string
	// Detail rides beside the title, e.g. the policy group's name.
	Detail string
	// Reason states what narrowed the session, e.g.
	// `keyword "C++" seen in billing/native/bridge.cc`.
	Reason string
	// Policy names the rule that acted, e.g.
	// "narrowed by policy group payments-restricted".
	Policy string
	// Models are what is left to choose from. A ModelChoice with none of them
	// is a bug in the application: there is nothing to ask.
	Models []ModelOption
	// Selected is the ModelOption.ID the cursor starts on, e.g. the model the
	// session is on now. Empty starts on the first.
	Selected string
	// Preference marks a choice the developer asked for (/model) rather
	// than one a narrowing forced: Enter switches and Esc keeps the current
	// model, and the box writes nothing to the transcript when it closes,
	// because the application reports what the backend made of it.
	Preference bool
}

// CancelModelChoice withdraws a pending ModelChoice. No ModelDecision is
// emitted for it.
type CancelModelChoice struct {
	ID     string
	Reason string
}

// Completion is one candidate in a completion popup.
type Completion struct {
	// Value is inserted into the input.
	Value string
	// Display overrides Value in the popup when non-empty.
	Display string
	// Detail is dim trailing text, e.g. a file size or a command description.
	Detail string
}

// Completions answers a CompletionRequest. Token must match the request's
// token; stale answers are dropped.
type Completions struct {
	Token uint64
	Items []Completion
}

// Quit asks the UI to shut down. Run returns and the actions channel closes.
type Quit struct{}

func (Notice) isEvent()            {}
func (Status) isEvent()            {}
func (Busy) isEvent()              {}
func (BusyLabel) isEvent()         {}
func (Idle) isEvent()              {}
func (Thinking) isEvent()          {}
func (AssistantStart) isEvent()    {}
func (AssistantDelta) isEvent()    {}
func (AssistantEnd) isEvent()      {}
func (AssistantMessage) isEvent()  {}
func (ToolStart) isEvent()         {}
func (ToolDetail) isEvent()        {}
func (ToolEnd) isEvent()           {}
func (Progress) isEvent()          {}
func (ProxyDecision) isEvent()     {}
func (ApprovalRequest) isEvent()   {}
func (CancelApproval) isEvent()    {}
func (Confirm) isEvent()           {}
func (CancelConfirm) isEvent()     {}
func (DiffReview) isEvent()        {}
func (CancelDiffReview) isEvent()  {}
func (ModelChoice) isEvent()       {}
func (CancelModelChoice) isEvent() {}
func (Completions) isEvent()       {}
func (Quit) isEvent()              {}
