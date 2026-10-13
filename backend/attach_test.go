package backend

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/oliverkofoed/gokit/agentkit/llm"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/policy"
	"github.com/popstarapps/john/protocol"
	"github.com/popstarapps/john/store"
)

// addLookingModel is addModel for a model that can be shown things.
func (h *controlHarness) addLookingModel(id, name string, vision, documents bool) {
	h.t.Helper()
	lm, _ := json.Marshal(llm.Model{ID: id, Name: name, API: llm.OpenAIChat,
		Provider: "selfhosted", BaseURL: "https://llm.internal/v1",
		Cost: llm.Cost{Input: 1, Output: 2}, Vision: vision, Documents: documents})
	if err := h.store.CreateModel(&store.Model{
		ID: "m_" + id, Name: name, Provider: "selfhosted", Hosting: "internal",
		ModelID: id, LLMModel: string(lm),
	}); err != nil {
		h.t.Fatal(err)
	}
}

var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
var pdfBytes = []byte("%PDF-1.7\nnot really a pdf\n")

func attachedPrompt(text string) *protocol.Prompt {
	return &protocol.Prompt{Turn: "t1", Text: text, Attachments: []protocol.Attachment{
		{Name: "shot.png", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(pngBytes)},
		{Name: "spec.pdf", MimeType: "application/pdf", Data: base64.StdEncoding.EncodeToString(pdfBytes)},
	}}
}

// blocksOf flattens every block of every message in the first request a model
// was asked to answer — what actually reached the provider, normalized.
func blocksOf(t *testing.T, fake *llmtest.Fake) []llm.Block {
	t.Helper()
	reqs := fake.Requests()
	if len(reqs) == 0 {
		t.Fatal("the model was never called")
	}
	var out []llm.Block
	for _, m := range reqs[0].Messages {
		out = append(out, m.Blocks...)
	}
	return out
}

func warned(seen []protocol.Message, substr string) bool {
	for _, m := range seen {
		if n, ok := m.(*protocol.Notice); ok && strings.Contains(n.Text, substr) {
			return true
		}
	}
	return false
}

// TestAnAttachmentReachesTheModel is the whole feature in one assertion: the
// bytes a developer named travel with the prompt and arrive as content, not
// as a path for a tool call that would have nothing useful to return.
func TestAnAttachmentReachesTheModel(t *testing.T) {
	fake := llmtest.New(llmtest.Text("I see it."))
	h := newControlHarness(t, fake)
	h.addLookingModel("codegen-l", "internal/codegen-l", true, true)
	_, token := h.user()
	h.connect(token)

	reason, _ := h.drivePrompt(attachedPrompt("what is wrong here?"), nil)
	if reason != "completed" {
		t.Fatalf("turn reason = %s", reason)
	}

	blocks := blocksOf(t, fake)
	var image, document *llm.Block
	for i, b := range blocks {
		switch b.Type {
		case llm.BlockImage:
			image = &blocks[i]
		case llm.BlockDocument:
			document = &blocks[i]
		}
	}
	if image == nil || document == nil {
		t.Fatalf("the model saw no attachment: %+v", blocks)
	}
	if image.MimeType != "image/png" || image.Data != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Errorf("image block = %+v", *image)
	}
	if document.MimeType != "application/pdf" || document.Name != "spec.pdf" ||
		document.Data != base64.StdEncoding.EncodeToString(pdfBytes) {
		t.Errorf("document block = %+v", *document)
	}
}

