package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oliverkofoed/gokit/agentkit/llm"
)

// Model is an admin-configured model. LLMModel is the agentkit llm.Model as
// JSON — the thing the backend actually calls with — and the other columns
// are what the console shows. APIKey is write-only through the API and lives
// only here.
type Model struct {
	ID            string
	Name          string
	Provider      string // anthropic, openai, google — or, for any other endpoint, the protocol it speaks
	Hosting       string // internal | external
	Endpoint      string
	ModelID       string // the provider's own identifier, e.g. claude-opus-5
	ContextWindow int
	Status        string // enabled | disabled
	CostIn        float64
	CostCachedIn  float64
	CostOut       float64
	APIKey        string
	LLMModel      string // JSON llm.Model
	AddedAt       time.Time
}

// LLM unmarshals the stored llm.Model.
func (m *Model) LLM() (llm.Model, error) {
	var out llm.Model
	err := json.Unmarshal([]byte(m.LLMModel), &out)
	// Rows written before these fixes carry the full endpoint and no output
	// limit; read them the way they are written now. A model agentkit knows
	// is read with what agentkit knows about it today, so a catalog update
	// reaches models saved before it.
	out.BaseURL = baseURL(out.API, out.BaseURL)
	withCatalog(&out)
	return out, err
}

// Providers the console offers by name. Their models come from agentkit's
// catalog, and they need nothing but a model and a key. Anything else is an
// endpoint the administrator describes, named by the protocol it speaks.
var providerAPIs = map[string]struct {
	api  llm.API
	base string
}{
	"anthropic":            {llm.AnthropicMessages, "https://api.anthropic.com"},
	"openai":               {llm.OpenAIResponses, "https://api.openai.com/v1"},
	"google":               {llm.GoogleGemini, "https://generativelanguage.googleapis.com"},
	"anthropic-compatible": {llm.AnthropicMessages, ""},
	"openai-compatible":    {llm.OpenAIChat, ""},
	"gemini-compatible":    {llm.GoogleGemini, ""},
	"vertex":               {llm.GoogleGemini, ""}, // rows from before the protocol choice
}

// providerAPI is the protocol a provider value speaks. Matched loosely, so a
// row written by hand or by an older console still reaches the right API;
// azure, bedrock, selfhosted and anything unknown speak OpenAI chat
// completions, the most widely implemented.
func providerAPI(provider string) (llm.API, string) {
	if p, ok := providerAPIs[strings.ToLower(strings.TrimSpace(provider))]; ok {
		return p.api, p.base
	}
	return llm.OpenAIChat, ""
}

// CatalogProviders are the providers whose models are listed from agentkit's
// catalog rather than typed in.
var CatalogProviders = []string{"anthropic", "openai", "google"}

// Catalog is agentkit's list of known models for the providers the console
// offers by name, in agentkit's order.
func Catalog() []llm.Model {
	var out []llm.Model
	for _, m := range llm.Catalog() {
		for _, p := range CatalogProviders {
			if m.Provider == p {
				out = append(out, m)
			}
		}
	}
	return out
}

func catalogEntry(api llm.API, id string) (llm.Model, bool) {
	for _, c := range llm.Catalog() {
		if c.API == api && c.ID == id {
			return c, true
		}
	}
	return llm.Model{}, false
}

// withCatalog gives a model what agentkit knows about it — output limit,
// capabilities and the protocol quirks it needs — matched on protocol and
// model id, so a known model behind a gateway gets them too. Its address,
// name, context and prices are left as configured.
func withCatalog(m *llm.Model) {
	if c, ok := catalogEntry(m.API, m.ID); ok {
		m.MaxOutput = c.MaxOutput
		m.Reasoning, m.Vision, m.Documents = c.Reasoning, c.Vision, c.Documents
		m.Quirks = c.Quirks
	}
	if m.MaxOutput == 0 {
		m.MaxOutput = defaultMaxOutput[m.API]
	}
}

// fillFromCatalog completes a known model's row from the catalog where the
// console left it blank, which for a named provider is everything but the
// model id and the key: its name, address, context window and prices.
func fillFromCatalog(m *Model) {
	api, base := providerAPI(m.Provider)
	if m.Endpoint == "" {
		m.Endpoint = base
	}
	c, ok := catalogEntry(api, m.ModelID)
	if !ok {
		return
	}
	if m.Name == "" {
		m.Name = c.Name
	}
	if m.ContextWindow == 0 {
		m.ContextWindow = c.ContextWindow
	}
	if m.CostIn == 0 && m.CostCachedIn == 0 && m.CostOut == 0 {
		m.CostIn, m.CostCachedIn, m.CostOut = c.Cost.Input, c.Cost.CacheRead, c.Cost.Output
	}
}

// defaultMaxOutput is the output limit a model gets when the console did not
// say one. Anthropic's API requires max_tokens on every request and refuses a
// stream with 0; 32k is within what every current Claude model accepts. The
// other APIs leave the limit out when it is 0, which means the model's own.
var defaultMaxOutput = map[llm.API]int{
	llm.AnthropicMessages: 32_000,
}

// endpointPaths are the paths each client appends to a base URL itself.
var endpointPaths = map[llm.API][]string{
	llm.AnthropicMessages: {"/v1/messages", "/v1"},
	llm.OpenAIResponses:   {"/responses", "/chat/completions"},
	llm.OpenAIChat:        {"/chat/completions"},
	llm.GoogleGemini:      {"/v1beta"},
}

// baseURL is what the client builds requests from. The console asks for an
// endpoint, and the one a provider documents is the full URL — which the
// client would then extend a second time, into a path that answers 404. So
// the part the client adds is taken off here.
func baseURL(api llm.API, endpoint string) string {
	u := strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	for _, p := range endpointPaths[api] {
		if strings.HasSuffix(u, p) {
			return strings.TrimSuffix(u, p)
		}
	}
	return u
}

