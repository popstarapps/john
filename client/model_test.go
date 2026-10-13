//go:build !windows

package client

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oliverkofoed/gokit/agentkit/llm"
	"github.com/oliverkofoed/gokit/agentkit/llm/llmtest"

	"github.com/popstarapps/john/store"
)

func withHaiku(t *testing.T) func(*store.Store) {
	return func(st *store.Store) {
		lm, _ := json.Marshal(llm.Model{ID: "haiku", Name: "Claude Haiku 5.5",
			API: llm.OpenAIChat, Provider: "selfhosted", BaseURL: "https://llm.internal/v1"})
		if err := st.CreateModel(&store.Model{
			ID: "m_haiku", Name: "Claude Haiku 5.5", Provider: "selfhosted", Hosting: "external",
			ModelID: "haiku", LLMModel: string(lm),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// /model opens the list with the cursor on the current model, a digit picks
// another, and the next prompt runs on it.
func TestModelPicksFromTheList(t *testing.T) {
	s := newE2EWith(t, withHaiku(t), llmtest.Text("hello from haiku"))

	// Esc keeps the current model and says nothing: no turn was stopped.
	s.type_("/model\r")
	s.wait("keep current")
	s.type_("\x1b")
	s.type_("/model\r")
	s.wait("MODEL")
	s.wait("keep current")
	if strings.Contains(s.screen(), "no model chosen") {
		t.Fatalf("keeping the current model was reported as a stopped turn:\n%s", s.screen())
	}
	s.type_("2")
	s.wait("model → Claude Haiku 5.5")

	s.type_("hi\r")
	s.wait("hello from haiku")
	if reqs := s.fake.Requests(); reqs[len(reqs)-1].Model.ID != "haiku" {
		t.Fatalf("the prompt after /model ran on %s", reqs[len(reqs)-1].Model.ID)
	}
}

// /model with a name moves straight there, and a name that is not one of the
// allowed models changes nothing and says what there is.
func TestModelByName(t *testing.T) {
	s := newE2EWith(t, withHaiku(t))

	s.type_("/model nope\r")
	s.wait(`no model matches "nope"`)
	if !strings.Contains(s.screen(), testModelName+", Claude Haiku 5.5") {
		t.Fatalf("the refusal did not list what there is:\n%s", s.screen())
	}

	s.type_("/model haiku\r")
	s.wait("model → Claude Haiku 5.5")
}
