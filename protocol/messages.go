// Package protocol defines messages exchanged by the John client and backend.
// Conn provides an ordered, resumable, acknowledged stream over a framed
// transport.
//
// The backend runs the agent loop and requests three sandboxed tools: read,
// edit, and run. The protocol exposes no direct model access to the client.
//
// Messages describe operations and events. The client controls presentation.
package protocol

import (
	"encoding/json"
	"fmt"
)

// Version is the protocol version spoken by this build. A backend that cannot
// serve the version a client asks for says so instead of degrading silently.
const Version = 1

// Message is one protocol message. Concrete types below; the T method returns
// the wire discriminator.
type Message interface {
	T() string
}

// --- shared shapes ---

// ClientInfo identifies the client build on session open.
type ClientInfo struct {
	Version string `json:"version"`
	OS      string `json:"os"`
}

// Limits are stated by both sides on open; the effective limit is the smaller.
type Limits struct {
	MaxResultBytes     int `json:"max_result_bytes,omitempty"`
	MaxConcurrentTools int `json:"max_concurrent_tools,omitempty"`
	// MaxAttachmentBytes limits each prompt attachment before base64 encoding.
	// Both sides negotiate this limit when the session opens.
	MaxAttachmentBytes int `json:"max_attachment_bytes,omitempty"`
}

// Negotiate returns the smaller stated limit for each field.
//
// Both sides use this calculation. The client enforces the result, and the
// backend reports it in session.ready.
//
// Zero means no limit was stated, so the other side's limit applies. Two
// zeroes remain zero; the caller decides how to handle an unspecified limit.
func (l Limits) Negotiate(other Limits) Limits {
	return Limits{
		MaxResultBytes:     smallerLimit(l.MaxResultBytes, other.MaxResultBytes),
		MaxConcurrentTools: smallerLimit(l.MaxConcurrentTools, other.MaxConcurrentTools),
		MaxAttachmentBytes: smallerLimit(l.MaxAttachmentBytes, other.MaxAttachmentBytes),
	}
}

func smallerLimit(a, b int) int {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case b < a:
		return b
	}
	return a
}

// ModelInfo describes a model to the client, for display only. The backend's
// record is authoritative; a client cannot choose a model it was not handed.
type ModelInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	AssignedBy string `json:"assigned_by,omitempty"`
	Internal   bool   `json:"internal,omitempty"`
}

// Resources are the sandbox limits, reported for the trace.
type Resources struct {
	CPUs      int `json:"cpus,omitempty"`
	MemoryGB  int `json:"memory_gb,omitempty"`
	DiskGB    int `json:"disk_gb,omitempty"`
	Processes int `json:"processes,omitempty"`
}

// SandboxInfo is the client's description of its sandbox on open and change.
type SandboxInfo struct {
	State     string     `json:"state"` // none | building | ready
	Image     string     `json:"image,omitempty"`
	Resources *Resources `json:"resources,omitempty"`
}

// --- session lifecycle (client → backend, backend → client) ---

// SessionOpen starts a fresh session, or reopens a named one.
type SessionOpen struct {
	Workspace    string      `json:"workspace"`     // display path; the trace is less useful without it
	WorkspaceKey string      `json:"workspace_key"` // stable hash of abs path + machine identity
	Client       ClientInfo  `json:"client"`
	Protocol     int         `json:"protocol"`
	Limits       Limits      `json:"limits"`
	Sandbox      SandboxInfo `json:"sandbox"`

	// Session names a past conversation to carry on with, as `/resume` does.
	// Empty takes this workspace's active session, or starts a new one.
	Session string `json:"session,omitempty"`

	// New starts a new conversation even when this workspace has an active
	// one, as `/new` does. The active one is ended, not deleted: /resume
	// still lists it.
	New bool `json:"new,omitempty"`

	// TraceKey is the client's age recipient. The backend wraps each new
	// session key for it so the developer can read their own sessions back,
	// and holds only this public half. A client that sends none still gets a
	// session; it just cannot read the trace afterwards.
	TraceKey string `json:"trace_key,omitempty"`
}

// SessionResume reattaches to a parked session. Ack is the highest contiguous
// seq the client received from the backend; the backend replays from there.
type SessionResume struct {
	Session      string `json:"session"`
	WorkspaceKey string `json:"workspace_key"`
	Ack          uint64 `json:"ack"`
	// Client is checked as on session.open: a backend upgraded while the
	// client was away must not take it back on the old build. Absent from
	// clients before v0.0.2, which are let through as before.
	Client ClientInfo `json:"client"`
}

