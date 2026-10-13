# The client/backend protocol

The backend runs the agent loop. The client holds the developer's session,
executes tool calls in the sandbox, and returns the results. This document
describes the messages and connection behavior between them.

[`messages.go`](messages.go) defines the message set and [`conn.go`](conn.go)
implements the connection. [`backend/hub.go`](../backend/hub.go) and
[`client/wire.go`](../client/wire.go) implement each side. The message set
exposes three sandboxed tools to the backend and no direct model access to the
client; tests check these limits.

Read [`readme.md`](../readme.md) first for what john is. Read
[`terminalui/readme.md`](../terminalui/readme.md) for what the client draws; this
protocol leaves rendering to the client, as described in
[Events and presentation](#events-and-presentation). Read
[`sandbox/readme.md`](../sandbox/readme.md) for the other link — the one between
the client and the agent inside the sandbox, which is where the tool calls
described here actually execute.

## Communication channels

```
                    ┌──────────── one WebSocket, TLS ────────────┐
                    │                                            │
   ┌────────────────▼─────────────────┐          ┌──────────────▼───────────────┐
   │ client                           │          │ backend                      │
   │                                  │          │                              │
   │  terminalui  ◄── maps events ──  │ ◄── tool.call, message.delta ──         │
   │  sandbox VM  ──  executes ──►    │ ── tool.result, prompt ──►   agent loop │
   │  proxy       ──  enforces        │                              model router│
   └──────────────┬───────────────────┘          └──────────────▲───────────────┘
                  │                                              │
                  └──── GET /api/v1/policy/snapshot (pull) ──────┘
```

The client uses two channels:

- **The session** is a WebSocket, opened when a session starts and closed when
  it ends. It carries the turn: prompts up, tool calls down, results up.
- **Policy** is pulled over ordinary HTTP, on a timer, independent of any
  session. The proxy evaluates against the snapshot locally and never asks the
  backend about an individual request. See [The proxy](#the-proxy).

Everything the developer decides in the moment — an ask-developer approval, a
diff review, a sandbox reset — is settled entirely on the client. None of it
appears below.

## Transport

One WebSocket per session, to `ws(s)://<backend>/api/v1/session`, using
[`github.com/coder/websocket`](https://github.com/coder/websocket).

`Authorization: Bearer <machine token>` on the upgrade request. The token is
what `john login` stored; see [Authentication](#authentication).

Messages are JSON, one per text frame. Every frame is a single flat object: a
`t` discriminator, a `seq`, an `ack`, and the message's own fields spliced in
alongside them.

```json
{"t":"tool.call","seq":41,"ack":17,"id":"c7","tool":"run","args":{"cmd":"go test ./billing/..."}}
```

Binary frames are not used. File contents and command output travel as JSON
strings; anything that is not valid UTF-8 is base64 in a sibling field named
with a `_b64` suffix (`content_b64`).

WebSocket was chosen over SSE-plus-POST because a turn crosses the boundary many
times and the round trip wants one ordered duplex stream, not two half-channels
correlated by hand. The message set is transport-independent — every envelope
also works as an event stream down and posts up — so if a TLS-terminating
middlebox mishandles the upgrade, only this section changes.

### Sequence and acknowledgement

Both directions number their messages. `seq` is monotonic from 1, per session,
per direction. `ack` is the highest contiguous `seq` the sender has received
from its peer, and rides on every message. A message with `seq` 0 is an
unnumbered control frame — the bare `ack` message, and the handshake below.

Each side buffers what it has sent and not seen acknowledged. On reconnect the
peer's `ack` says where to resume from, and everything after it is re-sent in
order. This is the whole of the resume mechanism, and it is symmetric because
either side can be the one that missed something: a client that died after
sending a `tool.result` and a backend that died after sending a `tool.call` are
the same problem, and the test that proves it kills the transport with a message
in flight in each direction and asserts every message arrives exactly once.

A direction that goes quiet while its peer keeps talking still has to let the
peer trim its buffer, so a bare `{"t":"ack","seq":0,"ack":N}` is pushed after a
threshold of unacknowledged frames or a few seconds of silence.

Buffers are bounded (default 1000 messages or 8 MiB per direction, whichever
comes first). A resume that reaches past the buffer is refused with
`session.stale`, and the client opens a fresh session.

### Heartbeats

The client pings the socket every 20s; two missed pings close the transport,
which surfaces as a disconnect. That is how a silently dead backend — not one
that closed cleanly, but one that stopped answering — is noticed, so john fails
closed the moment the backend is unreachable rather than the moment the next
write happens to fail.

## Authentication

`john login` runs the OIDC authorization-code flow with PKCE. Because the
backend is the OIDC client, the CLI never speaks to the identity provider
directly: it asks the backend to start a login (`POST /api/v1/auth/cli/start`),
opens the returned URL in the developer's browser, and polls
(`POST /api/v1/auth/cli/poll`) until the browser half completes and the backend
mints a **machine token**. This uses one registered redirect URI and so works
against any IdP.

The machine token is opaque, stored sha256-hashed on the backend, and kept in
`~/.john/credentials.json` (mode 0600) on the client. It authenticates both the
session WebSocket and the policy pull. The client never holds a model-provider
credential, because it never talks to a model provider — that is the point, and
it is enforced by the absence of any message below that would let it.

The embedded development identity provider ([`auth/devidp`](../auth/devidp)) serves
the same OIDC endpoints locally, so standalone mode and the test suite exercise
the real relying-party path — discovery, code exchange, signature verification —
on every run.

## Events and presentation

The backend says what happened. The client decides what it looks like.

```
backend →   {"t":"tool.call","id":"c7","tool":"run","args":{"cmd":"go test ./billing/..."}}
client      renders ToolStart{Verb:"run", Target:"go test ./billing/..."}, runs it in the sandbox
backend ←   {"t":"tool.result","id":"c7","status":"failed","exit":1,"stdout":"--- FAIL: …"}
```

No message in this protocol names a spinner, a colour, a verb, a truncation
width or a progress bar. `terminalui.ToolStart`, `Busy`, `Thinking` and
`Progress` are the client's translation of these messages; an IDE plugin or a
scripted `john -p` is a second translation rather than a second protocol.

The rule at the edges:

- An **unknown display message** is ignored. A newer backend can narrate more
  than an older client draws, and the session continues. The connection layer
  still honours its `seq`/`ack`.
- An **unknown tool** is an error result with `status:"blocked"`. The client
  never guesses at a capability, because a capability it guesses at is a
  capability nobody governed. A test asserts the dispatch table has exactly
  three entries.

## Session lifecycle

### Opening

```json
→ {"t":"session.open","seq":1,"workspace":"~/src/payments","workspace_key":"9f2c…",
   "client":{"version":"0.4.1","os":"darwin/arm64"},
   "protocol":1,
   "limits":{"max_result_bytes":1048576,"max_concurrent_tools":4,
             "max_attachment_bytes":4194304},
   "sandbox":{"state":"ready"},
   "trace_key":"age1ql3z…"}

← {"t":"session.ready","seq":1,"session":"s_01HX…","policy_epoch":284,
   "model":{"id":"m_codegen","name":"internal/codegen-l","internal":true},
   "narrowed":false,
   "models":[{"id":"m_codegen","name":"internal/codegen-l","internal":true}],
   "limits":{"max_result_bytes":1048576,"max_attachment_bytes":4194304},
   "resumable_for_s":300}
```

`workspace_key` is a stable hash of the absolute working directory and the
machine identity. It is how the backend recognises the same project on the same
laptop across sessions; `workspace` is the display path, sent because the trace
is less useful without it.

The client states its limits and the backend states its own; the effective
limit is the smaller. The backend finds the user's existing session for this
workspace or creates one, and answers with the model it is on and the full set
of models the session may use — so a reconnecting client can redraw the model
picker without guessing.

`trace_key` is this machine's age recipient, the public half of the key pair
the client generated at sign-in. The backend records it and wraps each new
session key for it, so the developer can read their own sessions back. It only
ever holds the public half. A client that sends none still gets a session; it
just cannot read the trace afterwards.

`session` may name a past conversation to carry on with — what `/resume` sends.
Without it the backend takes this workspace's active session, or starts one.

`session.open`, `session.resume`, the key exchange below and their replies
(`session.ready`, `session.stale`, `version.mismatch`, `error`) are exchanged as
unnumbered frames, before the numbered connection is attached; everything after
is numbered and resumable.

### Version

The client and the backend are one program split across two machines, and this
protocol changes. A backend serves only its own build: when `client.version` is
not the backend's, it answers `version.mismatch` instead of `session.ready` and
closes. `session.resume` carries `client` too and is checked the same way, so a
backend upgraded while a client was away does not take it back on the old
build. A client refused mid-session stops the session and asks about the update
there and then; the conversation is the backend's, and `/resume` finds it
again afterwards.

```json
← {"t":"version.mismatch","required":"0.5.2","client":"0.4.1",
   "download":"/releases/john-darwin-arm64"}
```

`download` is the path to the build for the platform the client named in
`client.os`. The client fetches it with its machine token, replaces its own
binary and starts again — after asking, because rewriting a binary somebody
just ran is not something to do silently.

The backend refuses only when it can also supply the fix. A backend built
without bundled client downloads, or one holding no build for that platform,
serves the session rather than locking a developer out with nothing to install.
Neither side refuses when the other is an unstamped development build: there is
no release to send it to. The client makes the same comparison against
`/api/v1/org/public` before it dials, which changes nothing about the guarantee
— the handshake is what enforces it — and only means the question is asked on a
plain terminal rather than under a session that has already drawn itself.

### Unlocking

A conversation the backend has written before comes back locked to it. Each
session is encrypted with its own key, and the backend keeps that key only in
memory — so after a restart, or on any later open, it must ask the machine
that can open it:

```json
← {"t":"session.key.request","seq":1,"session":"s_01HX…","wrapped":"YWdlLWVuY3J5…"}
→ {"t":"session.key","seq":1,"session":"s_01HX…","key":"3n8Q…"}
← {"t":"session.ready", …}
```

`wrapped` is that session's key sealed to the developer's `trace_key`, read
straight out of the trace file's header. The backend cannot open it; it is
handing the developer their own copy. The client unseals it with the private
half on the machine and returns the key, and only then can the backend read
back the conversation it wrote.

**What travels is a key, never a conversation.** The client holds the whole
transcript on screen and could plausibly hand it back instead — and that is
exactly the message this protocol does not have. Every control in the loop
reads the stored history as true, so a client that could supply history could
rewrite it: add a turn that never happened, drop the `model.narrow` that did,
or return content with the replacements undone. The history stays the
backend's to author; the client only holds the key to the safe.

An empty `key` means the client could not open it — a re-keyed machine, a lost
identity. The backend then starts a **fresh** session rather than continuing
one that has silently lost its history, and the client says so. An
administrator can restore access to the old one; see
[trace/readme.md](../trace/readme.md).

This exchange is skipped entirely for a session the backend has never
written, and for a backend with no trace store configured.

### Resuming

```json
→ {"t":"session.resume","seq":1,"session":"s_01HX…","ack":86,"workspace_key":"9f2c…",
   "client":{"version":"0.5.2","os":"darwin/arm64"}}
← {"t":"session.ready","seq":1,"session":"s_01HX…","ack_resume":41, …}       replay follows
← {"t":"session.stale","seq":1,"reason":"replay buffer overflowed while you were away"}
```

Two things resume, and they are worth separating. A **session** resumes so the
developer can reopen john in a directory and find the conversation still there —
the conversation is persisted in the database, so this survives even a backend
restart. `session.ready` says where it stands: `turns_so_far`, `started_at`, and
`context` and `context_window` from its latest model call, so the client can say
it is carrying a conversation on and show how full it is before the next turn. A
reopened conversation runs on the backend's current system prompt, not the one it
began with. `session.open` with `"new": true` is `/new`: the workspace's active
conversation is ended, not deleted, and a new one begins, on the model the last
one was on if it is still allowed. A **turn** resumes only inside the grace window; see
[Disconnection](#disconnection). If the backend refuses the resume with
`session.stale`, the client transparently opens a fresh numbered stream against
the same conversation.

### Closing

`session.close` from either side, `session.closed` in reply with a reason.
`Quit` in the UI, `ctrl-c` at the shell and `/quit` all produce the first.

## A turn

```
→ prompt                       the developer submitted something
← turn.begin                   the loop started
← thinking.delta               transient reasoning (never committed to scrollback)
← message.start/delta/end      a streaming answer
← tool.call                    do this in the sandbox
→ tool.result                  here is what happened
← model.narrow                 a routing keyword was seen — pick what to continue on
→ model.choice                 the developer picked, or declined
← usage                        tokens and spend so far
← turn.end                     completed, interrupted, failed, aborted, lost, or narrow-declined
→ interrupt                    stop, at any point
```

### Upstream (client → backend)

| Message | Fields | Notes |
| --- | --- | --- |
| `prompt` | `turn`, `text`, `attachments[]` | The client allocates the turn id. A prompt typed mid-turn is queued by the UI and delivered as steering. See [Attachments](#attachments). |
| `interrupt` | `turn` | Esc or ctrl-c. The backend stops the loop and answers with `turn.end{reason:"interrupted"}`. Tool calls already in flight are cancelled locally by the client, which still reports their results. |
| `tool.result` | `id`, `status`, tool-specific | One per `tool.call`, always. There is no silent drop. |
| `model.choice` | `id`, `model` or `cancelled` | Answers `model.narrow`. |
| `model.select` | `id`, `model` | The developer's `/model`: continue the session on another model. See [Choosing a model](#choosing-a-model). |
| `proxy.decision` | `at`, `method`, `url`, `verdict`, `policy`, `allowed` | What the proxy did, for the trace. Reported, never asked — see [The proxy](#the-proxy). |
| `sandbox.state` | `state`, `pending_files`, `resources` | Sent on change: image building, ready, reset, files applied. Drives nothing on the backend except the trace. |
| `ack` | — | Bare acknowledgement; see [Sequence and acknowledgement](#sequence-and-acknowledgement). |

### Downstream (backend → client)

| Message | Fields | Notes |
| --- | --- | --- |
| `turn.begin` | `turn` | |
| `thinking.delta` | `turn`, `text` | The whole reasoning so far; replaces the previous. Never committed. |
| `message.start` | `id` | |
| `message.delta` | `id`, `text` | A fragment, not a line. |
| `message.end` | `id` | Markdown; the client wraps it. |
| `tool.call` | `id`, `tool`, `args`, `attempt` | See [Tools](#tools). |
| `tool.cancel` | `id`, `reason` | The backend withdrew a call. The client stops it and reports `status:"cancelled"`. |
| `model.narrow` | `id`, `turn`, `reason`, `policy`, `models[]` | See [Narrowing](#narrowing). |
| `model.selected` | `id`, `model` or `error` | Answers `model.select`. |
| `usage` | `turn`, `tokens_in`, `tokens_out`, `cost_usd` | For the status line. Advisory; the backend's record is authoritative. |
| `notice` | `level`, `text` | Session chatter the backend has a reason to say: which model, that a session was narrowed. |
| `policy.refresh` | `epoch` | The snapshot the client holds is out of date. Pull a new one. Carries no policy content. |
| `turn.end` | `turn`, `reason`, `usage` | `completed`, `interrupted`, `failed`, `aborted`, `lost`. `lost` is a turn the backend was running when it stopped and could not pick up again. |
| `error` | `code`, `message`, `fatal`, `retry_after_s` | |
| `session.closed` | `reason` | |
| `ack` | — | |

## Attachments

A prompt may carry files: `attachments[]`, each `{name, mime_type, data}` with
the bytes base64 in `data`. They are images (`image/png`, `image/jpeg`,
`image/gif`, `image/webp`) and PDFs (`application/pdf`), and nothing else — the
backend keeps its own list of what it will pass to a model, because the client
is the half a developer can edit.

They exist because an `@` mention of a screenshot is not a request to read a
file. For text it is: the model calls `read`, in the sandbox, under every
policy that governs a read. There is nothing useful for `read` to return for a
PNG, and the developer asking about one is showing the model something rather
than sending it to go and look.

Three rules travel with them:

- **Inside the workspace, the file is read through the sandbox** — the same
  path the `read` tool takes, so an excluded path is absent and a symbolic
  link out of the workspace is refused. Outside it, the client reads the
  developer's own file off the disk: a screenshot lives in `~/Desktop`, the
  path has to be typed, and nothing in the loop can ask for one.
- **Whether a prompt may carry one at all is policy**, in the signed snapshot,
  and both ends ask. john's content controls read text: replacements rewrite
  what is on its way to a model and routing keywords hold it back, and neither
  can see inside a picture. The default (`guarded`) allows attachments except
  in a session under one of those controls; an administrator can say `on` or
  `off` instead.
- **`max_attachment_bytes` is negotiated** like the other limits, and it is
  the whole of one prompt rather than one file.

A model that does not accept the type is told a file was left out, by name,
rather than handed bytes it would reject — and the developer is told which
model, because switching model is the answer.

## Tools

Three, and the list is closed:

```json
{"t":"tool.call","id":"c7","tool":"read","args":{
   "path":"billing/client.go","max_bytes":262144}}

{"t":"tool.call","id":"c8","tool":"edit","args":{
   "path":"billing/client.go","find":"return resp, nil // caller decides","replace":"…","all":false}}

{"t":"tool.call","id":"c9","tool":"run","args":{
   "cmd":"go test ./billing/...","cwd":".","timeout_ms":300000}}
```

There is no tool for fetching a URL, spawning a sub-agent or installing a
plugin, and none the agent can use to load tools of its own. MCP servers are not
supported yet; if they are added, the set will come from servers an
administrator approved in the console, never from the agent. Listing a directory and searching a tree are
`run` invocations, which is the correct answer rather than a workaround: they
are commands, they execute in the sandbox, and they pass the same policies as
any other command. A fourth tool is a change to this document and to the threat
model in the readme, not a feature flag — and a test that counts the dispatch
table enforces exactly that.

`edit` carries `find`/`replace` (with `all` to replace every occurrence),
`content` to write a file whole, or `delete:true` to remove one. Paths are
relative to the workspace root and rejected if they escape it, before the
sandbox ever sees them.

### Results

```json
{"t":"tool.result","id":"c9","status":"failed","exit":1,"duration_ms":1180,
 "stdout":"--- FAIL: TestRetry429 (0.02s)\n    client_test.go:88: gave up after 1 attempt, want 4\n",
 "stderr":"","truncated":false}
```

`status` is one of:

- `ok` — it did what it was asked. `read` returns `content` (or `content_b64`).
- `failed` — it ran and failed. A non-zero `exit` is not a protocol error; it is
  the answer, and the loop is expected to reason about it.
- `blocked` — refused before running: an unknown tool, a path outside the root,
  a path a workspace-protection policy covers (`policy` reads
  `workspace protection`, and `stderr` names the rule), or — the backend's own
  check — an `edit` whose arguments carry a replacement alias (see
  [Replacements](#replacements)). `policy` says which.
- `cancelled` — it never finished, because of an interrupt or a `tool.cancel`.

Output over the negotiated `max_result_bytes` is truncated to a head and a tail
with a marker between them, and `truncated` is set.

### Concurrency and idempotency

The backend may have up to `max_concurrent_tools` calls outstanding. The client
executes them and results may arrive in any order; `id` correlates.

Both sides state a number on open and the smaller one governs. Both keep it:
the backend holds a call back rather than sending more than it said it would,
and the client queues one rather than starting more than it agreed to run —
because the client owns the machine the work happens in, and a limit kept only
by the side asking for the work is not a limit. A call cancelled while it is
queued comes back `cancelled`, having never run.

A `tool.call` re-sent after a reconnect carries the same `id` and an incremented
`attempt`. The client keeps the results of the current turn's calls, so a
re-issued call it already completed returns the cached result rather than
running `go test` a second time. This is what makes resuming a turn safe: the
grace window would otherwise be a promise to re-run side effects. The turn's
cache is dropped at `turn.end`.

## Disconnection

The client goes away mid-turn — a lid closes, wifi drops, the process is killed.

The backend **parks** the turn: it stops issuing work, holds the pending
`tool.call`, and keeps the session in memory for `resumable_for_s` (default
300). A client that reconnects inside the window sends `session.resume`, the
buffered messages replay, the pending call is re-issued with an incremented
`attempt`, and the turn continues as though nothing happened.

Outside the window the turn ends and the session survives without it. Reopening
john in the directory shows the conversation up to where it stopped — it is in
the database — and the sandbox still holds whatever the turn managed to write,
because the sandbox is on the laptop and never went anywhere.

### Backend restart

A backend restart clears in-memory sessions. The conversation is checkpointed
at every message boundary, and the session row records the running turn.

So the client reconnects, is told its session is stale, opens a fresh one, and
the backend reads that record and carries the turn on: `turn.begin` with the id
the old process gave it, and the run continues from the conversation's last
entry. Outstanding tool calls are not re-issued — agentkit answers unanswered
calls as interrupted, so the model is told the work stopped and decides what to
do about it, rather than a `run` command being executed a second time by a
backend nobody asked.

A turn it cannot pick up — a conversation whose last word is a request for
tools that were never answered, which is what a hard kill leaves — ends as
`turn.end{reason:"lost"}`. The client stops waiting and the developer asks
again. The client stops the spinner when the turn ends.

During reconnection, the status line reports that the backend is unreachable
and new prompts are refused.

## Choosing a model

A session starts on the first model the developer's policy groups allow. The
developer can move it to another with `/model`, which sends `model.select`;
`session.ready` lists the models it may choose from.

```json
→ {"t":"model.select","id":"s1","model":"m_haiku"}
← {"t":"model.selected","id":"s1","model":{"id":"m_haiku","name":"Claude Haiku 5.5"}}
← {"t":"model.selected","id":"s1","error":"a turn is running · switch when it ends"}
```

The backend decides, not the client. The model has to be one the developer's
groups allow, and one any narrowing of the session left in place, so `/model`
is never a way around a routing policy. It is refused while a turn is running,
so a turn runs on one model from start to finish. The choice is persisted with
the session and recorded in the trace.

## Narrowing

A routing policy's keyword is matched by the backend against everything it is
about to send to a model — the prompt, file contents a `read` returned, the
output of a `run` — **before** the request leaves for a provider. The match
happens in the streamer that wraps every model call, so there is no path around
it.

```json
← {"t":"model.narrow","id":"n1","turn":"t4",
   "reason":"keyword \"C++\" seen in what was about to reach a model",
   "policy":"narrowed by policy group payments-restricted",
   "models":[{"id":"m_codegen","name":"internal/codegen-l","internal":true,
              "detail":"hosted in-organization"}]}

→ {"t":"model.choice","id":"n1","model":"m_codegen"}
→ {"t":"model.choice","id":"n1","cancelled":true}
```

The order is the entire control. The keyword scan returns before dispatch, the
backend holds the request and asks, switches the session's model to the choice,
and only then continues the turn — so the content that tripped the keyword
reaches only the permitted model. A test asserts no model was called before the
choice arrived.

A cancelled choice ends the turn with `reason:"aborted"`. There is nothing safe
to fall back to: the keyword has been seen, and that cannot be taken back.

The session is narrowed on the backend, permanently — the narrowing is
persisted — and every later turn is subject to it. A client that reconnects to a
narrowed session is told so in `session.ready`, and there is no message that
widens one.

## Replacements

Replacement policies run on the backend, in the same streamer, on everything on
the way to a model — prompts, file contents a `read` returned, `run` output. The
client never sees them run and has nothing to opt out of, which is the property
that makes them worth anything.

A `#` in the replacement becomes a stable alias per distinct matched value, so
one account number is always the same placeholder and two different ones never
collide.

**The alias-in-tool-arguments question is resolved, conservatively.** When the
model, having only ever seen `ACCT-#3`, writes an `edit` whose arguments contain
that alias, the backend refuses the call — the client is never asked to run it,
and the model gets a `blocked` result it can reason about. Substituting the real
value back into a file on the strength of the model's output would be a write
primitive built out of a redaction feature; refusing turns a file containing a
matched value into one the agent cannot edit, which is the correct trade for v1.
Region-level tracking could lift the restriction later without a protocol
change.

## The proxy

The proxy runs on the client, outside the sandbox, and it does not appear in the
session protocol as a question. It gets its policy by pulling a snapshot, and it
decides alone.

```
GET /api/v1/policy/snapshot
Authorization: Bearer <machine token>

200 {"snapshot":"<json>","sig":"<base64 Ed25519 over the snapshot bytes>"}
```

The snapshot carries an `epoch`, an `issued_at`, an `expires_at`, the network
and workspace policies, and whether replacements are active. The client pins the
backend's signing public key at login (`GET /api/v1/policy/signing-key`) and
refuses any snapshot that does not verify against it — a snapshot loosened on
disk is not one the proxy will honour. It refreshes at 60% of the lifetime and
on a `policy.refresh` push, which the backend sends to every live session the
moment anything moves the policy epoch — so an approved access request works on
the developer's next attempt rather than at their next refresh. Past `expires_at` the snapshot is dead: every
request that needs a policy decision is blocked, and the status line says why.
That is the readme's fail-closed rule expressed as an expiry, so it holds even
when the backend is up and the client's clock is the thing that drifted.

The proxy is a full TLS-terminating MITM: a certificate authority is generated
on the developer's machine, installed into the sandbox's trust store, and used
to mint a leaf per host. The proxy buffers each request whole, evaluates it
against the snapshot, and produces one of four outcomes:

- **always allow** — forwarded, drawn as a `ProxyDecision`.
- **ask developer** — the buffered request goes to the approval inspector. The
  developer answers about that one request. `ApprovalRequest` is a `terminalui`
  event, not a protocol message.
- **deny** — refused. An administrator already decided; nobody is asked again.
- **unknown** — nothing matched, so it is refused the moment it is made. The
  client then offers to file a network access request — an ordinary
  `POST /api/v1/access-requests` — which never unblocks the request that failed.

When several policies match, the most restrictive wins. Protocol upgrades,
oversized bodies, and other request forms that would defeat inspection are
refused. Every decision made during a session is reported up as `proxy.decision`
so it lands in the trace and the console's network-request record.

## Errors

| Code | Meaning |
| --- | --- |
| `unauthenticated` | Token missing, expired or revoked. Fatal; the client tells the developer to run `john login`. |
| `protocol_unsupported` | The backend cannot serve the requested `protocol`. Fatal. |
| `session_unknown` | Resume for a session the backend has never heard of, or a `/resume` naming a session that is not the caller's. |
| `session_stale` | Resume past the replay buffer or the grace window. The client opens a fresh session. |
| `policy_unavailable` | The backend cannot produce a policy decision. Every dependent operation blocks. |
| `no_model` | Every model the session could use has been removed by policy. Fatal for the turn. |
| `setup_required` | The backend has not completed `--setup`. |
| `sandbox_unavailable` | The client reported it has no working sandbox. |
| `rate_limited` | With `retry_after_s`. |
| `internal` | Everything else. Never carries provider detail; the trace carries that. |

Errors are messages, not connection closes. The connection closes only when the
error is `fatal`, and then with a `session.closed` carrying the same code.

## Tests

The property that matters is not that the messages serialize. It is that the
client cannot reach a model and the backend cannot reach the developer's disk,
and both are tested as absences and as behaviour:

- The tool dispatch table has exactly three entries, asserted by a test that
  names this document.
- A `tool.call` with a path outside the workspace root is rejected by the client
  before the sandbox, and by the sandbox mount regardless.
- Reconnect mid-turn, with a `tool.result` in flight in each direction, is the
  test that the sequence-and-ack design actually works.
- The PTY session tests drive a real terminal against a real backend over a real
  WebSocket, with only the model scripted — so a streamed reply, a tool
  round-trip, an edit-review-apply, an interrupt, and a fail-closed outage are
  all exercised end to end.
- The in-loop controls are proven through the same wire: a replacement's secret
  never appears in any model request, an alias in an `edit` comes back blocked,
  and a keyword narrows the session before any model is called.
- The key exchange is tested as a property, not a handshake: after the backend
  has forgotten everything, a client that returns its key carries the
  conversation on with the model seeing the history, and a client that cannot
  gets a fresh session rather than an empty one wearing the old one's name.
  Nothing the backend persisted — trace file, session state, database — holds a
  word of the conversation in the clear.
