package backend

import (
	"strings"
	"testing"
	"time"

	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/protocol"
)

// selectModel sends the developer's /model and waits for the answer.
func (h *controlHarness) selectModel(id, model string) *protocol.ModelSelected {
	h.t.Helper()
	h.send(&protocol.ModelSelect{ID: id, Model: model})
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-h.msgs:
			if sel, ok := m.(*protocol.ModelSelected); ok && sel.ID == id {
				return sel
			}
		case <-deadline:
			h.t.Fatalf("no model.selected for %s", id)
		}
	}
}

// TestADeveloperMovesTheSessionToAnotherModel is /model: the session starts
// on the first model it is allowed, moves to another on request, and the next
// turn runs there. What it may move to is the backend's decision, not the
// client's: not a model nobody assigned, not one a narrowing ruled out, and
// not halfway through a turn.
func TestADeveloperMovesTheSessionToAnotherModel(t *testing.T) {
	fake := llmtest.New(
		llmtest.Text("on haiku"),
		llmtest.ToolCall("run", map[string]any{"cmd": "go test ./..."}),
		llmtest.Text("tests pass"),
	)
	h := newControlHarness(t, fake)
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	h.addModel("haiku", "Claude Haiku 5.5", "external")
	userID, token := h.user()

	ready := h.connect(token)
	if ready.Model.ID != "m_codegen-l" || len(ready.Models) != 2 {
		t.Fatalf("ready = model %s of %+v, want the first of both", ready.Model.ID, ready.Models)
	}

	sel := h.selectModel("s1", "m_haiku")
	if sel.Error != "" || sel.Model.ID != "m_haiku" || sel.Model.Name != "Claude Haiku 5.5" {
		t.Fatalf("select = %+v", sel)
	}
	if reason, _ := h.drive("hello", nil); reason != "completed" {
		t.Fatalf("turn ended %s", reason)
	}
	if reqs := fake.Requests(); reqs[len(reqs)-1].Model.ID != "haiku" {
		t.Fatalf("the turn after /model ran on %s", reqs[len(reqs)-1].Model.ID)
	}
	if row, _ := h.store.FindActiveSession(userID, "wk1"); row == nil || row.ModelID != "m_haiku" {
		t.Fatalf("the choice was not kept with the session: %+v", row)
	}

	// Not in the middle of a turn: it runs on one model from start to end.
	h.send(&protocol.Prompt{Turn: "t2", Text: "run the tests"})
	var call *protocol.ToolCall
	for call == nil {
		select {
		case m := <-h.msgs:
			call, _ = m.(*protocol.ToolCall)
		case <-time.After(10 * time.Second):
			t.Fatal("no tool.call")
		}
	}
	if sel := h.selectModel("s2", "m_codegen-l"); !strings.Contains(sel.Error, "a turn is running") {
		t.Fatalf("select mid-turn = %+v, want refused", sel)
	}
	h.send(&protocol.ToolResult{ID: call.ID, Status: "ok"})
	for {
		m := <-h.msgs
		if _, ok := m.(*protocol.TurnEnd); ok {
			break
		}
	}

	// Not a model nobody assigned.
	if sel := h.selectModel("s3", "m_somebody-elses"); !strings.Contains(sel.Error, "not a model your policy groups allow") {
		t.Fatalf("select of an unknown model = %+v", sel)
	}

	// Not a way around a routing policy: a session narrowed to the internal
	// model cannot be moved back out of it.
	if err := h.store.SetSessionNarrowed(ready.Session, `["m_codegen-l"]`); err != nil {
		t.Fatal(err)
	}
	if sel := h.selectModel("s4", "m_haiku"); !strings.Contains(sel.Error, "narrowed") {
		t.Fatalf("select past a narrowing = %+v", sel)
	}
	if sel := h.selectModel("s5", "m_codegen-l"); sel.Error != "" {
		t.Fatalf("select within the narrowing = %+v", sel)
	}
}

// /new keeps the model the developer moved to: a new conversation is not a
// reason to be put back on the first model.
func TestANewSessionKeepsTheModelPicked(t *testing.T) {
	h := newControlHarness(t, llmtest.New())
	h.addModel("codegen-l", "internal/codegen-l", "internal")
	h.addModel("haiku", "Claude Haiku 5.5", "external")
	_, token := h.user()
	h.connect(token)
	if sel := h.selectModel("s1", "m_haiku"); sel.Error != "" {
		t.Fatal(sel.Error)
	}
	fresh := openWith(t, h.srv.Addr(), token, &protocol.SessionOpen{
		Workspace: "/tmp/proj", WorkspaceKey: "wk1", New: true})
	if fresh.Model.ID != "m_haiku" {
		t.Fatalf("/new started on %s, want m_haiku", fresh.Model.ID)
	}
}