// SessionReady answers SessionOpen or SessionResume. On resume it also
// carries Ack — the highest contiguous seq the backend received from the
// client — so the client knows what to re-send.
type SessionReady struct {
	Session   string `json:"session"`
	StartedAt string `json:"started_at,omitempty"` // RFC 3339; when the conversation began
	// Context and ContextWindow are as in Usage, for the conversation as it
	// stands, so a reopened session shows how full it is before its next
	// turn rather than nothing.
	Context       int         `json:"context,omitempty"`
	ContextWindow int         `json:"context_window,omitempty"`
	PolicyEpoch   uint64      `json:"policy_epoch"`
	Model         ModelInfo   `json:"model"`
	Narrowed      bool        `json:"narrowed"`
	Models        []ModelInfo `json:"models,omitempty"` // the allowed set, so a reconnect redraws truthfully
	Limits        Limits      `json:"limits"`
	ResumableForS int         `json:"resumable_for_s"`
	Ack           uint64      `json:"ack_resume,omitempty"`
	// TurnsSoFar is the highest turn number this session has used. The
	// client numbers its turns, and one carrying on a session it did not
	// start counts on from here rather than from t1 again.
	TurnsSoFar int `json:"turns_so_far,omitempty"`
}

// VersionMismatch is the backend refusing a client that is not its own build.
// The client and the backend are one program split across two machines, and
// the wire between them changes; running two versions of it against each
// other is a class of bug nobody should have to debug, so the backend does
// not serve a session to a client it did not ship.
//
// It is sent instead of SessionReady and it is the end of that connection.
// The client is expected to fetch Download, replace itself and start again —
// which is why the backend only refuses when it actually has that build to
// give: a refusal a client cannot act on would be a locked door with no key.
type VersionMismatch struct {
	Required string `json:"required"` // the version this backend serves
	Client   string `json:"client"`   // what the client said it was
	Download string `json:"download"` // path to the client build for its platform
}

// SessionKeyRequest asks the client to unlock a session the backend cannot
// read. It carries that session's key wrapped for the developer, straight out
// of the trace file's header; the backend cannot open it. This is answered
// inside the handshake, before SessionReady, so there is never a moment when
// a turn could run against a conversation the backend has not opened yet.
type SessionKeyRequest struct {
	Session string `json:"session"`
	Wrapped string `json:"wrapped"` // base64 age ciphertext of the session key
}

// SessionKey answers SessionKeyRequest with the unwrapped key, base64. An
// empty Key means the client could not open it — the developer's identity is
// gone or was never the one this session was wrapped for — and the backend
// answers by starting a fresh session rather than a silently empty one.
type SessionKey struct {
	Session string `json:"session"`
	Key     string `json:"key,omitempty"`
}

// SessionStale refuses a resume: past the grace window or the replay buffer.
// The client opens a fresh session.
type SessionStale struct {
	Reason string `json:"reason"`
}

// SessionClose is sent by either side to end the session deliberately.
type SessionClose struct {
	Reason string `json:"reason,omitempty"`
}

// SessionClosed confirms the end, with the reason that stands.
type SessionClosed struct {
	Reason string `json:"reason"`
}

// --- a turn, upstream (client → backend) ---

