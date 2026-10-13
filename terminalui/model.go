package terminalui

import (
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const (
	// 120ms: fast enough to read as motion, slow enough not to hurry the
	// developer along while they read what is above it.
	spinnerInterval = 120 * time.Millisecond
	ctrlCWindow     = 1500 * time.Millisecond
)

type eventMsg struct{ ev Event }

// flushMsg asks for a first commit, so anything queued before the session
// started (the logo) reaches the terminal even if no event ever arrives.
type flushMsg struct{}
type spinTickMsg struct{}

// quitNowMsg ends the program, one grace period after Quit was asked for. See
// quit() for why the exit is not immediate.
type quitNowMsg struct{}

// quitGrace is how long the exit yields to the renderer before going. The
// renderer draws at 60fps, so this is a couple of frames — long enough for it
// to have written out everything queued, and short enough not to be felt.
const quitGrace = 40 * time.Millisecond

// uiModel is the interactive renderer. It keeps exactly one invariant that
// everything else follows from: a block is drawn in the live region until it is
// finished, and printed into the terminal's own scrollback the moment it is —
// in order, never redrawn, never moved.
// typingGrace is the pause a box that opened under a developer's fingers
// waits for before it takes a key. See (*uiModel).typedInto.
const typingGrace = 500 * time.Millisecond

type uiModel struct {
	// lastKey is when the previous key arrived, and modalArmed whether the
	// box on screen takes keys yet: see typedInto. now is the clock, so a
	// test can be the developer's hands.
	lastKey    time.Time
	modalArmed bool
	now        func() time.Time

	opts *Options
	th   *Theme
	md   *markdownRenderer
	hl   *highlighter
	out  *actionQueue

	width, height int
	ready         bool

	pending []*block
	byID    map[string]*block
	// proxyOpen is the live counting line for each host — one for its
	// allowed requests and one for each reason its requests were blocked —
	// while nothing else has been printed since it started.
	proxyOpen map[string]*block
	anyDone   bool
	// last is the most recently committed block, which decides the spacing
	// before the next one.
	last *block
	// inTurn is set from a prompt until the turn it started goes Idle; blocks
	// pushed meanwhile are nested under the prompt.
	inTurn bool
	// altScreen is whether the terminal is on the alternate screen; see
	// syncScreen.
	altScreen bool
	// recent is the notices committed since the last prompt, the last couple
	// of lines of them, which a full-screen view repeats above itself.
	recent []string

	busy bool
	// queued holds prompts submitted while the agent was working. They are
	// released one per Idle, so the developer can think ahead without
	// interrupting the turn or racing it.
	queued    []string
	busyLabel string
	busyStart time.Time
	// busySpin and busyFx are this turn's spinner and label effect, chosen
	// when the turn starts and held until it ends.
	busySpin BusySpinner
	busyFx   BusyEffect
	thinking string

	status    Status
	spinFrame int
	ticking   bool

	approval *approvalView
	confirm  *confirmView
	diff     *diffView
	picker   *modelPickerView
	// modalQueue holds interactive requests that arrived while another one was
	// on screen. The proxy can buffer several requests at once, and every one
	// of them is owed exactly one answer, so they queue instead of replacing
	// each other.
	modalQueue []Event

	input   *promptInput
	ctrlCAt time.Time
	focused bool
	// sink is the terminal itself, for the one thing that cannot go through a
	// frame: the bell.
	sink      io.Writer
	quitting  bool
	farewell  []string // the last of the transcript, drawn as the closing frame
	pendingCT bool     // "press ctrl+c again" hint is showing

	nextToken uint64
}

func newUIModel(o *Options, out *actionQueue, md *markdownRenderer, width, height int) *uiModel {
	sink := o.Output
	m := &uiModel{
		opts:      o,
		th:        o.Theme,
		md:        md,
		hl:        diffHighlighter(o),
		out:       out,
		byID:      map[string]*block{},
		proxyOpen: map[string]*block{},
		input:     newPromptInput(o),
		focused:   true,
		sink:      sink,
	}
	if width <= 0 || height <= 0 {
		width, height = 80, 24
	}
	m.width, m.height = width, height
	m.input.setWidth(boxInnerWidth(m.frameWidth()))
	if o.Logo {
		// Queued rather than printed: whatever commits first, the logo is at
		// the head of the queue, so it cannot end up under the boot lines.
		m.push(&block{kind: kindArt, done: true, art: LogoArt(o.Theme, m.frameWidth())})
	}
	return m
}

// Init starts the cursor blink. Notably absent: EnterAltScreen. The
// transcript belongs to the terminal, not to this program.
func (m *uiModel) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, func() tea.Msg { return flushMsg{} })
}

// modalActive reports whether a modal view owns the keyboard.
func (m *uiModel) modalActive() bool {
	return m.approval != nil || m.confirm != nil || m.diff != nil || m.picker != nil
}

func (m *uiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := m.update(msg)
	return model, m.syncScreen(cmd)
}

// fullscreen reports whether the current view takes the alternate screen. The
// diff review does: it is tall, it scrolls, and while it is up the transcript
// above it is only something to scroll past.
func (m *uiModel) fullscreen() bool {
	return m.diff != nil && !m.quitting
}