// TestAModelThatCannotReadOneIsToldOneWasLeftOut: agentkit downgrades rather
// than sending bytes a provider would reject, and the developer is told which
// model, because switching model is the answer.
func TestAModelThatCannotReadOneIsToldOneWasLeftOut(t *testing.T) {
	fake := llmtest.New(llmtest.Text("I cannot see it."))
	h := newControlHarness(t, fake)
	h.addLookingModel("textonly", "internal/textonly", false, false)
	_, token := h.user()
	h.connect(token)

	if reason, seen := h.drivePrompt(attachedPrompt("look"), nil); reason != "completed" {
		t.Fatalf("turn reason = %s", reason)
	} else if !warned(seen, "does not see images") || !warned(seen, "does not read documents") {
		t.Fatalf("the developer was not told the model cannot read them: %+v", seen)
	}
	var text []string
	for _, b := range blocksOf(t, fake) {
		if b.Type == llm.BlockImage || b.Type == llm.BlockDocument {
			t.Fatalf("bytes were sent to a model that cannot read them: %+v", b)
		}
		text = append(text, b.Text)
	}
	joined := strings.Join(text, "\n")
	if !strings.Contains(joined, "[image omitted]") || !strings.Contains(joined, "[document spec.pdf omitted]") {
		t.Fatalf("the model was not told a file was left out: %q", joined)
	}
}

// TestASessionThatRedactsWillNotCarryAPicture is the reason this is a policy
// question at all: a replacement rewrites what is on its way to a model by
// reading it, and there is nothing to read in a PNG.
func TestASessionThatRedactsWillNotCarryAPicture(t *testing.T) {
	fake := llmtest.New(llmtest.Text("ok"))
	h := newControlHarness(t, fake)
	h.addLookingModel("codegen-l", "internal/codegen-l", true, true)
	userID, token := h.user()

	g, _ := h.store.CreatePolicyGroup("redaction", "", true, userID)
	spec, _ := json.Marshal(map[string]any{"kind": "replacement",
		"pattern": `ACCT-\d+`, "replacement": "ACCT-#"})
	h.store.CreatePolicy(g.ID, "redact accounts", "", spec, userID)

	h.connect(token)
	_, seen := h.drivePrompt(attachedPrompt("look at this"), nil)
	for _, b := range blocksOf(t, fake) {
		if b.Type == llm.BlockImage || b.Type == llm.BlockDocument {
			t.Fatalf("an attachment walked around the replacement policy: %+v", b)
		}
	}
	if !warned(seen, "attachments are not allowed here") {
		t.Fatalf("the refusal was silent: %+v", seen)
	}
}

// TestAnAdministratorCanAllowThemAnyway: guarded is a default, not a law.
func TestAnAdministratorCanAllowThemAnyway(t *testing.T) {
	fake := llmtest.New(llmtest.Text("ok"))
	h := newControlHarness(t, fake)
	h.addLookingModel("codegen-l", "internal/codegen-l", true, true)
	userID, token := h.user()

	g, _ := h.store.CreatePolicyGroup("redaction", "", true, userID)
	spec, _ := json.Marshal(map[string]any{"kind": "replacement",
		"pattern": `ACCT-\d+`, "replacement": "ACCT-#"})
	h.store.CreatePolicy(g.ID, "redact accounts", "", spec, userID)

	org, err := h.store.Org()
	if err != nil {
		t.Fatal(err)
	}
	org.Attachments = policy.AttachmentsOn
	if err := h.store.SaveOrg(org); err != nil {
		t.Fatal(err)
	}

	h.connect(token)
	h.drivePrompt(attachedPrompt("look at this"), nil)
	for _, b := range blocksOf(t, fake) {
		if b.Type == llm.BlockImage {
			return
		}
	}
	t.Fatalf("the administrator's decision was not honoured: %+v", blocksOf(t, fake))
}