// Prompt is the developer's submission. The client allocates the turn id.
type Prompt struct {
	Turn string `json:"turn"`
	Text string `json:"text"`

	// Attachments are files the developer named in the prompt and wants the
	// model to look at rather than read: an image or a PDF. They travel with
	// the prompt because they are part of what the developer said, and they
	// are the one thing on this wire that reaches a model without having been
	// read by a tool call.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is one file carried with a prompt.
//
// The name is what the developer called it, and it is shown to the model:
// "the failing screenshot" is not a useful thing to say about an untitled
// image, and a model that can name the file can ask about it. Data is the
// file itself, base64, because this wire is JSON and these bytes are not
// text — the same reason ToolResult has a ContentB64.
//
// Whether a prompt may carry one of these at all is a policy question, not a
// protocol one: see policy/attachments.go. Both ends ask, and the backend's
// answer is the one that decides.
type Attachment struct {
	Name     string `json:"name"`
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

// Interrupt stops the current turn. The backend answers with
// TurnEnd{Reason:"interrupted"}.
type Interrupt struct {
	Turn string `json:"turn"`
}

// ToolResult answers exactly one ToolCall. There is no silent drop.
type ToolResult struct {
	ID         string `json:"id"`
	Status     string `json:"status"` // ok | failed | blocked | cancelled
	Exit       int    `json:"exit,omitempty"`
	DurationMS int    `json:"duration_ms,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	Content    string `json:"content,omitempty"`     // read results; valid UTF-8
	ContentB64 string `json:"content_b64,omitempty"` // read results that are not
	Truncated  bool   `json:"truncated,omitempty"`
	Policy     string `json:"policy,omitempty"` // which policy blocked it
}

// ModelChoice answers ModelNarrow: the developer picked, or declined.
type ModelChoice struct {
	ID        string `json:"id"`
	Model     string `json:"model,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// ModelSelect asks for the session to continue on another model: the
// developer's /model. The backend answers with ModelSelected.
type ModelSelect struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

// ProxyDecision reports what the proxy did, for the trace. Reported, never
// asked: the proxy decides alone against its snapshot.
type ProxyDecision struct {
	At      string `json:"at"`
	Method  string `json:"method"`
	URL     string `json:"url"`
	Verdict string `json:"verdict"` // allowed | asked | denied | unknown
	Policy  string `json:"policy,omitempty"`
	Allowed bool   `json:"allowed"`
	// Error is why an allowed request failed anyway: the origin could not
	// be reached, or the response broke off on its way to the sandbox.
	Error string `json:"error,omitempty"`
	// Rejected is the access request whose rejection refused this request,
	// when that is what did. The backend tells the model so, rather than
	// that no policy covers the host and the developer can approve it.
	Rejected string `json:"rejected,omitempty"`
}

// SandboxState is sent on change: image building, ready, reset, applied.
// Drives nothing on the backend except the trace.
type SandboxState struct {
	State        string     `json:"state"`
	PendingFiles int        `json:"pending_files,omitempty"`
	Resources    *Resources `json:"resources,omitempty"`
}

// --- a turn, downstream (backend → client) ---

// TurnBegin says the loop started on a prompt.
type TurnBegin struct {
	Turn string `json:"turn"`
}

// ThinkingDelta is transient reasoning. Replaces the previous one; never
// committed to scrollback.
type ThinkingDelta struct {
	Turn string `json:"turn"`
	Text string `json:"text"`
}

// MessageStart begins a streaming assistant message.
type MessageStart struct {
	ID string `json:"id"`
}

// MessageDelta is a fragment, not a line.
type MessageDelta struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// MessageEnd completes a streaming assistant message. Markdown; the client
// wraps it.
type MessageEnd struct {
	ID string `json:"id"`
}

// ToolCall asks the client to run one of the three tools in the sandbox.
// A call re-sent after a reconnect carries the same ID and an incremented
// Attempt; the client answers a call it already completed from its per-turn
// result cache rather than running it again.
type ToolCall struct {
	ID      string          `json:"id"`
	Tool    string          `json:"tool"` // read | edit | run — the list is closed
	Args    json.RawMessage `json:"args"`
	Attempt int             `json:"attempt,omitempty"`
}

// ToolCancel withdraws a call. The client stops it and reports
// status "cancelled".
type ToolCancel struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// ModelNarrow says a routing keyword was seen in something about to go to a
// model. The request is being held; the developer picks from Models or
// declines, and declining ends the turn. A session never un-narrows.
type ModelNarrow struct {
	ID     string      `json:"id"`
	Turn   string      `json:"turn"`
	Reason string      `json:"reason"`
	Policy string      `json:"policy"`
	Models []ModelInfo `json:"models"`
}

// ModelSelected answers ModelSelect: the model the session is now on, or
// why it is not. Policy has the last word: only a model the developer's
// groups allow, and that no routing policy has narrowed the session away
// from, is ever selected, and never in the middle of a turn.
type ModelSelected struct {
	ID    string    `json:"id"`
	Model ModelInfo `json:"model"`
	Error string    `json:"error,omitempty"`
}

// Usage feeds the status line. Advisory; the backend's record is
// authoritative.
type Usage struct {
	Turn      string  `json:"turn"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	// Context is how much of the model's context the latest call used — its
	// whole prompt, cached or not, and its answer — and ContextWindow how
	// much there is. The totals above are summed over every call in a turn,
	// each of which resends the conversation, so they say what a turn cost
	// and not how long the conversation has grown.
	Context       int `json:"context,omitempty"`
	ContextWindow int `json:"context_window,omitempty"`
}

// Notice is session chatter the backend has a reason to say: which model,
// which group assigned it, that replacements are active.
type Notice struct {
	Level string `json:"level"` // info | warn | error
	Text  string `json:"text"`
}

// PolicyRefresh says the snapshot the client holds is out of date. Pull a new
// one. Carries no policy content.
type PolicyRefresh struct {
	Epoch uint64 `json:"epoch"`
	// What the change was about: network, workspace, routing, replacements,
	// models, builds, prompts or groups. Empty is any of them, which is all a
	// backend from before the field could say. It is here so the developer
	// is told what changed: "policies updated" for a model an administrator
	// added was a policy change that never happened.
	What []string `json:"what,omitempty"`
}

// AccessDecision tells a developer that an access request they filed was
// decided. A rejection writes no policy, so nothing else would reach them:
// the next attempt was blocked exactly as before anyone had looked, and the
// screen said only "no matching policy".
type AccessDecision struct {
	Ref    string `json:"ref"`
	Title  string `json:"title"`
	Host   string `json:"host,omitempty"`
	Status string `json:"status"` // approved | rejected
}

// TurnEnd finishes a turn: completed, interrupted, failed, aborted, or lost.
//
// Lost is a turn the backend was running when it stopped and could not pick
// up again when it came back — the conversation is intact and the developer
// asks again. A turn it could pick up says nothing here at all; it simply
// carries on, beginning with a TurnBegin the client may never have seen the
// other half of.
type TurnEnd struct {
	Turn   string `json:"turn"`
	Reason string `json:"reason"`
	Usage  *Usage `json:"usage,omitempty"`
}

// Error is a message, not a connection close. The connection closes only when
// Fatal, and then with a SessionClosed carrying the same code.
type Error struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Fatal       bool   `json:"fatal,omitempty"`
	RetryAfterS int    `json:"retry_after_s,omitempty"`
}

// Error codes.
const (
	ErrUnauthenticated     = "unauthenticated"
	ErrProtocolUnsupported = "protocol_unsupported"
	ErrSessionUnknown      = "session_unknown"
	ErrSessionStale        = "session_stale"
	ErrPolicyUnavailable   = "policy_unavailable"
	ErrNoModel             = "no_model"
	ErrRateLimited         = "rate_limited"
	ErrSetupRequired       = "setup_required"
	ErrSandboxUnavailable  = "sandbox_unavailable"
	ErrInternal            = "internal"
)

// Ack is a bare acknowledgement, sent when a direction has nothing else to
// say but the peer's replay buffer needs advancing.
type Ack struct{}

// --- discriminators ---

func (SessionOpen) T() string       { return "session.open" }
func (SessionResume) T() string     { return "session.resume" }
func (SessionReady) T() string      { return "session.ready" }
func (SessionKeyRequest) T() string { return "session.key.request" }
func (SessionKey) T() string        { return "session.key" }
func (SessionStale) T() string      { return "session.stale" }
func (VersionMismatch) T() string   { return "version.mismatch" }
func (SessionClose) T() string      { return "session.close" }
func (SessionClosed) T() string     { return "session.closed" }
func (Prompt) T() string            { return "prompt" }
func (Interrupt) T() string         { return "interrupt" }
func (ToolResult) T() string        { return "tool.result" }
func (ModelChoice) T() string       { return "model.choice" }
func (ModelSelect) T() string       { return "model.select" }
func (ProxyDecision) T() string     { return "proxy.decision" }
func (SandboxState) T() string      { return "sandbox.state" }
func (TurnBegin) T() string         { return "turn.begin" }
func (ThinkingDelta) T() string     { return "thinking.delta" }
func (MessageStart) T() string      { return "message.start" }
func (MessageDelta) T() string      { return "message.delta" }
func (MessageEnd) T() string        { return "message.end" }
func (ToolCall) T() string          { return "tool.call" }
func (ToolCancel) T() string        { return "tool.cancel" }
func (ModelNarrow) T() string       { return "model.narrow" }
func (ModelSelected) T() string     { return "model.selected" }
func (Usage) T() string             { return "usage" }
func (Notice) T() string            { return "notice" }
func (PolicyRefresh) T() string     { return "policy.refresh" }
func (AccessDecision) T() string    { return "access.decision" }
func (TurnEnd) T() string           { return "turn.end" }
func (Error) T() string             { return "error" }
func (Ack) T() string               { return "ack" }

// decoders maps a discriminator to a constructor for its pointer type, which
// json.Unmarshal fills. An unknown discriminator is not an error at this
// layer: a newer backend may narrate more than an older client understands,
// and the rule at the edges is that unknown display messages are ignored.
var decoders = map[string]func() Message{
	"session.open":        func() Message { return &SessionOpen{} },
	"session.resume":      func() Message { return &SessionResume{} },
	"session.ready":       func() Message { return &SessionReady{} },
	"session.key.request": func() Message { return &SessionKeyRequest{} },
	"session.key":         func() Message { return &SessionKey{} },
	"session.stale":       func() Message { return &SessionStale{} },
	"version.mismatch":    func() Message { return &VersionMismatch{} },
	"session.close":       func() Message { return &SessionClose{} },
	"session.closed":      func() Message { return &SessionClosed{} },
	"prompt":              func() Message { return &Prompt{} },
	"interrupt":           func() Message { return &Interrupt{} },
	"tool.result":         func() Message { return &ToolResult{} },
	"model.choice":        func() Message { return &ModelChoice{} },
	"model.select":        func() Message { return &ModelSelect{} },
	"proxy.decision":      func() Message { return &ProxyDecision{} },
	"sandbox.state":       func() Message { return &SandboxState{} },
	"turn.begin":          func() Message { return &TurnBegin{} },
	"thinking.delta":      func() Message { return &ThinkingDelta{} },
	"message.start":       func() Message { return &MessageStart{} },
	"message.delta":       func() Message { return &MessageDelta{} },
	"message.end":         func() Message { return &MessageEnd{} },
	"tool.call":           func() Message { return &ToolCall{} },
	"tool.cancel":         func() Message { return &ToolCancel{} },
	"model.narrow":        func() Message { return &ModelNarrow{} },
	"model.selected":      func() Message { return &ModelSelected{} },
	"usage":               func() Message { return &Usage{} },
	"notice":              func() Message { return &Notice{} },
	"policy.refresh":      func() Message { return &PolicyRefresh{} },
	"access.decision":     func() Message { return &AccessDecision{} },
	"turn.end":            func() Message { return &TurnEnd{} },
	"error":               func() Message { return &Error{} },
	"ack":                 func() Message { return &Ack{} },
}

// header is the part of every frame the Conn layer reads.
type header struct {
	T   string `json:"t"`
	Seq uint64 `json:"seq"`
	Ack uint64 `json:"ack"`
}

// encodeFrame flattens a message and its header into one JSON object:
// {"t":"prompt","seq":3,"ack":17,"text":"…"}.
func encodeFrame(m Message, seq, ack uint64) ([]byte, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode %s: %w", m.T(), err)
	}
	head, err := json.Marshal(header{T: m.T(), Seq: seq, Ack: ack})
	if err != nil {
		return nil, err
	}
	if len(body) <= 2 || string(body) == "null" { // empty object
		return head, nil
	}
	// head is {...}, body is {...}; splice body's fields into head.
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head[:len(head)-1]...)
	out = append(out, ',')
	out = append(out, body[1:]...)
	return out, nil
}

// ErrUnknownMessage is returned by decodeFrame for a discriminator this build
// does not know. The Conn skips such frames (still honouring their seq/ack);
// message-level policy for unknown tools lives above this layer.
type ErrUnknownMessage struct{ Discriminator string }

func (e ErrUnknownMessage) Error() string {
	return "protocol: unknown message " + e.Discriminator
}

// decodeFrame parses one frame into its header and typed message.
func decodeFrame(data []byte) (header, Message, error) {
	var h header
	if err := json.Unmarshal(data, &h); err != nil {
		return h, nil, fmt.Errorf("protocol: bad frame: %w", err)
	}
	mk, ok := decoders[h.T]
	if !ok {
		return h, nil, ErrUnknownMessage{Discriminator: h.T}
	}
	m := mk()
	if err := json.Unmarshal(data, m); err != nil {
		return h, nil, fmt.Errorf("protocol: decode %s: %w", h.T, err)
	}
	return h, m, nil
}