// BuildLLMModel derives the agentkit model config from the console's fields.
// Unsupported providers still build — they fail at call time with an honest
// error rather than blocking an admin from recording them.
func BuildLLMModel(provider, endpoint, modelID, name string, costIn, costCachedIn, costOut float64, contextWindow int) llm.Model {
	api, base := providerAPI(provider)
	m := llm.Model{
		ID:            modelID,
		Name:          name,
		API:           api,
		Provider:      strings.ToLower(strings.TrimSpace(provider)),
		BaseURL:       base,
		ContextWindow: contextWindow,
		Cost: llm.Cost{
			Input:     costIn,
			CacheRead: costCachedIn,
			Output:    costOut,
		},
	}
	withCatalog(&m)
	// The endpoint, when given, wins over the provider's usual one.
	if b := baseURL(m.API, endpoint); b != "" {
		m.BaseURL = b
	}
	return m
}

const modelColumns = `id, name, provider, hosting, endpoint, model_id, context_window, status,
	cost_in, cost_cached_in, cost_out, api_key, llm_model, added_at`

func scanModel(row interface{ Scan(...any) error }) (*Model, error) {
	var m Model
	var added string
	err := row.Scan(&m.ID, &m.Name, &m.Provider, &m.Hosting, &m.Endpoint, &m.ModelID,
		&m.ContextWindow, &m.Status, &m.CostIn, &m.CostCachedIn, &m.CostOut,
		&m.APIKey, &m.LLMModel, &added)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.AddedAt, _ = time.Parse(time.RFC3339, added)
	return &m, nil
}

// CreateModel inserts a model. A missing ID or LLMModel is derived.
func (s *Store) CreateModel(m *Model) error {
	fillFromCatalog(m)
	if m.ID == "" {
		m.ID = NewID("m")
	}
	if m.Status == "" {
		m.Status = "enabled"
	}
	if m.Hosting == "" {
		m.Hosting = "external"
	}
	if m.LLMModel == "" {
		lm := BuildLLMModel(m.Provider, m.Endpoint, m.ModelID, m.Name,
			m.CostIn, m.CostCachedIn, m.CostOut, m.ContextWindow)
		b, err := json.Marshal(lm)
		if err != nil {
			return fmt.Errorf("store: build llm model: %w", err)
		}
		m.LLMModel = string(b)
	}
	if m.AddedAt.IsZero() {
		m.AddedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`INSERT INTO models (`+modelColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Name, m.Provider, m.Hosting, m.Endpoint, m.ModelID, m.ContextWindow, m.Status,
		m.CostIn, m.CostCachedIn, m.CostOut, m.APIKey, m.LLMModel, m.AddedAt.Format(time.RFC3339))
	return err
}

// Models lists every model, oldest first.
func (s *Store) Models() ([]*Model, error) {
	rows, err := s.db.Query(`SELECT ` + modelColumns + ` FROM models ORDER BY added_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ModelByID looks a model up.
func (s *Store) ModelByID(id string) (*Model, error) {
	return scanModel(s.db.QueryRow(`SELECT `+modelColumns+` FROM models WHERE id = ?`, id))
}

// UpdateModel rewrites a model's configuration in place, keeping its id, its
// enabled/disabled status and its added_at.
//
// The stored llm.Model is derived, never edited: it is rebuilt from the
// fields the administrator just changed, so an endpoint or a cost corrected
// in the console is the endpoint and the cost the next request actually uses.
//
// An empty APIKey means "leave the credential alone". The console is never
// given a stored key back, so it has nothing to send us but a new one, and a
// blank field there means the admin was editing something else.
func (s *Store) UpdateModel(m *Model) error {
	fillFromCatalog(m)
	lm := BuildLLMModel(m.Provider, m.Endpoint, m.ModelID, m.Name,
		m.CostIn, m.CostCachedIn, m.CostOut, m.ContextWindow)
	b, err := json.Marshal(lm)
	if err != nil {
		return fmt.Errorf("store: build llm model: %w", err)
	}
	m.LLMModel = string(b)
	if m.Hosting == "" {
		m.Hosting = "external"
	}
	res, err := s.db.Exec(`UPDATE models SET name = ?, provider = ?, hosting = ?, endpoint = ?,
		model_id = ?, context_window = ?, cost_in = ?, cost_cached_in = ?, cost_out = ?,
		llm_model = ?, api_key = CASE WHEN ? != '' THEN ? ELSE api_key END WHERE id = ?`,
		m.Name, m.Provider, m.Hosting, m.Endpoint, m.ModelID, m.ContextWindow,
		m.CostIn, m.CostCachedIn, m.CostOut, m.LLMModel, m.APIKey, m.APIKey, m.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteModel removes a model outright, credential and all.
//
// Usage rows keep their model id: a day's spend happened whether or not the
// model is still configured, so the totals stay right and only the per-model
// breakdown loses the row. Refusing to delete a model a routing policy still
// names is the caller's job — see PoliciesReferencingModel.
func (s *Store) DeleteModel(id string) error {
	res, err := s.db.Exec(`DELETE FROM models WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.BumpPolicyEpoch(ChangeModels)
	return nil
}

// SetModelStatus enables or disables a model.
func (s *Store) SetModelStatus(id, status string) error {
	if status != "enabled" && status != "disabled" {
		return fmt.Errorf("store: bad model status %q", status)
	}
	res, err := s.db.Exec(`UPDATE models SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetModelAPIKey replaces a model's stored credential.
func (s *Store) SetModelAPIKey(id, key string) error {
	_, err := s.db.Exec(`UPDATE models SET api_key = ? WHERE id = ?`, key, id)
	return err
}