// TestTheBackendKeepsItsOwnListOfWhatItWillSend: the client has a list too,
// and they agree — but the client is the half a developer can edit.
func TestTheBackendKeepsItsOwnListOfWhatItWillSend(t *testing.T) {
	fake := llmtest.New(llmtest.Text("ok"))
	h := newControlHarness(t, fake)
	h.addLookingModel("codegen-l", "internal/codegen-l", true, true)
	_, token := h.user()
	h.connect(token)

	_, seen := h.drivePrompt(&protocol.Prompt{Turn: "t1", Text: "run this",
		Attachments: []protocol.Attachment{{Name: "payload.bin", MimeType: "application/octet-stream",
			Data: base64.StdEncoding.EncodeToString([]byte("whatever"))}}}, nil)
	for _, b := range blocksOf(t, fake) {
		if b.Type == llm.BlockImage || b.Type == llm.BlockDocument {
			t.Fatalf("a type john does not send reached the model: %+v", b)
		}
	}
	if !warned(seen, "not a kind of file john sends") {
		t.Fatalf("the refusal was silent: %+v", seen)
	}
}

// TestWhetherAPromptMayCarryAFileIsSetInTheConsoleAndTravelsSigned is the
// other half of the setting: the console control and the signed snapshot,
// asserted together, because a switch wired to nothing and a snapshot field
// no console can write are the same bug from opposite ends.
func TestWhetherAPromptMayCarryAFileIsSetInTheConsoleAndTravelsSigned(t *testing.T) {
	srv, admin, member, memberUser := adminAndMember(t)

	var org struct {
		Prompts struct{ Attachments string }
	}
	admin.get("/api/v1/org", &org)
	if org.Prompts.Attachments != policy.AttachmentsGuarded {
		t.Fatalf("a fresh organization is on %q, want guarded", org.Prompts.Attachments)
	}
	snap := snapshotFor(t, srv, memberUser.ID)
	if !snap.AttachmentsAllowed(false) || snap.AttachmentsAllowed(true) {
		t.Fatalf("guarded should allow a plain session and refuse a controlled one, got %v/%v",
			snap.AttachmentsAllowed(false), snap.AttachmentsAllowed(true))
	}

	before := policyEpoch(t, srv)

	// An administrator switches them off; the machine's next snapshot says so.
	admin.post("PATCH", "/api/v1/org", map[string]any{
		"prompts": map[string]any{"attachments": policy.AttachmentsOff},
	}, &org)
	if org.Prompts.Attachments != policy.AttachmentsOff {
		t.Fatalf("after the change the console says %q", org.Prompts.Attachments)
	}
	if snap = snapshotFor(t, srv, memberUser.ID); snap.AttachmentsAllowed(false) {
		t.Fatal("a session with no text control may still attach after the setting was turned off")
	}

	// And the epoch moved, so a session already running is told rather than
	// left on the old answer until its snapshot expires.
	if after := policyEpoch(t, srv); after <= before {
		t.Fatalf("policy epoch %d -> %d: an attachment change must move it", before, after)
	}

	// On is the administrator taking the trade knowingly: allowed even in a
	// session whose content is under a text control.
	admin.post("PATCH", "/api/v1/org", map[string]any{
		"prompts": map[string]any{"attachments": policy.AttachmentsOn},
	}, &org)
	if snap = snapshotFor(t, srv, memberUser.ID); !snap.AttachmentsAllowed(true) {
		t.Fatal("on did not allow an attachment in a redacting session")
	}

	// A value the backend does not recognise is refused at the door rather
	// than stored and read as "off" by every client afterwards.
	if code, body := admin.do("PATCH", "/api/v1/org", map[string]any{
		"prompts": map[string]any{"attachments": "sometimes"},
	}); code != http.StatusBadRequest {
		t.Fatalf("PATCH with an unknown setting = %d, want 400: %s", code, body)
	}
	if snap = snapshotFor(t, srv, memberUser.ID); snap.Attachments() != policy.AttachmentsOn {
		t.Fatalf("a refused change still moved the setting to %q", snap.Attachments())
	}

	// It is an administrator's decision, so a member cannot make it.
	if code, body := member.do("PATCH", "/api/v1/org", map[string]any{
		"prompts": map[string]any{"attachments": policy.AttachmentsOff},
	}); code != http.StatusForbidden {
		t.Fatalf("member PATCH /api/v1/org = %d, want 403: %s", code, body)
	}
}