// syncScreen moves the terminal in and out of the alternate screen when the
// view calls for it.
//
// Bubbletea drops anything printed while the alternate screen is up, so
// commitReady holds the transcript for as long as it is, and the flush that
// follows the exit prints what came in meanwhile. Lines printed before the
// switch are safe: the renderer keeps them queued until it is back on the main
// screen. The entry still waits for cmd to finish, so that a print already in
// flight is queued before the switch rather than racing it.
func (m *uiModel) syncScreen(cmd tea.Cmd) tea.Cmd {
	switch want := m.fullscreen(); {
	case want && !m.altScreen:
		m.altScreen = true
		return tea.Sequence(cmd, tea.EnterAltScreen)
	case !want && m.altScreen:
		m.altScreen = false
		return tea.Batch(cmd, tea.Sequence(tea.ExitAltScreen, func() tea.Msg { return flushMsg{} }))
	}
	return cmd
}

func (m *uiModel) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.ready = true
		m.input.setWidth(boxInnerWidth(m.frameWidth()))
		if m.approval != nil {
			m.approval.lines = nil
		}
		if m.diff != nil {
			m.diff.dirty = true
		}
		if m.picker != nil {
			m.picker.dirty = true
		}
		return m, nil

	case flushMsg:
		return m, m.commitReady()

	case quitNowMsg:
		return m, tea.Quit

	case spinTickMsg:
		m.spinFrame++
		if m.animating() {
			return m, m.tick()
		}
		m.ticking = false
		return m, nil

	case tea.FocusMsg:
		m.focused = true
		return m, nil

	case tea.BlurMsg:
		m.focused = false
		return m, nil

	case eventMsg:
		return m, m.handleEvent(msg.ev)

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}

	// Anything else (cursor blink, paste) belongs to the input.
	if !m.modalActive() {
		return m, m.input.update(msg)
	}
	return m, nil
}

func (m *uiModel) animating() bool {
	if m.busy {
		return true
	}
	for _, b := range m.pending {
		if (b.kind == kindTool || b.kind == kindProgress) && !b.done {
			return true
		}
	}
	return false
}

func (m *uiModel) tick() tea.Cmd {
	m.ticking = true
	return tea.Tick(spinnerInterval, func(time.Time) tea.Msg { return spinTickMsg{} })
}

func (m *uiModel) maybeTick() tea.Cmd {
	if m.ticking || !m.animating() {
		return nil
	}
	return m.tick()
}

// frameWidth is the widest a rendered line may be. It is one column short of
// the terminal on purpose.
//
// A line that fills the last column has two consequences, both bad. The
// terminal marks the row as wrapped, so a later resize re-flows it into two
// rows and everything the renderer believes about its own geometry is wrong —
// which is what leaves stale copies of the input box on screen after a resize.
// And bubbletea only appends "erase to end of line" to lines strictly narrower
// than the terminal, so a full-width line cannot clear what was under it.
func (m *uiModel) frameWidth() int {
	if m.width <= 20 {
		return 20
	}
	return m.width - 1
}

