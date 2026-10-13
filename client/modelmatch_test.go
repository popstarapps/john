package client

import (
	"testing"

	"github.com/popstarapps/john/protocol"
)

func TestMatchModels(t *testing.T) {
	models := []protocol.ModelInfo{
		{ID: "m_opus", Name: "Claude Opus 5.5"},
		{ID: "m_sonnet", Name: "Claude Sonnet 5.5"},
		{ID: "m_haiku", Name: "Claude Haiku 5.5"},
	}
	for arg, want := range map[string]string{
		"haiku": "m_haiku", "SONNET": "m_sonnet", "m_opus": "m_opus", "claude opus 5.5": "m_opus",
	} {
		if got := matchModels(models, arg); len(got) != 1 || got[0].ID != want {
			t.Errorf("%q matched %+v, want %s", arg, got, want)
		}
	}
	if got := matchModels(models, "claude"); len(got) != 3 {
		t.Errorf("an ambiguous name matched %d, want all three", len(got))
	}
}
