# terminalui

The terminal UI package for John. It renders transcripts, streaming answers,
tool calls, approval requests, and diff reviews, and reports user actions to
the application. Typed events keep rendering independent of sandbox, policy,
proxy, and model implementations, so the UI can be tested without an agent.

macOS, Linux and Windows (see [Windows](#windows)). Built on
[Bubble Tea](https://github.com/charmbracelet/bubbletea).

```
go run .                         # john itself: proxy, policies, sandbox, a real backend
go run . -plain | tee out.log    # the non-tty rendering
go run ./terminalui/cmd/demo     # the minimal example: one file, one fake agent
```

`terminalui/cmd/demo` is the second consumer of this package, and the reason to
keep it: a package with one caller is a package whose API has never been
questioned. The UI there is real; the agent behind it is a fake that answers
from a script, with no backend, policy snapshot, proxy or sandbox under it.

## Package API

Two typed streams, both of plain values:

```go
ui := terminalui.New(terminalui.Options{
    FileCompletion: true,
    Commands: []terminalui.Command{{Name: "apply", Description: "review and apply"}},
})

go func() {
    ui.Send(terminalui.Notice{Text: "sandbox resumed"})

    for act := range ui.Actions() {
        switch a := act.(type) {
        case terminalui.UserPrompt:       agent.Ask(a.Text)
        case terminalui.SlashCommand:     run(a.Name, a.Args)
        case terminalui.Interrupt:        agent.Stop()
        case terminalui.ApprovalDecision: proxy.Resolve(a.ID, a.Decision)
        case terminalui.DiffDecision:     sandbox.Apply(a.Paths)
        case terminalui.ModelDecision:    session.ContinueOn(a.ModelID)
        case terminalui.CompletionRequest:
            ui.Send(terminalui.Completions{Token: a.Token, Items: paths(a.Query)})
        }
    }
}()

err := ui.Run(ctx) // owns the terminal until the session ends
```

- `Send` never blocks and is safe from any goroutine, including before `Run`.
- `Actions()` closes when `Run` returns, so ranging over it is a whole app loop.
- Nothing in the package reads the clock, the filesystem or the network.

### Events (application → UI)

| Event | Meaning |
| --- | --- |
| `Notice` | a standalone line: boot messages, warnings, errors |
| `Status` | the state on the input box's edges (never scrollback) |
| `Busy` / `Idle` | a turn is running; shows the spinner, arms Esc, queues prompts |
| `Thinking` | transient reasoning, shown live and never committed |
| `AssistantStart` / `Delta` / `End` | a streaming message |
| `AssistantMessage` | a complete message in one event |
| `ToolStart` / `ToolDetail` / `ToolEnd` | a tool call and its child lines |
| `Progress` | long staged work: a bar, a clock, one committed line at the end |
| `ProxyDecision` | a proxy verdict that needed no interaction |
| `ApprovalRequest` / `CancelApproval` | the buffered-request inspector |
| `Confirm` / `CancelConfirm` | a yes/no question in the same box |
| `DiffReview` / `CancelDiffReview` | the review-and-apply viewer |
| `ModelChoice` / `CancelModelChoice` | the session narrowed; pick what it continues on |
| `Completions` | the answer to a `CompletionRequest` |
| `Quit` | shut down; `Run` returns |

### Actions (UI → application)

`UserPrompt`, `SlashCommand`, `Interrupt`, `ApprovalDecision`, `ConfirmDecision`,
`DiffDecision`, `ModelDecision`, `CompletionRequest`.

Request/response pairs (`ApprovalRequest` → `ApprovalDecision`,
`DiffReview` → `DiffDecision`, `ModelChoice` → `ModelDecision`) are correlated by `ID`; every request produces
exactly one decision unless the application withdraws it. See
`cmd/demo/main.go` for the pattern.

## Session layout

```
 ╭─────────────╮
 │  >_  john.  │
 ╰─────────────╯

  sandbox resumed · 4 min to first token saved (toolchain cached)

❯ the retry in billing/client.go gives up on 429s. fix it and prove it.
  ● read billing/client.go, client_test.go, go.mod
    └ 3 files · 412 lines
  ● edit billing/client.go  +34 −6
  ✗ run go test ./billing/...  FAIL · 1 test
    ├ --- FAIL: TestRetry429 (0.02s)
    └     client_test.go:88: gave up after 1 attempt, want 4
  ● proxy GET proxy.golang.org/golang.org/x/time/@v/list  always allow

  → 429 is now retryable with Retry-After honoured, capped at 30s.

    • billing/client.go — retry loop handles 429 and 503, with jittered backoff
    • billing/client_test.go — TestRetry429 asserts four attempts and the cap

╭─ ~/src/payments ───────────────────────────────────────── 2 unapplied · /apply ─╮
│ ❯ ask john to do something…  / commands · @ files                               │
╰──────────────────────────────────────────── internal/codegen-l · 6.1k / 200k ─╯
```

Assistant replies use an arrow in the terminal's foreground color.
Continuation lines are indented to match the marker's width, including the
ASCII fallback (`->`).

A turn's activity — tool calls, proxy decisions, progress and notices — is
indented under the prompt that started it and stacks without blank lines.
Assistant replies get one blank line before and after them, and so does each
new prompt.

The input field sits in a box, with a blank row above it. The completion popup
and history search open inside the box, above the field. Its edges carry the
session's state, so there is no status line: the working directory top left;
the sandbox's state (`2 unapplied · /apply`, `sandbox starting`) top right, in
the border's colour; what can be done now (`esc to interrupt`, `press ctrl+c
again to exit`) bottom left in the key colour; and the model and the context
used bottom right. As the terminal narrows, the directory gives up its leading
segments; the context shortens to `6.1k`, then the model goes. The sandbox's
state and the bottom-left text go last. Key hints follow the placeholder in an empty field.

An alert (`Status.Alert`) is a line of its own directly above the box, and turns
the box's border the alert colour.

The diff review (`DiffReview`) takes the terminal's alternate screen and fills
it, so the transcript is back as it was when the review closes. Transcript
lines that arrive meanwhile are held and printed after it; notices printed just
before it opened are repeated above the box.

`Options.Logo` draws a badge once at the start of scrollback. The plain renderer
omits it. Only the prompt marker and full stop are green; the frame uses the
terminal's foreground color. Narrow terminals show `john.` on one line.
`LogoArt(theme, width)` is also available to callers.

Detail lines use an indented branch marker, such as `  └ 3 files`, without a
vertical rule.

### The busy line

The busy line combines a one-character spinner with an animated label. By
default, each turn randomly selects one spinner and one label effect.

**Spinners** (`Options.BusySpinner`): `braille` `dots` `bar` `column` `arc`
`circle` `quadrant` `bounce` `triangle` `halves` `fill` `pipe` `binary`. Each is
exactly one column, has an ASCII fallback, and the ones that are a scale rather
than a cycle (`bar`, `column`, `fill`) rise and fall in place and brighten as
they grow.

`dot` (`·•●•`) is not in that list: it is reserved for running tool calls and
progress blocks, and kept out of the random pool. A spinner beside a command
means that command is running; a spinner beside the label means the agent is
thinking.

The running dot matches the verb's color: blue for `read`, orange for `run`.
On completion it becomes a static `●` in the same color, unless the tool failed.

```
● run go test ./billing/...

⠋ thinking · 22s · esc to interrupt
  └ the 429 branch returns early instead of backing off
```

**Effects** (`Options.BusyEffect`), shown here as brightness maps of the word
`thinking`, one row per frame:

```
  shimmer                ripple                 reveal                 scan
  ██▓▒····               ▓████▓▒·               █▒▒▒▒▒▒▒               ████████
  ███▓▒···               ▓█████▓▒               ██▒▒▒▒▒▒               ▓███████
  ████▓▒··               ▒▓█████▓               ███▒▒▒▒▒               ▒▓██████
  █▓███▓▒·               ·▒▓████▓               ████▒▒▒▒               ▓▒▓█████
  █▒▓███▓▒               ·▒▓█████               █████▒▒▒               █▓▒▓████
```

`none` leaves it dim and still, `pulse` breathes the whole label, `flow` runs a
ramp through it continuously, and `sparkle` lights a few characters at a time
from a hash, so the same character twinkles on the same frame every time rather
than flickering.

```
go run . -busy-preview                    # every spinner and effect, animating
go run . -spinner=bar -effect=shimmer     # or pin a pair
```

Both halves are chosen when a turn starts and held until it ends, so a turn
never changes character halfway through — and never the same as the turn before,
because a random draw that repeats itself looks exactly like nothing changed. No spinner is
ever wider than one column, no effect changes the width of the label, and on a
terminal without colour every effect degrades to plain dim text while the
spinners carry on spinning. The colours come from `Theme.BusyBase` and
`Theme.BusyHigh`: two hex values blended per character, so a theme can retune
the ramp without touching the code.

### Typing while it works

A prompt submitted mid-turn is queued rather than sent, and released one at a
time as the application goes `Idle`. The developer can think ahead without
interrupting the turn or racing it, and the application never has to reason
about two prompts in flight:

```
⠦ thinking · 4s · esc to interrupt
  └ planning: honour Retry-After, cap the backoff, add a test

  ⇢ also cover the 503 path
  ⇢ then run the linter
────────────────────────────────────────────────────────────
❯ queue a follow-up…
────────────────────────────────────────────────────────────
  ~/src/payments · internal/codegen-l · ⏎ queue · ⌫ take back · esc interrupt
```

- Queued prompts are **not** in the transcript yet. They are shown above the
  input, and committed when they are actually sent — a transcript that showed
  them before the answer above them would be lying about the order.
- One is released per `Idle`, not the whole queue: the application says when it
  is ready by going idle, and dumping the queue on it would recreate the race
  the queue exists to avoid. This makes `Busy`/`Idle` part of the contract.
- **Slash commands are never queued.** They are instructions to the
  application, not messages to the model, and `/quit` has to work mid-turn.
- **Esc hands the queue back** rather than firing it at an agent that was just
  told to stop. Nothing typed is lost: it lands in the input box to edit,
  resend or clear. Backspace on an empty box takes the last one back the same
  way.
- The placeholder changes to say what Enter will do while a turn is running.

`Options.PromptQueue = DeliverImmediately` turns all of this off and emits every
prompt as it is submitted.

### Questions and answers

Four events open a box, and all four are answered exactly once: an
`ApprovalRequest` (a buffered network request), a `Confirm` (a yes/no question),
a `DiffReview`, and a `ModelChoice`. They queue behind each other rather than
replacing one another — the proxy can buffer several requests at once, and every one of them
is owed an answer.

`Confirm` exists for the decisions no policy covers. Resetting a sandbox throws
away work that was never applied; applying a diff writes to files the developer
has open. Those deserve the same box a network request gets, not a line of
output that scrolls past:

```
╭────────────────────────────────────────────────────────────────╮
│ DISCARD SANDBOX ───────────────────────────────────────────── │
├────────────────────────────────────────────────────────────────┤
│ Reset the sandbox for ~/src/payments?                          │
│                                                                │
│ ├ 2 files have not been applied to your working directory.     │
│ └ Installed tools and caches are rebuilt on the next run.      │
├────────────────────────────────────────────────────────────────┤
│  d  discard    k  keep it            ←/→ choose · ⏎ confirm    │
╰────────────────────────────────────────────────────────────────╯
```

Mark the safe option `Cancel: true` and the box opens on it, so Enter, Esc and
Ctrl+C all mean the same thing.

### Model selection

`ModelChoice` is the one box with no safe answer, so it is the one box with no
`Cancel` option to default to.

It opens when a routing policy's keyword has been seen somewhere in the session
— the prompt, a file the agent read, command output, a diff — and the models
the session may use have narrowed underneath the developer. Two things have to
happen at once: the narrowing has to be stated, and a choice has to be made
from what is left. They belong in one box, because the reason is what makes
sense of the question:

```
╭──────────────────────────────────────────────────────────────────────────╮
│ CONTINUE ON  payments-restricted ─────────────────────────────────────── │
├──────────────────────────────────────────────────────────────────────────┤
│   keyword "C++" seen in billing/native/bridge.cc                         │
│   this session continues on an internally hosted model                   │
│                                                                          │
│ ▸ 1 internal/codegen-l · internally hosted · 128k context                │
│   2 internal/codegen-s · internally hosted · 32k context, faster         │
├──────────────────────────────────────────────────────────────────────────┤
│  ⏎  continue    esc  cancel turn            ↑/↓ move · 1-9 pick          │
╰──────────────────────────────────────────────────────────────────────────╯
```

Up and down walk the model rows and never stray onto the prose above them,
Enter takes the one under the cursor, and the digits pick straight from the
list for someone who already knows it. The list does not wrap at either end:
"am I at the bottom?" should be answerable by pressing a key.

Declining sets `Cancelled` rather than choosing a model, and the application is
expected to abandon the turn. Falling back to whatever the session was routed
to before would undo the narrowing, and a session that has seen the keyword
cannot un-see it. The transcript keeps the outcome either way — the model it
continued on, or the fact that the turn did not run — because the narrowing is
a fact about the rest of the session, not about a box that has closed.

### Long work

`Progress` is for the first run in a project: provisioning a virtual machine and
building its image is minutes of dense output, and a spinner that says nothing
for four of them is indistinguishable from a hang. Send it repeatedly with one
ID; it replaces itself, and commits a single line when done.

```
⠋ building sandbox image  step 3/6 · 42s
  ├ ██████████████░░░░░░░░░░░░░░  50%
  └ RUN go mod download
```

### Backend outages

`Status.Alert` is drawn first, in the alert colour, and is the last thing
dropped as the terminal narrows. John fails closed, so the UI says so before it
says anything else:

```
▲ backend unreachable · policy decisions blocked   ~/src/payments · internal/…
```

## Scrollback and rendering

Active blocks are drawn in the live region at the bottom. Completed blocks are
printed to the terminal's scrollback in start order; a tool that finishes early
waits for earlier blocks to finish. Committed output is never repainted or
moved.

The UI leaves the alternate screen and mouse capture disabled to preserve
terminal selection, copy, and search. Committed lines keep their original
wrapping when the window is resized. Only the live region reflows.

### Terminal width limits

Every live and committed line is limited to one column less than the terminal
width. Filling the last column sets the terminal's *wrapped* flag; a later
resize can change the row count and leave the renderer drawing in the wrong
place. Bubble Tea also emits "erase to end of line" only for lines narrower
than the terminal.

`bubbles/textarea` pads rows to its configured width, so that width must leave
the last column free. Status hints follow the text without padding and are
dropped progressively as the terminal narrows.

`TestFrameNeverFillsTerminalWidth` and `TestCommittedLinesNeverFillTerminalWidth`
hold the invariant across widths 24–200, and `TestResizeRelaysEverything` checks
it across a sequence of resizes.

Known limitation: shrinking below the width of a live line can leave one stale
row because the terminal reflows the previous frame before Bubble Tea v1
redraws it. Growing the window does not have this issue. The UI accepts this
limitation to preserve scrollback and avoid erasing committed output.

### Interactive views stay at the bottom

The approval inspector and the diff viewer grow the live region up to
`MaxModalPercent` of the terminal (60% by default) and scroll inside it. The
transcript above them is untouched and still selectable. Neither view leaves
anything behind except its one-line outcome.

The approval inspector is drawn as a box around the whole request, with the
request scrolling inside it and the choices as buttons along the bottom:

```
╭──────────────────────────────────────────────────────────────────────────╮
│ APPROVAL REQUIRED  POST https://telemetry.vendor.io/v1/events ─ 4–11 of 12│
├──────────────────────────────────────────────────────────────────────────┤
│ ask developer · policy group staging-writes                              │
│                                                                          │
│ headers                                                                  │
│   content-type: application/json                                        █│  ← scrollbar, only
│   authorization: Bearer sk-live-…redacted by policy…                    █│    when it overflows
│                                                                          │
│ body (223 bytes)                                                         │
│   {"project":"payments","event":"build.finished","duration_ms":31402}    │
├──────────────────────────────────────────────────────────────────────────┤
│  a  allow once   r  reject                       ←/→ choose · ⏎ confirm  │
╰──────────────────────────────────────────────────────────────────────────╯
```

The box asks about the request in front of the developer and nothing else:
allow this one, or refuse it. A rule that would cover the next request like it
is a different question, and `DefaultApprovalOptions` deliberately does not
offer it here — an application that wants to ask it opens its own `Confirm`
once the request has already been dealt with.

The title row is ruled out to the edge in the border's own colour rather than
painted with a background — inside a drawn box, a filled bar reads as a second
frame competing with the first.

`←`/`→` (or Tab) move between buttons, Enter confirms the selected one, and the
single-key shortcuts still work. Arrow keys never do two jobs at once: left and
right choose, up and down scroll the request.

The keys that drive the box live in the box, on the right of the button row, and
drop out when the terminal is too narrow to hold both. Esc is not among them: it
rejects, and a way out of a decision does not belong in the same breath as the
decision.

The diff viewer is drawn the same way, with the totals riding on the bottom
border so they stay in view while the hunks scroll past:

```
╭──────────────────────────────────────────────────────────────────────────╮
│ REVIEW CHANGES  apply to ~/src/payments ─────────────────────  1–12 of 22│
├──────────────────────────────────────────────────────────────────────────┤
│   [✓] - billing/client.go  +38 −10                                      █│
│       @@ -18,10 +18,38 @@ func (c *Client) do(req *http.Request)        █│
│ ▸           if err != nil {                                             █│
│       -     if resp.StatusCode == http.StatusTooManyRequests {           │
│       +     if retryable(resp.StatusCode) {                              │
├──────────────────────────────────────────────────────────────────────────┤
│  a  apply selected   q  cancel     ↑/↓ move · space select · ⏎ expand    │
╰─ 3 files · +59 −10 · 2 selected ─────────────────────────────────────────╯
```

Changed lines carry two more things worth having. Code is syntax-highlighted
(chroma, off automatically when the terminal has no colour, or with
`Options.PlainDiff`), and the part of a line that *actually differs* from the
line it replaced is picked out by comparing each removed line with the added one
that follows it. A one-character change to a URL in a build script otherwise
looks exactly like a reindented line — and the readme's own threat model says
the review is the control.

`/` filters the review by path, because a hundred-file review that cannot be
narrowed is a review that gets skimmed. Select-all only ever touches what is on
screen: selecting files the filter is hiding would be selecting files nobody
looked at.

Nothing in the diff viewer is *focused* the way an approval button is: Enter
expands a file there, so a filled button would promise something Enter does not
do. Its buttons are labels for keys, and the apply button greys out while
nothing is selected — which is exactly when the key does nothing.

The scroll position, the scrollbar and the `j/k scroll` hint appear **only when
the request is taller than the box**. A counter reading `1/13` while all
thirteen lines are visible is a promise of more that does not exist, and it
makes a working scroll key look broken.

The cursor starts on the rejecting option, so Enter, Esc and Ctrl+C all mean the
same thing — nothing leaves the machine — and allowing takes a deliberate move.

## Without a terminal

When stdout or stdin is not a terminal (`ModeAuto`), the same event stream
renders as sequential plain lines with no live region, no cursor movement and no
escape sequences — `john | tee build.log` produces a readable log.

Decisions that cannot be presented are not taken on the developer's behalf:

- `NonInteractiveReject` (default) answers `ApprovalRequest` with
  `DecisionReject`, `Confirm` with its cancelling choice, `DiffReview` with
  `Apply: false`, and `ModelChoice` with `Cancelled: true`, each carrying a
  reason.
- `NonInteractiveError` makes `Run` return `ErrNotInteractive` instead.

A `ModelChoice` is never answered by picking the first model on the list. The
session narrowed because something in it must not reach every model, and
choosing one on the developer's behalf is exactly the decision they were meant
to make.

## Input

Enter sends, `alt+enter` (or `ctrl+j`) inserts a newline, and the box grows to
eight lines. Up/down walk prompt history, readline keys work
(`ctrl+a/e/k/u/w`), and bracketed paste arrives as one edit.

`/` at the start of the line opens the command popup, filtered from
`Options.Commands`. `@` opens file completion, which emits a `CompletionRequest`
per keystroke and is answered by the application — the package never touches the
filesystem. Tab accepts the highlighted candidate; Enter sends what was typed
unless you actually chose a candidate with the arrow keys.

Esc interrupts a running turn. Ctrl+C interrupts, then clears the input, then
(pressed twice) quits. Ctrl+D on an empty prompt quits.

## Theming

`Options.Theme` is a struct of `lipgloss` styles and a `Symbols` set; override
whatever you like on top of `DefaultTheme(w)`. Colors are adaptive for light and
dark terminals and degrade through 256-color, 16-color and no-color
automatically (including `NO_COLOR`). `Options.ASCII` swaps every glyph for an
ASCII equivalent.

The terminal is asked for its background color exactly once, inside `Run`,
before anything starts reading keys — `New` never touches the terminal.

## Windows

Cross-compiles and runs on `windows/amd64`. Nothing in the
package uses a POSIX-only syscall, and resizing works without SIGWINCH: Bubble
Tea reads the console's own window-buffer events.

Two things needed handling, and are handled:

- **The output code page.** Bubble Tea enables virtual terminal processing, but
  nothing in the stack sets the console's output code page. On a default
  installation it is 437 or 850, so UTF-8 bytes are decoded as something else
  and every box, glyph and spinner frame arrives as mojibake. `Run` sets it to
  UTF-8 and restores it on the way out.
- **Glyph coverage.** Consolas and the raster fonts of the old console host have
  no `❯`, no `→` and no braille. When the code page cannot be set, or when
  nothing in the environment identifies a modern terminal (`WT_SESSION`,
  `TERM_PROGRAM`, `ConEmuANSI`, `ANSICON`, `TERM`, …), the built-in theme
  downgrades to the ASCII glyph set. Setting `Options.Theme` explicitly opts out
  of the downgrade: an application that asks for a specific theme gets it.

`ASCII` mode is ASCII all the way down — glyphs, box drawing, scroll ranges, the
logo — and `TestASCIIModeEmitsOnlyASCII` holds it there. Text the application
sends is passed through as given; if a session must be ASCII end to end, that
text is the application's to keep ASCII.

Alt+Enter is the fullscreen toggle on Windows and never reaches the program, so
`ctrl+j` inserts a newline there and the hint says so. Both bindings work
everywhere.

This is all source-level: it cross-compiles and vets clean for Windows, but the
package has not been run on a Windows box.

## Safety notes

Tool output, headers and bodies are untrusted input: control characters and
escape sequences are stripped before anything is drawn, so a test that prints
`\x1b[2J` cannot clear the screen or hide what it did. Everything is truncated
or wrapped to the terminal width, never allowed to overflow.

### Input

Enter sends; a prompt submitted mid-turn queues instead. Large pastes collapse
to `[paste #1: 412 lines, 9021 bytes]` in the box and expand again on the way
out, so the developer keeps sight of their own prompt. `ctrl+r` searches history
backwards. A turn that runs longer than `Options.BellAfter` and ends while the
terminal is unfocused rings the bell.

## Tests

```
go test ./terminalui/                 # everything
go test ./terminalui/ -update         # re-record the golden frames
go test -race ./terminalui/           # the concurrency
```

Four layers, because they catch different things:

- **Model tests** drive the model directly — keys in, actions out, no terminal
  involved. That is the same property that makes the package scriptable.
- **Golden frames** (`testdata/*.frame`) record what each state actually looks
  like. Invariants catch "too wide" and "missing"; only a recorded frame catches
  "still looks the way it was designed to".
- **PTY tests** run the real UI over a real pty into a small terminal emulator
  (`vt`, tested in its own right) that autowraps and re-flows the way a
  terminal does. This is the only layer that can prove the resize guarantee, and
  `TestPTYResizeLeavesNoDebris` is the regression test for the bug that started
  it.
- **Session tests** in `client` drive john itself by typing at it, so
  the flows an application has to get right — an approval mid-turn, a review, a
  confirmation, a session that narrows onto a different model, a backend that
  goes missing — are covered as a developer meets them rather than as a
  sequence of events.

Two habits worth keeping. Anything untrusted that reaches the screen — tool
output, a URL, a header, a status string — gets a case in
`TestUntrustedTextIsSanitizedEverywhere`; that test has already caught escape
sequences reaching the terminal through the approval box. And anything with a
width gets a case at several terminal widths including a very narrow one, since
almost every layout bug in this package has been an off-by-one at the edge of
the screen.