// clamp cuts every line to frameWidth, preserving styling. It is the last line
// of defence: no line reaches the terminal without passing through here.
func (m *uiModel) clamp(lines []string) []string {
	w := m.frameWidth()
	for i, l := range lines {
		if visibleWidth(l) > w {
			l = ansi.Truncate(l, w, "")
		}
		// Trailing blanks are invisible on screen but not in a selection: the
		// renderer erases to the end of every line it draws, so padding a row
		// out to the width buys nothing and costs the developer a mouthful of
		// spaces when they copy it.
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// spinner is the frame running tool calls and progress blocks show. It is the
// theme's own, never the one the busy line drew: a spinner beside a command
// means that command is running, and a spinner beside the label means the agent
// is thinking. Two different animations, because they say two different things.
func (m *uiModel) spinner() string {
	f := m.th.Symbols.Spinner
	if len(f) == 0 {
		return "*"
	}
	return f[m.spinFrame%len(f)]
}

// ---------------------------------------------------------------- events ---

func (m *uiModel) handleEvent(ev Event) tea.Cmd {
	switch e := ev.(type) {
	case Notice:
		m.push(&block{kind: kindNotice, text: e.Text, level: e.Level, prominent: e.Prominent, done: true})

	case Status:
		m.status = e

	case BusyLabel:
		if m.busy && e.Label != "" {
			m.busyLabel = e.Label
		}

	case Busy:
		m.busy = true
		m.busyLabel = e.Label
		if m.busyLabel == "" {
			m.busyLabel = "working"
		}
		m.busyStart = time.Now()
		// Never the same pair twice running: the change is only visible from
		// one turn to the next.
		m.busySpin = pickSpinner(m.opts.BusySpinner, m.busySpin)
		m.busyFx = pickEffect(m.opts.BusyEffect, m.busyFx)
		if m.opts.PromptQueue == QueueWhileBusy {
			// Say what Enter does now, in the one place that is always visible.
			m.input.setPlaceholder(m.opts.BusyPlaceholder)
		}

	case Idle:
		if m.busy && m.shouldRing() {
			m.ring()
		}
		m.busy = false
		m.thinking = ""
		m.input.setPlaceholder(m.opts.Placeholder)
		m.closeOpen()
		m.inTurn = false
		m.releaseQueued()

	case Thinking:
		m.thinking = e.Text

	case AssistantStart:
		m.push(&block{kind: kindAssistant, id: e.ID})

	case AssistantDelta:
		if b := m.byID[e.ID]; b != nil && b.kind == kindAssistant {
			b.sb.WriteString(e.Text)
		}

	case AssistantEnd:
		if b := m.byID[e.ID]; b != nil {
			b.done = true
		}

	case AssistantMessage:
		b := &block{kind: kindAssistant, done: true}
		b.sb.WriteString(e.Text)
		m.push(b)

	case ToolStart:
		m.push(&block{kind: kindTool, id: e.ID, verb: e.Verb, target: e.Target, targets: e.Targets, meta: e.Meta})

	case ToolDetail:
		if b := m.byID[e.ID]; b != nil {
			b.details = append(b.details, e.Text)
		}

	case ToolEnd:
		if b := m.byID[e.ID]; b != nil {
			b.status = e.Status
			if e.Meta != "" {
				b.meta = e.Meta
			}
			b.done = true
		}

	case Progress:
		b := m.byID[e.ID]
		if b == nil {
			b = &block{kind: kindProgress, id: e.ID, started: time.Now()}
			m.push(b)
		}
		b.target = e.Label
		b.step, b.steps, b.percent = e.Step, e.Steps, e.Percent
		b.details = nil
		if e.Detail != "" {
			b.details = []string{e.Detail}
		}
		if e.Meta != "" {
			b.meta = e.Meta
		}
		if e.Done {
			b.status, b.done = e.Status, true
		}

	case ProxyDecision:
		// Requests to a host collapse into one line that counts them: apt
		// fetching four hundred packages is one thing happening, not four
		// hundred lines burying the conversation. Blocked requests collapse
		// the same way, on a line of their own for each reason: the first one
		// is what needs the developer, and an npm install that retries a
		// blocked registry fifty times is still that one thing. So do allowed
		// requests that failed anyway, under the last reason given.
		host := proxyHost(e.URL)
		key, status, verdict, meta := host, ToolOK, "allowed", e.Verdict
		switch {
		case e.Error != "":
			key, status, verdict, meta = host+" failed", ToolFail, "failed", "failed"
		case !e.Allowed:
			key, status, verdict = host+" "+e.Verdict, ToolBlocked, e.Verdict
		}
		var details []string
		if e.Error != "" {
			details = []string{e.Error}
		}
		if b := m.proxyOpen[key]; b != nil && !b.done {
			b.requests++
			b.target = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
			b.meta = fmt.Sprintf("%d requests %s", b.requests, verdict)
			b.details = details
			break
		}
		b := &block{
			kind: kindTool, verb: "proxy",
			target: strings.TrimSpace(e.Method + " " + e.URL),
			meta:   meta, status: status, requests: 1, details: details,
		}
		m.push(b)
		m.proxyOpen[key] = b

	case ApprovalRequest:
		m.openModal(e)

	case CancelApproval:
		if m.approval != nil && m.approval.req.ID == e.ID {
			m.approval = nil
			text := "approval withdrawn"
			if e.Reason != "" {
				text += ": " + e.Reason
			}
			m.push(&block{kind: kindNotice, text: text, level: NoticeWarn, done: true})
			m.openNextModal()
		} else {
			m.dropQueuedModal(e.ID)
		}

	case Confirm:
		m.openModal(e)

	case CancelConfirm:
		if m.confirm != nil && m.confirm.req.ID == e.ID {
			m.confirm = nil
			m.openNextModal()
		} else {
			m.dropQueuedModal(e.ID)
		}

	case DiffReview:
		m.openModal(e)

	case CancelDiffReview:
		if m.diff != nil && m.diff.rev.ID == e.ID {
			m.diff = nil
			m.openNextModal()
		} else {
			m.dropQueuedModal(e.ID)
		}

	case ModelChoice:
		if len(e.Models) == 0 {
			// Nothing to ask. Choosing for the developer is exactly what this
			// box exists not to do, so it declines and says why.
			m.out.push(ModelDecision{ID: e.ID, Cancelled: true,
				Reason: "no models left to choose from"})
			break
		}
		m.openModal(e)

	case CancelModelChoice:
		if m.picker != nil && m.picker.req.ID == e.ID {
			m.picker = nil
			m.openNextModal()
		} else {
			m.dropQueuedModal(e.ID)
		}

	case Completions:
		if m.input.pop.active && m.input.pop.token == e.Token {
			items := e.Items
			if m.input.pop.kind == CompletionSlash {
				items = append(filterCommands(m.opts.Commands, m.input.pop.query), items...)
			}
			m.input.pop.items = items
			if m.input.pop.sel >= len(items) {
				m.input.pop.sel = 0
			}
		}

	case Quit:
		return m.quit()
	}

	return tea.Batch(m.commitReady(), m.maybeTick())
}

// diffHighlighter builds the syntax highlighter for the diff viewer, or nil
// when it is turned off or the terminal has no colour.
func diffHighlighter(o *Options) *highlighter {
	if o.PlainDiff {
		return nil
	}
	return newHighlighter(o.Theme.SyntaxStyle, o.Theme.SyntaxFormatter)
}

func (m *uiModel) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// typedInto reports whether a key belongs to the sentence the developer was
// typing when a box opened over it, rather than to the box — and drops it.
//
// The boxes that open on their own — "REQUEST A POLICY", a request waiting
// for approval — answer to single letters, and can open mid-word. Typing on
// into one answered it a letter at a time: the "n" of "Run `node`" said no,
// and nothing on screen said so. So a box that opens while keys are coming
// takes none until they stop for typingGrace — time to see the box and stop
// — and one that opens while the developer is idle answers at once. Ctrl+C
// always gets through.
func (m *uiModel) typedInto(prev time.Time, key string) bool {
	if (m.approval == nil && m.confirm == nil && m.picker == nil) || m.modalArmed || key == "ctrl+c" {
		return false
	}
	if prev.IsZero() || m.lastKey.Sub(prev) >= typingGrace {
		m.modalArmed = true
		return false
	}
	return true
}

// openModal shows an interactive request, or queues it behind the one already
// on screen.
func (m *uiModel) openModal(ev Event) {
	if m.modalActive() {
		m.modalQueue = append(m.modalQueue, ev)
		return
	}
	// Armed unless keys are still coming: see typedInto.
	m.modalArmed = m.lastKey.IsZero() || m.clock().Sub(m.lastKey) >= typingGrace
	switch e := ev.(type) {
	case ApprovalRequest:
		m.approval = newApprovalView(e, m.th)
	case Confirm:
		m.confirm = newConfirmView(e, m.th)
	case DiffReview:
		m.diff = newDiffView(e, m.th, m.hl)
	case ModelChoice:
		m.picker = newModelPickerView(e, m.th)
	}
}

// openNextModal shows the next queued request, if any.
func (m *uiModel) openNextModal() {
	if m.modalActive() || len(m.modalQueue) == 0 {
		return
	}
	next := m.modalQueue[0]
	m.modalQueue = m.modalQueue[1:]
	m.openModal(next)
	// Opened by answering the one before: the key just pressed was for a
	// box, not part of a sentence, so this one takes keys at once.
	m.modalArmed = true
}

// dropQueuedModal removes a withdrawn request that had not been shown yet.
func (m *uiModel) dropQueuedModal(id string) {
	for i, ev := range m.modalQueue {
		var match bool
		switch e := ev.(type) {
		case ApprovalRequest:
			match = e.ID == id
		case Confirm:
			match = e.ID == id
		case DiffReview:
			match = e.ID == id
		case ModelChoice:
			match = e.ID == id
		}
		if match {
			m.modalQueue = append(m.modalQueue[:i], m.modalQueue[i+1:]...)
			return
		}
	}
}

// ring rings the terminal bell.
//
// It is written straight to the terminal rather than placed in a frame,
// because a frame is a picture of the current state: the renderer coalesces
// them, and the one carrying the bell is usually overwritten before it is
// flushed. A bell is an event, not a state. This is the same channel Bubble
// Tea itself writes window titles on, and a zero-width byte cannot move the
// cursor or disturb the layout.
func (m *uiModel) ring() {
	if m.sink == nil {
		return
	}
	m.sink.Write([]byte{7})
}

// shouldRing decides whether the end of this turn is worth a bell: long
// enough to have walked away from, and (unless configured otherwise) in a
// window the developer is not looking at.
func (m *uiModel) shouldRing() bool {
	if m.opts.Bell == BellNever {
		return false
	}
	if time.Since(m.busyStart) < m.opts.BellAfter {
		return false
	}
	return m.opts.Bell == BellAlways || !m.focused
}

// releaseQueued sends the next queued prompt, if there is one. One per Idle,
// not all of them: the application says when it is ready by going Idle, and
// dumping the whole queue on it would recreate the race the queue exists to
// avoid.
// It queues the prompt as a block and leaves committing to the caller, so that
// whatever the turn left behind and the prompt that follows it are printed in
// one pass, in the order they happened.
func (m *uiModel) releaseQueued() {
	if len(m.queued) == 0 {
		return
	}
	text := m.queued[0]
	m.queued = m.queued[1:]
	m.push(&block{kind: kindUser, text: text, done: true})
	m.out.push(UserPrompt{Text: text})
}

// interrupt stops the turn and hands anything queued back to the developer,
// rather than firing it at an agent that was just told to stop. Nothing typed
// is thrown away: it lands in the input box to edit, resend or clear.
func (m *uiModel) interrupt() {
	m.out.push(Interrupt{})
	if len(m.queued) == 0 {
		return
	}
	text := strings.Join(m.queued, "\n")
	m.queued = nil
	if v := m.input.value(); v != "" {
		text = v + "\n" + text
	}
	m.input.setValue(text)
}

// queuedLines renders the prompts waiting for the current turn to finish.
func (m *uiModel) queuedLines() []string {
	if len(m.queued) == 0 {
		return nil
	}
	t := m.th
	out := make([]string, 0, len(m.queued))
	for _, q := range m.queued {
		out = append(out, "  "+t.QueuedGlyph.Render(t.Symbols.Queued)+" "+
			t.Queued.Render(truncPlain(q, m.frameWidth()-5, t.Symbols.Ellipsis)))
	}
	return out
}

// push appends a block and indexes it by ID when it is still open. Anything
// printed after a collapsed proxy line finishes it, so the transcript keeps
// its order: allowed requests that come after start a line of their own. Other
// hosts' allowed lines do not — a package manager alternates between an index
// and a download host, and each keeps counting on its own line — and nor do
// blocked requests, which are part of the same burst of traffic: they go
// beneath the count, counting on a line of their own, and the allowed count
// carries on past them, rather than splitting one npm install
// into "123 requests allowed" and, two lines later, "63 requests allowed".
func (m *uiModel) push(b *block) {
	if b.requests == 0 && b.verb != "proxy" {
		m.finishProxyLines()
	}
	if b.id != "" && !b.done {
		m.byID[b.id] = b
	}
	switch b.kind {
	case kindUser:
		m.inTurn = true
	case kindArt:
	default:
		b.nested = m.inTurn
	}
	m.pending = append(m.pending, b)
}

func (m *uiModel) finishProxyLines() {
	for host, b := range m.proxyOpen {
		b.done = true
		delete(m.proxyOpen, host)
	}
}

// proxyHost is the host a proxy line is collapsed under: scheme and host,
// the way a policy tells requests apart.
func proxyHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

// closeOpen finishes anything still open. Idle means the turn is over, so a
// tool the application forgot to end is reported as skipped rather than
// wedging the commit queue forever.
func (m *uiModel) closeOpen() {
	m.finishProxyLines()
	for _, b := range m.pending {
		if b.done {
			continue
		}
		if b.kind == kindTool || b.kind == kindProgress {
			b.status = ToolSkipped
		}
		b.done = true
	}
}

// commitReady prints every finished block at the head of the queue into the
// terminal's scrollback.
func (m *uiModel) commitReady() tea.Cmd {
	if m.altScreen || m.fullscreen() {
		return nil // held until the alternate screen is gone; see syncScreen
	}
	lines := m.commitLines()
	if len(lines) == 0 {
		return nil
	}
	return tea.Println(strings.Join(lines, "\n"))
}

// commitLines drains the finished head of the transcript and returns it. It is
// the whole of commitReady while the UI is running; on the way out the lines
// are drawn instead of printed, which is why it is separate.
func (m *uiModel) commitLines() []string {
	var lines []string
	for len(m.pending) > 0 && m.pending[0].done {
		b := m.pending[0]
		m.pending = m.pending[1:]
		if b.id != "" {
			delete(m.byID, b.id)
		}
		rendered := b.renderIn(m.th, m.md, m.frameWidth(), m.spinner())
		if len(rendered) == 0 {
			continue
		}
		if spacingBefore(m.last, b) {
			lines = append(lines, "")
		}
		lines = append(lines, rendered...)
		m.anyDone = true
		m.last = b
		switch b.kind {
		case kindUser:
			m.recent = nil
		case kindNotice:
			m.recent = tailLines(append(m.recent, rendered...), 2)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return m.clamp(lines)
}

// quit ends the session, carrying the rest of the transcript out with it.
//
// Bubbletea holds tea.Println output in the renderer until the next flush, and
// flush returns early when the frame buffer is empty — dropping every queued
// line with it. Since the frame that View reports while quitting is the last
// one ever flushed, an empty one silently discards whatever the transcript had
// just committed. Whether the last lines survived came down to whether a 60fps
// render tick happened to land between the print and the quit, so `john apply`
// lost its "N files ->" confirmation about half the time it ran.
//
// Worse, tea.Quit returns from the event loop the moment it is seen, so a
// print the previous update put in flight is dropped before it is ever
// handled. Whether the last lines survived came down to which goroutine won,
// so `john apply` lost its "N files ->" confirmation about half the time.
//
// The exit therefore has three parts, and all of them are needed: anything
// still uncommitted is drawn as the closing frame; View guarantees that frame
// is never empty, so the flush carrying it out is not skipped; and the quit
// itself waits a couple of frames, which is long enough for a print already in
// flight to be delivered and written.
func (m *uiModel) quit() tea.Cmd {
	if m.quitting {
		return nil
	}
	m.quitting = true
	m.farewell = m.commitLines()
	return tea.Tick(quitGrace, func(time.Time) tea.Msg { return quitNowMsg{} })
}

// ------------------------------------------------------------------ keys ---

func (m *uiModel) handleKey(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()
	prev := m.lastKey
	m.lastKey = m.clock()
	if m.typedInto(prev, key) {
		return nil
	}
	if m.modalActive() || key == "enter" {
		// A key in a box is not typing: the box john opens because of it
		// — "apply" and then "write these files?" — takes keys at once.
		// Neither is the Enter that sends a prompt: the sentence is over,
		// and a box the turn opens is one the developer is waiting for.
		defer func() { m.lastKey = time.Time{} }()
	}
	if key != "ctrl+c" {
		m.pendingCT = false
	}

	// A big paste collapses to a placeholder. The application still gets every
	// byte; the box just stops being unusable.
	if msg.Paste && !m.modalActive() {
		if m.input.collapsePaste(string(msg.Runes), m.opts.PasteLines) {
			return nil
		}
	}

	if m.input.searching && !m.modalActive() {
		m.input.searchKey(msg)
		return nil
	}

	// Same order as View: exactly one box is ever open, and the two places
	// that decide which are kept in step so they cannot disagree.
	if m.approval != nil {
		return m.approvalKey(msg, key)
	}
	if m.confirm != nil {
		return m.confirmKey(msg, key)
	}
	if m.picker != nil {
		return m.pickerKey(msg, key)
	}
	if m.diff != nil {
		return m.diffKey(msg, key)
	}

	// Completion popup owns navigation keys while it has candidates.
	if m.input.pop.active && len(m.input.pop.items) > 0 {
		switch key {
		case "up", "ctrl+p":
			m.input.pop.sel = (m.input.pop.sel - 1 + len(m.input.pop.items)) % len(m.input.pop.items)
			m.input.pop.moved = true
			return nil
		case "down", "ctrl+n":
			m.input.pop.sel = (m.input.pop.sel + 1) % len(m.input.pop.items)
			m.input.pop.moved = true
			return nil
		case "tab":
			m.input.accept()
			return nil
		case "enter":
			// Enter means "send what I typed" unless the developer has
			// actually chosen something from the list.
			if m.input.pop.moved {
				m.input.accept()
				return nil
			}
			m.input.pop = popupState{}
		case "esc":
			m.input.pop = popupState{}
			return nil
		}
	}

	switch key {
	case "ctrl+c":
		return m.interruptOrQuit()

	case "ctrl+d":
		if m.input.value() == "" {
			return m.quit()
		}

	case "esc":
		if m.busy {
			m.interrupt()
			return nil
		}

	case "ctrl+r":
		if len(m.input.history) > 0 {
			m.input.startSearch()
		}
		return nil

	case "enter":
		return m.submit()

	case "backspace":
		// Nothing to delete, but something to take back.
		if m.input.value() == "" && len(m.queued) > 0 {
			m.input.setValue(m.queued[len(m.queued)-1])
			m.queued = m.queued[:len(m.queued)-1]
			return nil
		}

	case "up":
		if row, _ := m.input.cursorRowCol(); row == 0 && m.input.historyPrev() {
			m.refreshCompletion()
			return nil
		}
	case "down":
		if row, _ := m.input.cursorRowCol(); row == m.input.ta.LineCount()-1 && m.input.historyNext() {
			m.refreshCompletion()
			return nil
		}
	case "tab":
		// Tab with no popup opens one when there is a trigger under the cursor.
		if cmd := m.refreshCompletion(); m.input.pop.active {
			return cmd
		}
		return nil
	}

	cmd := m.input.update(msg)
	return tea.Batch(cmd, m.refreshCompletion())
}

func (m *uiModel) interruptOrQuit() tea.Cmd {
	if m.busy {
		m.interrupt()
		return nil
	}
	if m.input.value() != "" {
		m.input.reset()
		return nil
	}
	if !m.ctrlCAt.IsZero() && time.Since(m.ctrlCAt) < ctrlCWindow {
		return m.quit()
	}
	m.ctrlCAt = time.Now()
	m.pendingCT = true
	return nil
}

func (m *uiModel) approvalKey(msg tea.KeyMsg, key string) tea.Cmd {
	// Lay out before reading a key: the view may not have been drawn yet, and
	// scrolling needs to know how tall the content is.
	m.approval.layout(boxInnerWidth(m.frameWidth()))
	h := m.modalHeight(len(m.approval.lines), approvalChrome)
	if key == "ctrl+c" {
		m.out.push(ApprovalDecision{ID: m.approval.req.ID, Decision: DecisionReject, Reason: "interrupted"})
		m.approval = nil
		m.openNextModal()
		return nil
	}
	decision, answered := m.approval.choiceBox.update(msg, h)
	if !answered {
		return nil
	}
	req := m.approval.req
	m.approval = nil
	m.out.push(ApprovalDecision{ID: req.ID, Decision: decision})
	defer m.openNextModal()
	m.push(&block{
		kind: kindTool, verb: "proxy", done: true,
		target: strings.TrimSpace(req.Method + " " + req.URL),
		meta:   decision,
		status: approvalGlyphStatus(decision),
	})
	return m.commitReady()
}

func approvalGlyphStatus(decision string) ToolStatus {
	if decision == DecisionReject {
		return ToolBlocked
	}
	return ToolOK
}

func (m *uiModel) confirmKey(msg tea.KeyMsg, key string) tea.Cmd {
	m.confirm.layout(boxInnerWidth(m.frameWidth()))
	h := m.modalHeight(len(m.confirm.lines), confirmChrome)
	if key == "ctrl+c" {
		m.out.push(ConfirmDecision{ID: m.confirm.req.ID, Decision: m.confirm.cancelDecision(), Reason: "interrupted"})
		m.confirm = nil
		m.openNextModal()
		return nil
	}
	decision, answered := m.confirm.update(msg, h)
	if !answered {
		return nil
	}
	req := m.confirm.req
	m.confirm = nil
	m.out.push(ConfirmDecision{ID: req.ID, Decision: decision})
	m.openNextModal()
	return nil
}

func (m *uiModel) diffKey(msg tea.KeyMsg, key string) tea.Cmd {
	m.diff.layout(boxInnerWidth(m.frameWidth()))
	h := m.modalHeight(len(m.diff.rows), diffChrome)
	if key == "ctrl+c" {
		m.out.push(DiffDecision{ID: m.diff.rev.ID, Apply: false})
		m.diff = nil
		m.openNextModal()
		return nil
	}
	dec := m.diff.update(msg, h)
	if dec == nil {
		return nil
	}
	m.diff = nil
	m.out.push(*dec)
	m.openNextModal()
	return nil
}

// pickerKey drives the model picker. Ctrl+C cancels it like every other box,
// and cancelling here means the turn does not run: there is no model left to
// fall back to that the developer has agreed to.
func (m *uiModel) pickerKey(msg tea.KeyMsg, key string) tea.Cmd {
	m.picker.layout(boxInnerWidth(m.frameWidth()))
	h := m.modalHeight(len(m.picker.rows), pickerChrome)
	if key == "ctrl+c" {
		m.out.push(ModelDecision{ID: m.picker.req.ID, Cancelled: true, Reason: "interrupted"})
		m.picker = nil
		m.openNextModal()
		return nil
	}
	dec := m.picker.update(msg, h)
	if dec == nil {
		return nil
	}
	req := m.picker.req
	m.picker = nil
	m.out.push(*dec)
	defer m.openNextModal()
	if req.Preference {
		return nil
	}

	// The narrowing is a fact about the rest of the session, so it is stated
	// in the transcript rather than left in a box that has closed.
	if dec.Cancelled {
		m.push(&block{
			kind: kindNotice, level: NoticeWarn, done: true,
			text: "no model chosen " + m.th.Symbols.Dot + " the turn stopped here",
		})
		return m.commitReady()
	}
	name := dec.ModelID
	for _, mo := range req.Models {
		if mo.ID == dec.ModelID {
			name = mo.Name
			break
		}
	}
	// One line for the switch, saying why: "session narrowed" was jargon, and
	// the reason was gone with the box.
	b := &block{
		kind: kindTool, verb: "model", done: true, status: ToolOK,
		target: name, meta: "switched by a routing policy for this session",
	}
	if req.Reason != "" {
		b.details = []string{req.Reason}
	}
	m.push(b)
	return m.commitReady()
}

func (m *uiModel) submit() tea.Cmd {
	raw := strings.TrimRight(m.input.value(), " \t\n")
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}
	// Expand before the reset: the input forgets its pastes when it clears.
	full := m.input.expandPastes(text)
	m.input.pushHistory(text, m.opts.HistoryLimit)
	m.input.reset()

	// Slash commands are for the application and go straight through, busy or
	// not: /quit has to work while the agent is mid-turn.
	if strings.HasPrefix(text, "/") {
		name, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
		m.push(&block{kind: kindUser, text: text, done: true})
		m.out.push(SlashCommand{Name: name, Args: strings.TrimSpace(args), Raw: text})
		return m.commitReady()
	}

	if m.busy && m.opts.PromptQueue == QueueWhileBusy {
		// Not committed to the transcript yet: it has not been sent, and a
		// transcript that shows it before the answer above it would be lying
		// about the order things happened in.
		m.queued = append(m.queued, full)
		return nil
	}

	m.push(&block{kind: kindUser, text: text, done: true})
	m.out.push(UserPrompt{Text: full})
	return m.commitReady()
}

// refreshCompletion recomputes the popup from what is typed before the cursor.
func (m *uiModel) refreshCompletion() tea.Cmd {
	kind, row, col, query, ok := m.input.detectTrigger(m.opts)
	if !ok {
		m.input.pop = popupState{}
		return nil
	}
	same := m.input.pop.active && m.input.pop.kind == kind &&
		m.input.pop.row == row && m.input.pop.col == col && m.input.pop.query == query
	if same {
		return nil
	}

	m.nextToken++
	pop := popupState{active: true, kind: kind, row: row, col: col, query: query, token: m.nextToken}
	if kind == CompletionSlash {
		pop.items = filterCommands(m.opts.Commands, query)
		if !m.opts.CompleteSlashCommands {
			m.input.pop = pop
			return nil
		}
	}
	m.input.pop = pop
	m.out.push(CompletionRequest{Token: pop.token, Kind: kind, Query: query})
	return nil
}

// ------------------------------------------------------------------ view ---

// modalHeight is how many rows a modal body may use: enough for its content,
// never more than the configured share of the terminal, minus the rows its own
// chrome (borders, buttons, hints, status line) needs.
func (m *uiModel) modalHeight(contentLines, chrome int) int {
	budget := m.height*m.opts.MaxModalPercent/100 - chrome
	if budget < 3 {
		budget = 3
	}
	if contentLines < budget {
		return max(contentLines, 1)
	}
	return budget
}

// approvalChrome is the row count around the approval box body: top border,
// title bar, divider, closing divider, button row, bottom border.
const approvalChrome = 6

// confirmChrome is the approval box's chrome without the scrollbar's needs.
const confirmChrome = 6

// pickerChrome matches approvalChrome: the model picker is the same box.
const pickerChrome = 6

// diffChrome matches approvalChrome: top border, title row, divider, closing
// divider, button row, bottom border.
const diffChrome = 6

// modalBox draws one interactive box: the border, the title row, the body with
// a scrollbar down the right when it overflows, the divider, the button row,
// and whatever rides on the bottom border.
//
// Every box on screen is one of these. The only thing that differs between the
// approval inspector, a confirmation, the model picker and the diff viewer is
// what they put in the body and what their keys mean.
func (m *uiModel) modalBox(h boxHeader, body []string, offset, total int, buttons, footer string) []string {
	w := m.frameWidth()
	bar := m.th.scrollbar(offset, len(body), total)
	out := make([]string, 0, len(body)+6)
	out = append(out, m.th.boxTop("", "", w))
	out = append(out, m.th.boxTitle(h, w))
	out = append(out, m.th.boxSep(w))
	for i, l := range body {
		if bar == nil {
			out = append(out, m.th.boxRow(l, w))
			continue
		}
		out = append(out, m.th.boxRowBar(l, bar[i], w))
	}
	out = append(out, m.th.boxSep(w))
	out = append(out, m.th.boxRow(buttons, w))
	return append(out, m.th.boxBottom(footer, "", w))
}

func (m *uiModel) View() string {
	if m.quitting {
		// The closing frame, and never empty: an empty one is skipped by the
		// renderer, which takes every line still queued for printing with it.
		// See quit().
		if len(m.farewell) == 0 {
			return "\n"
		}
		return strings.Join(m.farewell, "\n") + "\n"
	}
	if !m.ready {
		return ""
	}
	var lines []string

	switch {
	case m.approval != nil:
		lines = append(lines, m.liveTail()...)
		v := m.approval
		v.layout(boxInnerWidth(m.frameWidth()))
		h := m.modalHeight(len(v.lines), approvalChrome)
		body := v.view(h)
		lines = append(lines, m.modalBox(v.header(h), body, v.offset, len(v.lines),
			v.buttonRow(boxInnerWidth(m.frameWidth()), h), "")...)

	case m.confirm != nil:
		lines = append(lines, m.liveTail()...)
		v := m.confirm
		v.layout(boxInnerWidth(m.frameWidth()))
		h := m.modalHeight(len(v.lines), confirmChrome)
		body := v.view(h)
		lines = append(lines, m.modalBox(v.header(h), body, v.offset, len(v.lines),
			v.buttonRow(boxInnerWidth(m.frameWidth()), h), "")...)

	case m.picker != nil:
		lines = append(lines, m.liveTail()...)
		v := m.picker
		v.layout(boxInnerWidth(m.frameWidth()))
		h := m.modalHeight(len(v.rows), pickerChrome)
		body := v.view(h)
		lines = append(lines, m.modalBox(v.header(h), body, v.offset, len(v.rows),
			v.buttonRow(boxInnerWidth(m.frameWidth()), h), "")...)

	case m.diff != nil:
		// The whole screen is the review's; see fullscreen. What the
		// transcript said about it — "1 file not selected", say — was
		// printed to the screen the review hides, so it is repeated above
		// the box, with the tail of any work still running.
		v := m.diff
		v.layout(boxInnerWidth(m.frameWidth()))
		var above []string
		above = append(above, m.recent...)
		above = append(above, m.liveLines(2)...)
		if len(above) > 0 {
			above = append(above, "")
		}
		lines = append(lines, above...)
		// View keeps the frame a row short of the terminal, and an alert
		// takes one more under the box.
		h := m.height - 1 - diffChrome - len(above)
		if m.status.Alert != "" {
			h--
		}
		h = max(h, 3)
		body := v.view(h)
		for len(body) < h {
			body = append(body, "")
		}
		lines = append(lines, m.modalBox(v.header(h), body, v.offset, len(v.rows),
			v.buttonRow(boxInnerWidth(m.frameWidth()), h), v.summary())...)

	default:
		// The in-flight turn, then a blank row, then the input field between
		// two rules. The rules give the field an edge to sit against, and the
		// blank keeps that edge from crowding the work above it.
		live := m.liveLines(m.opts.MaxLiveLines)
		busy := m.busyLine()
		queued := m.queuedLines()
		if (len(live) > 0 && m.liveGap()) || (len(live) == 0 && (busy != "" || len(queued) > 0)) {
			lines = append(lines, "")
		}
		lines = append(lines, live...)
		if busy != "" {
			// The spinner is its own element, so it gets the same blank line
			// between it and the work above that any two blocks get.
			if len(live) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, busy)
		}
		if len(queued) > 0 {
			if len(live) > 0 || busy != "" {
				lines = append(lines, "")
			}
			lines = append(lines, queued...)
		}
		lines = append(lines, "")
		if a := m.alertLine(); a != "" {
			lines = append(lines, a)
		}
		lines = append(lines, m.inputBox()...)
	}
	if m.modalActive() {
		// A modal hides the input box and the state on its edges, but not an
		// alert: john fails closed, and the developer deciding on a request
		// should know why nothing is going through.
		if a := m.alertLine(); a != "" {
			lines = append(lines, a)
		}
	}

	// Some pieces (the busy line and its thinking text, the input box) render
	// as multi-line strings. Normalise to one entry per terminal row before
	// measuring anything, or the height cap counts rows that do not exist.
	lines = strings.Split(strings.Join(lines, "\n"), "\n")
	lines = m.clamp(lines)
	// The frame must never be taller than the terminal: bubbletea would drop
	// lines from the top itself, but then its own line count no longer matches
	// what is on screen.
	if maxRows := m.height - 1; maxRows > 0 && len(lines) > maxRows {
		lines = lines[len(lines)-maxRows:]
	}
	return strings.Join(lines, "\n")
}

// liveTail is the couple of lines of in-flight work shown above a modal view,
// kept apart from it by a blank line like everything else.
func (m *uiModel) liveTail() []string {
	live := m.liveLines(2)
	if len(live) == 0 {
		return []string{""}
	}
	var lines []string
	if m.liveGap() {
		lines = append(lines, "")
	}
	lines = append(lines, live...)
	return append(lines, "")
}

// liveGap reports whether the live region needs a blank line between it and
// the scrollback above: not when its first block stacks under the last one
// committed, the way it will once it is committed too.
func (m *uiModel) liveGap() bool {
	if m.last == nil {
		return true
	}
	for _, b := range m.pending {
		if len(b.renderIn(m.th, m.md, m.frameWidth(), m.spinner())) > 0 {
			return spacingBefore(m.last, b)
		}
	}
	return true
}

// liveLines renders the in-flight blocks, keeping the tail when they overflow.
func (m *uiModel) liveLines(maxLines int) []string {
	if len(m.pending) == 0 {
		return nil
	}
	var out []string
	prev := m.last
	for _, b := range m.pending {
		r := b.renderIn(m.th, m.md, m.frameWidth(), m.spinner())
		if len(r) == 0 {
			continue
		}
		if spacingBefore(prev, b) && len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, r...)
		prev = b
	}
	if len(out) > maxLines {
		out = append([]string{m.th.Muted.Render("  " + m.th.Symbols.Ellipsis)}, tailLines(out, maxLines-1)...)
	}
	return out
}

func (m *uiModel) busyLine() string {
	if !m.busy {
		return ""
	}
	t := m.th
	elapsed := time.Since(m.busyStart).Truncate(time.Second)
	line := t.busySpinner(m.busySpin, m.spinFrame) + " " +
		t.busyLabel(m.busyFx, m.busyLabel, m.spinFrame)
	if elapsed >= time.Second {
		line += t.Muted.Render(" " + t.Symbols.Dot + " " + elapsed.String())
	}
	if m.thinking != "" {
		// Hung off the spinner with the same connector a tool hangs its output
		// from, so that the missing blank line reads as "this belongs to that"
		// rather than as two elements run together.
		room := m.frameWidth() - 4
		line += "\n  " + t.Tree.Render(t.Symbols.TreeEnd+" ") +
			t.Hint.Render(truncPlain(m.thinking, room, t.Symbols.Ellipsis))
	}
	return line
}

func formatTokens(n int) string {
	unit := func(whole, tenth int, suffix string) string {
		if tenth == 0 {
			return itoa(whole) + suffix // "200k", not "200.0k"
		}
		return itoa(whole) + "." + itoa(tenth) + suffix
	}
	switch {
	case n >= 1_000_000:
		return unit(n/1_000_000, n/100_000%10, "M")
	case n >= 1_000:
		return unit(n/1_000, n/100%10, "k")
	default:
		return itoa(n)
	}
}
