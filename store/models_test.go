package store

import (
	"testing"

	"github.com/oliverkofoed/gokit/agentkit/llm"
)

// The console asks for an endpoint, and people paste the one the provider
// documents — the full URL. The client adds that path itself, so it has to
// come off, or every request goes to …/v1/messages/v1/messages and gets a 404.
func TestAPastedEndpointBecomesTheBaseTheClientExtends(t *testing.T) {
	for _, c := range []struct {
		preset, endpoint, want string
	}{
		{"anthropic", "https://api.anthropic.com/v1/messages", "https://api.anthropic.com"},
		{"anthropic", "https://api.anthropic.com/", "https://api.anthropic.com"},
		{"anthropic", "", "https://api.anthropic.com"},
		{"anthropic", "https://llm-gw.example/anthropic/v1", "https://llm-gw.example/anthropic"},
		{"openai", "https://api.openai.com/v1/chat/completions", "https://api.openai.com/v1"},
		{"openai", "https://api.openai.com/v1/responses", "https://api.openai.com/v1"},
		{"openai", "", "https://api.openai.com/v1"},
		{"google", "", "https://generativelanguage.googleapis.com"},
		{"selfhosted", "https://llm-gw.internal.example/v1/chat/completions", "https://llm-gw.internal.example/v1"},
	} {
		m := BuildLLMModel(c.preset, c.endpoint, "m", "m", 0, 0, 0, 0)
		if m.BaseURL != c.want {
			t.Errorf("%s %q: base %q, want %q", c.preset, c.endpoint, m.BaseURL, c.want)
		}
	}
}

// Anthropic refuses a streamed request with max_tokens 0, and a model added
// in the console says no limit of its own; it gets one that works.
func TestAnAnthropicModelAlwaysHasAnOutputLimit(t *testing.T) {
	if m := BuildLLMModel("anthropic", "", "claude-x", "x", 0, 0, 0, 0); m.MaxOutput == 0 {
		t.Fatal("built without an output limit")
	}
	stored := &Model{LLMModel: `{"id":"claude-x","api":"` + string(llm.AnthropicMessages) + `"}`}
	if lm, _ := stored.LLM(); lm.MaxOutput == 0 {
		t.Fatal("a stored model without a limit is read without one")
	}
	if m := BuildLLMModel("openai", "", "gpt-x", "x", 0, 0, 0, 0); m.MaxOutput != 0 {
		t.Fatalf("openai got limit %d; 0 leaves it to the model", m.MaxOutput)
	}
}

// Rows saved before the fix hold the full endpoint in their stored config;
// reading one gives the base the client needs, with no migration.
func TestAStoredFullEndpointIsReadAsItsBase(t *testing.T) {
	m := &Model{LLMModel: `{"id":"claude-x","api":"` + string(llm.AnthropicMessages) +
		`","base_url":"https://api.anthropic.com/v1/messages"}`}
	lm, err := m.LLM()
	if err != nil {
		t.Fatal(err)
	}
	if lm.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("base %q", lm.BaseURL)
	}
}

// A named provider needs only a model and a key: everything else comes from
// agentkit's catalog, and so do the limits and quirks the client needs.
func TestAKnownModelIsCompletedFromTheCatalog(t *testing.T) {
	s := openTestStore(t)
	m := &Model{Provider: "anthropic", ModelID: llm.ClaudeOpus5.ID, APIKey: "sk"}
	if err := s.CreateModel(m); err != nil {
		t.Fatal(err)
	}
	got, err := s.ModelByID(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != llm.ClaudeOpus5.Name || got.ContextWindow != llm.ClaudeOpus5.ContextWindow ||
		got.CostIn != llm.ClaudeOpus5.Cost.Input || got.Endpoint != "https://api.anthropic.com" {
		t.Fatalf("row = %+v", got)
	}
	lm, err := got.LLM()
	if err != nil {
		t.Fatal(err)
	}
	if lm.MaxOutput != llm.ClaudeOpus5.MaxOutput || !lm.Quirks.AnthropicAdaptiveThinking || !lm.Vision {
		t.Fatalf("model = %+v", lm)
	}
}

// A model saved before the catalog knew it — or before john used the
// catalog — is read with what the catalog knows now.
func TestASavedModelIsReadWithTheCatalogsKnowledge(t *testing.T) {
	stored := &Model{LLMModel: `{"id":"` + llm.ClaudeSonnet5.ID + `","api":"` + string(llm.AnthropicMessages) +
		`","base_url":"https://api.anthropic.com/v1/messages","max_output":0,"vision":false}`}
	lm, err := stored.LLM()
	if err != nil {
		t.Fatal(err)
	}
	if lm.MaxOutput != llm.ClaudeSonnet5.MaxOutput || !lm.Vision || !lm.Quirks.AnthropicAdaptiveThinking {
		t.Fatalf("model = %+v", lm)
	}
}

// Behind a gateway, a known model keeps the gateway's address and the
// administrator's prices, and still gets the quirks its protocol needs.
func TestAKnownModelBehindAGatewayKeepsItsAddress(t *testing.T) {
	m := BuildLLMModel("anthropic-compatible", "https://gw.example/anthropic", llm.ClaudeOpus5.ID, "Opus via gateway", 1, 0.1, 2, 0)
	if m.BaseURL != "https://gw.example/anthropic" || m.Cost.Input != 1 || m.Name != "Opus via gateway" {
		t.Fatalf("model = %+v", m)
	}
	if !m.Quirks.AnthropicAdaptiveThinking || m.MaxOutput != llm.ClaudeOpus5.MaxOutput {
		t.Fatalf("quirks and limits not applied: %+v", m)
	}
}
