//go:build ignore

// Command console-backend runs a real john backend, seeded with a fictional
// company, for landing/scripts/console-shots.mjs to photograph. It is not
// meant to be run by hand — `npm run screenshots` in landing/ builds and
// drives it — but it can be, to click around the same console the screenshots
// show:
//
//	go run landing/scripts/console-backend.go -data /tmp/shots
//
// Everything here goes through the code john itself runs. The backend is
// backend.New, the same constructor `john backend` and standalone mode use;
// the sign-in is the embedded dev identity provider (auth/devidp) that
// standalone mode and the test suite sign in through, so the browser walks the
// production OIDC path — discovery, code exchange, ID-token verification — and
// ends up holding an ordinary session cookie. Configuration is written through
// the backend's own HTTP API as the administrator, which is what gives the
// change history its entries; requests are filed by the members who make
// them, with their own tokens. Only what has no API — people signing in, the
// usage rollups and proxy traffic the agent loop records, sessions and their
// encrypted traces — is written through the store and trace packages
// directly, with the same calls the backend makes.
//
// The one liberty is time. The store stamps everything with time.Now(), and a
// console where every change happened in the last three seconds is not a
// believable one, so after each step of the story the rows it just wrote are
// moved back to when the story says it happened (see stamp). Nothing else is
// edited after the fact.
//
// All of the company — people, teams, groups, policies, models, requests,
// sessions — is in the "the company" section right below, so a changed
// screenshot is usually a changed line there.
//
// It prints one JSON line to stdout when the console is ready:
//
//	{"url":"http://127.0.0.1:…","traceKey":"AGE-SECRET-KEY-1…","admin":"…"}
//
// and serves until stdin closes or it is interrupted. With -dist it serves a
// console build from that directory in front of the backend (so a fresh
// `vite build` can be photographed without rebuilding the Go binary); without
// it, the console embedded in the binary.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"

	"github.com/popstarapps/john/auth/devidp"
	"github.com/popstarapps/john/backend"
	"github.com/popstarapps/john/store"
	"github.com/popstarapps/john/trace"
)

// ================================================================ the company

const (
	orgName = "Lumenfield"
	domain  = "lumenfield.com"
)

// person is someone who has signed in. The first one is the administrator the
// browser signs in as.
type person struct {
	key, name string
	teams     []string // team keys
	admin     bool
	intensity float64 // how hard they use john: 1 is an ordinary day's work
}

var people = []person{
	{"maya", "Maya Okafor", []string{"security", "platform"}, true, 0.7},
	{"jonas", "Jonas Brandt", []string{"platform"}, false, 1.5},
	{"priya", "Priya Raman", []string{"payments"}, false, 1.7},
	{"tomas", "Tomás Herrera", []string{"payments"}, false, 1.1},
	{"elin", "Elin Sørensen", []string{"payments"}, false, 0.8},
	{"david", "David Kim", []string{"mobile"}, false, 1.3},
	{"aisha", "Aisha Bello", []string{"mobile"}, false, 0.9},
	{"lukas", "Lukas Novak", []string{"platform"}, false, 1.0},
	{"sofia", "Sofia Marchetti", []string{"data"}, false, 1.4},
	{"ben", "Ben Whitaker", []string{"data"}, false, 0.8},
	{"hana", "Hana Watanabe", []string{"data"}, false, 0.6},
	{"oscar", "Oscar Lindgren", []string{"security"}, false, 0.9},
	{"grace", "Grace Mensah", []string{"mobile"}, false, 0.7},
	{"ravi", "Ravi Iyer", []string{"platform", "data"}, false, 1.2},
}

type team struct {
	key, name, slug, description string
	groups                       []string // group keys, besides the baseline
}

var teams = []team{
	{"platform", "Platform", "platform", "Build, deploy and the internal developer platform.", []string{"registries", "internal", "frontier"}},
	{"payments", "Payments", "payments", "Card processing, the ledger and payouts. In PCI scope.", []string{"registries", "internal", "pci"}},
	{"mobile", "Mobile", "mobile", "The iOS and Android apps.", []string{"registries", "mobile"}},
	{"data", "Data", "data", "Warehouse, pipelines and the analytics platform.", []string{"registries", "data"}},
	{"security", "Security", "security", "Application security and the controls John enforces.", []string{"registries", "security", "frontier"}},
}

type model struct {
	key, name, provider, hosting, endpoint, modelID string
	context                                         int
	in, cached, out                                 float64 // $ per million tokens
}

var models = []model{
	{"sonnet", "Claude Sonnet 5", "anthropic", "external", "https://api.anthropic.com/v1/messages", "claude-sonnet-5", 1_000_000, 3, 0.3, 15},
	{"opus", "Claude Opus 5", "anthropic", "external", "https://api.anthropic.com/v1/messages", "claude-opus-5", 1_000_000, 5, 0.5, 25},
	{"gpt", "GPT-5.1", "openai", "external", "https://api.openai.com/v1/chat/completions", "gpt-5.1", 400_000, 4, 0.4, 16},
	{"coder", "Lumen Coder 32B", "selfhosted", "internal", "https://llm-gw.lumenfield.internal/v1/chat/completions", "qwen2.5-coder-32b", 128_000, 0.2, 0.02, 0.6},
	{"llama", "Llama 3.3 70B", "selfhosted", "internal", "https://llm-gw.lumenfield.internal/v1/chat/completions", "llama-3.3-70b", 128_000, 0.35, 0.035, 0.9},
	{"haiku", "Claude Haiku 4.5", "anthropic", "external", "https://api.anthropic.com/v1/messages", "claude-haiku-4-5", 200_000, 1, 0.1, 5},
}

// disabledModels were tried and switched off again.
var disabledModels = []string{"haiku"}

type group struct {
	key, name, description string
	baseline               bool
	policies               []policy
}

type policy struct {
	name, description string
	spec              map[string]any
}

// Spec builders, so the table below reads as policy rather than JSON.
func network(decision, host string, methods []string, path string) map[string]any {
	return map[string]any{"kind": "network", "methods": methods, "scheme": "https", "host": host,
		"port": nil, "path": path, "headerMatches": []any{}, "queryMatches": []any{},
		"decision": decision, "maxBodyBytes": nil}
}
func workspace(mode string, paths ...string) map[string]any {
	return map[string]any{"kind": "workspace", "mode": mode, "paths": paths}
}
func replacement(pattern, with string) map[string]any {
	return map[string]any{"kind": "replacement", "pattern": pattern, "replacement": with}
}

// routing names models by key; the seeder swaps in their ids.
func routing(mode string, modelKeys []string, keywords ...string) map[string]any {
	if keywords == nil {
		keywords = []string{}
	}
	return map[string]any{"kind": "model-routing", "mode": mode, "modelIds": modelKeys, "keywords": keywords}
}

var (
	get     = []string{"GET", "HEAD"}
	anyVerb = []string{}
)

var groups = []group{
	{"baseline", "Lumenfield baseline", "What is true in every repository at Lumenfield, for every team. Nothing attached to a team can loosen it.", true, []policy{
		{"Secrets never enter the sandbox", "Dotenv files, private keys and local credentials are absent from the sandbox, so nothing running in there can read one.",
			workspace("exclude", ".env", ".env.*", "**/.env", "**/*.pem", "**/id_rsa*", ".aws/credentials")},
		{"Git history is read-only", "The agent can read log, diff and blame, and cannot rewrite history.",
			workspace("read-only", ".git")},
		{"CI workflows are read-only", "Pipeline definitions change through review, never through an agent session.",
			workspace("read-only", ".github/workflows", ".gitlab-ci.yml")},
		{"Migrations cannot be deleted", "Applied migrations are history; a new one is how the schema changes.",
			workspace("no-delete", "db/migrations/**")},
		{"Redact AWS access keys", "Key ids never reach a model provider; each distinct key gets a stable alias.",
			replacement(`AKIA[0-9A-Z]{16}`, "AWS_KEY_#")},
		{"Redact IBANs", "Customer bank accounts in fixtures, logs and stack traces become stable aliases.",
			replacement(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`, "IBAN_#")},
		{"Block paste sites", "Paste and file-drop services are the easiest exfiltration path there is.",
			network("deny", "*.pastebin.com", anyVerb, "/*")},
		{"Block file-sharing uploads", "", network("deny", "transfer.sh", anyVerb, "/*")},
		{"Default models", "Every engineer gets Sonnet and the internal model.",
			routing("assign", []string{"sonnet", "coder"})},
		{"Project Halyard stays internal", "Anything mentioning the acquisition codename continues on the internally hosted model only.",
			routing("restrict", []string{"coder"}, "halyard", "project-halyard")},
	}},
	{"registries", "Package registries", "Read-only access to the public package ecosystems our builds depend on.", false, []policy{
		{"npm registry", "", network("always-allow", "registry.npmjs.org", get, "/*")},
		{"Go module proxy", "", network("always-allow", "proxy.golang.org", get, "/*")},
		{"Go checksum database", "", network("always-allow", "sum.golang.org", get, "/*")},
		{"PyPI", "", network("always-allow", "pypi.org", get, "/simple/*")},
		{"PyPI files", "", network("always-allow", "files.pythonhosted.org", get, "/packages/*")},
		{"GitHub, read-only", "Clones and release downloads. Pushes go through the developer, not the sandbox.",
			network("always-allow", "github.com", get, "/*")},
		{"GitHub raw content", "", network("always-allow", "raw.githubusercontent.com", get, "/*")},
	}},
	{"internal", "Internal services", "Reaching Lumenfield's own systems from a sandbox.", false, []policy{
		{"Internal Git, read-only", "", network("always-allow", "git.lumenfield.internal", get, "/*")},
		{"Artifact mirror", "", network("always-allow", "artifacts.lumenfield.internal", get, "/*")},
		{"Staging API writes", "Writes against staging are allowed one at a time, by the developer, after they have seen the request.",
			network("ask-developer", "api.staging.lumenfield.internal", []string{"POST", "PUT", "PATCH", "DELETE"}, "/*")},
		{"No production hosts", "Nothing in a sandbox talks to production, whatever it is for.",
			network("deny", "*.prod.lumenfield.internal", anyVerb, "/*")},
	}},
	{"pci", "Cardholder data (PCI)", "The controls that keep card data out of external models.", false, []policy{
		{"Redact card numbers", "Primary account numbers become stable aliases before any model sees them.",
			replacement(`\b(?:4\d{3}|5[1-5]\d{2}|3[47]\d{2})[ -]?\d{4}[ -]?\d{4}[ -]?\d{1,4}\b`, "PAN_#")},
		{"Cardholder data stays internal", "A session that touches cardholder code continues on the internal model only.",
			routing("restrict", []string{"coder"}, "cardholder", "pan_encrypt", "pci-scope")},
		{"Card fixtures are excluded", "", workspace("exclude", "fixtures/cardholder/**", "testdata/pans/**")},
		{"Payment processor sandbox", "Test-mode calls only; the developer approves each one.",
			network("ask-developer", "api.sandbox.paymentsprovider.example", []string{"POST"}, "/v1/*")},
	}},
	{"mobile", "Mobile toolchains", "What the iOS and Android builds pull in.", false, []policy{
		{"Google Maven", "", network("always-allow", "dl.google.com", get, "/*")},
		{"CocoaPods CDN", "", network("always-allow", "cdn.cocoapods.org", get, "/*")},
		{"App Store Connect", "Uploads and metadata edits are the developer's call, one request at a time.",
			network("ask-developer", "api.appstoreconnect.apple.com", anyVerb, "/v1/*")},
	}},
	{"data", "Data platform", "Warehouse and notebook access for the data team.", false, []policy{
		{"Warehouse queries", "Every query leaves the sandbox only after the developer has read it.",
			network("ask-developer", "lumenfield.eu-west-1.snowflakecomputing.com", []string{"POST"}, "/queries/*")},
		{"Conda channels", "", network("always-allow", "conda.anaconda.org", get, "/*")},
		{"Warehouse exports are excluded", "", workspace("exclude", "exports/**", "*.parquet")},
		{"Llama for notebooks", "The larger internal model, for long notebook sessions.", routing("assign", []string{"llama"})},
	}},
	{"security", "Security tooling", "Vulnerability databases and scanners.", false, []policy{
		{"OSV", "", network("always-allow", "api.osv.dev", []string{"GET", "POST"}, "/v1/*")},
		{"NVD", "", network("always-allow", "services.nvd.nist.gov", get, "/rest/*")},
	}},
	{"frontier", "Frontier models", "Opus for the teams doing the hardest refactors.", false, []policy{
		{"Opus and GPT-5.1", "", routing("assign", []string{"opus", "gpt"})},
	}},
}

// accessRequest is one a developer filed from the CLI after an unknown
// request was blocked. evidence is what the proxy saw.
type accessRequest struct {
	who, team, title, justification string
	group                           string // proposed group key, "" for a new one
	spec                            map[string]any
	evidence                        []string // URLs that were blocked
	daysAgo                         float64
	// What became of it. "" leaves it pending.
	outcome   string // approved | rejected | needs-info
	decidedAt float64
	decider   string // person key; the first administrator when empty
	comments  []comment
}

type comment struct {
	who     string
	body    string
	daysAgo float64
}

var requests = []accessRequest{
	{who: "david", team: "mobile", title: "Maven Central for the Android build", group: "mobile",
		justification: "Gradle resolves most of our Android dependencies from Maven Central. Without it the sandbox build fails on the first sync.",
		spec:          network("always-allow", "repo.maven.apache.org", get, "/maven2/*"), evidence: []string{"https://repo.maven.apache.org/maven2/androidx/core/core-ktx/1.15.0/core-ktx-1.15.0.pom"},
		daysAgo: 31, outcome: "approved", decidedAt: 30.8},
	{who: "jonas", team: "platform", title: "Docker Hub image pulls", group: "registries",
		justification: "Our integration tests start Postgres and Redis from Docker Hub images.",
		spec:          network("always-allow", "registry-1.docker.io", get, "/v2/*"), evidence: []string{"https://registry-1.docker.io/v2/library/postgres/manifests/17"},
		daysAgo: 12, outcome: "approved", decidedAt: 11.7},
	{who: "ben", team: "data", title: "pastebin.com for sharing query plans", group: "",
		justification: "Easier to share long EXPLAIN output with the team.",
		spec:          network("always-allow", "pastebin.com", []string{"POST"}, "/api/api_post.php"), evidence: []string{"https://pastebin.com/api/api_post.php"},
		daysAgo: 6, outcome: "rejected", decidedAt: 5.8, decider: "oscar",
		comments: []comment{{"oscar", "Paste sites are blocked in the baseline. Put long plans in the ticket or in a gist on internal Git instead.", 5.8}}},
	{who: "sofia", team: "data", title: "OpenAI API for the evaluation harness", group: "data",
		justification: "The churn-model eval harness calls the OpenAI API to grade summaries.",
		spec:          network("always-allow", "api.openai.com", []string{"POST"}, "/v1/*"), evidence: []string{"https://api.openai.com/v1/responses"},
		daysAgo: 2.4, outcome: "needs-info",
		comments: []comment{
			{"maya", "What data goes into the prompts? If it's customer text we'd want it on the internal model instead of a direct API call from the sandbox.", 2.1},
		}},
	{who: "priya", team: "payments", title: "crates.io for the ledger rewrite", group: "registries",
		justification: "The new ledger reconciliation service is in Rust. Cargo needs the crates.io index and the crate downloads; read-only is enough.",
		spec:          network("always-allow", "index.crates.io", get, "/*"), evidence: []string{"https://index.crates.io/config.json", "https://index.crates.io/se/rd/serde"},
		daysAgo: 0.9},
	{who: "david", team: "mobile", title: "Sentry release uploads", group: "mobile",
		justification: "Uploading dSYMs and ProGuard mappings at the end of a release build so crashes symbolicate.",
		spec:          network("ask-developer", "sentry.io", []string{"POST"}, "/api/0/projects/lumenfield/*"), evidence: []string{"https://sentry.io/api/0/projects/lumenfield/ios/files/difs/"},
		daysAgo: 0.35},
	{who: "lukas", team: "platform", title: "Terraform registry", group: "registries",
		justification: "terraform init needs provider downloads for the infra modules.",
		spec:          network("always-allow", "registry.terraform.io", get, "/v1/providers/*"), evidence: []string{"https://registry.terraform.io/v1/providers/hashicorp/aws/versions"},
		daysAgo: 0.15},
	{who: "hana", team: "data", title: "Hugging Face model downloads", group: "data",
		justification: "Pulling a small sentence-embedding model for the search relevance experiment.",
		spec:          network("always-allow", "huggingface.co", get, "/api/models/*"), evidence: []string{"https://huggingface.co/api/models/sentence-transformers/all-MiniLM-L6-v2"},
		daysAgo: 0.08},
	{who: "oscar", team: "security", title: "Snyk vulnerability database", group: "security",
		justification: "Cross-checking OSV results against Snyk for the quarterly dependency audit.",
		spec:          network("always-allow", "api.snyk.io", get, "/rest/*"), evidence: []string{"https://api.snyk.io/rest/orgs/lumenfield/issues"},
		daysAgo: 1.6},
	{who: "aisha", team: "mobile", title: "Figma API for design tokens", group: "mobile",
		justification: "The design-token sync script reads colour and spacing variables from the Figma file.",
		spec:          network("always-allow", "api.figma.com", get, "/v1/files/*"), evidence: []string{"https://api.figma.com/v1/files/k3Lm9Qx/variables/local"},
		daysAgo: 0.6},
}

// sessionStory is one conversation, for the traces page. The first is the one
// the screenshot opens; the rest fill the list.
type sessionStory struct {
	who, workspace string
	hoursAgo       float64
	model          string
	turns          int
	records        []rec
}

type rec struct {
	offset  time.Duration // since the session started
	kind    string
	payload any
}

func toolCall(id, tool string, args map[string]any) map[string]any {
	return map[string]any{"id": id, "tool": tool, "args": args}
}

var showcase = sessionStory{who: "priya", workspace: "/Users/priya/src/ledger-service", hoursAgo: 0.45, model: "sonnet", turns: 3, records: []rec{
	{0, "session.open", map[string]any{"workspace": "/Users/priya/src/ledger-service", "client": map[string]string{"version": "1.4.0", "os": "darwin/arm64"}}},
	{4 * time.Second, "sandbox.state", map[string]any{"state": "ready", "resources": map[string]int{"cpus": 6, "memory_gb": 12, "disk_gb": 64}}},
	{31 * time.Second, "prompt", map[string]any{"turn": "t1", "text": "The nightly reconciliation job double-counts refunds that settle on the same day as the capture. Find out why and fix it, with a test.", "attachments": []any{}}},
	{35 * time.Second, "tool.call", toolCall("c1", "run", map[string]any{"cmd": "rg -n \"settled_on\" internal/reconcile"})},
	{36 * time.Second, "tool.result", map[string]any{"id": "c1", "status": "ok", "exit": 0}},
	{39 * time.Second, "tool.call", toolCall("c2", "read", map[string]any{"path": "internal/reconcile/refunds.go"})},
	{39 * time.Second, "tool.result", map[string]any{"id": "c2", "status": "ok"}},
	{44 * time.Second, "tool.call", toolCall("c3", "read", map[string]any{"path": "internal/reconcile/refunds_test.go"})},
	{44 * time.Second, "tool.result", map[string]any{"id": "c3", "status": "ok"}},
	{52 * time.Second, "model.narrow", map[string]any{"turn": "t1", "policy": "Cardholder data stays internal", "keyword": "pan_encrypt"}},
	{58 * time.Second, "model.choice", map[string]any{"turn": "t1", "model": "coder"}},
	{71 * time.Second, "tool.call", toolCall("c4", "edit", map[string]any{"path": "internal/reconcile/refunds.go", "find": "if r.SettledOn.After(c.CapturedOn) {", "replace": "if !r.SettledOn.Before(c.CapturedOn) {"})},
	{71 * time.Second, "tool.result", map[string]any{"id": "c4", "status": "ok"}},
	{80 * time.Second, "tool.call", toolCall("c5", "edit", map[string]any{"path": "internal/reconcile/refunds_test.go", "find": "func TestRefundAfterCapture(", "replace": "func TestRefundSameDayAsCapture(t *testing.T) {\n\t// …\n}\n\nfunc TestRefundAfterCapture("})},
	{80 * time.Second, "tool.result", map[string]any{"id": "c5", "status": "ok"}},
	{84 * time.Second, "tool.call", toolCall("c6", "run", map[string]any{"cmd": "go test ./internal/reconcile/..."})},
	{86 * time.Second, "proxy.decision", map[string]any{"at": "", "method": "GET", "url": "https://proxy.golang.org/github.com/shopspring/decimal/@v/v1.4.0.zip", "verdict": "allowed", "policy": "Go module proxy", "allowed": true}},
	{88 * time.Second, "proxy.decision", map[string]any{"at": "", "method": "GET", "url": "https://sum.golang.org/lookup/github.com/shopspring/decimal@v1.4.0", "verdict": "allowed", "policy": "Go checksum database", "allowed": true}},
	{97 * time.Second, "tool.result", map[string]any{"id": "c6", "status": "ok", "exit": 0}},
	{104 * time.Second, "assistant.message", map[string]any{"turn": "t1", "text": "Found it: `matchRefunds` only paired a refund with its capture when the refund settled strictly after it, so same-day refunds fell through to the unmatched bucket and were counted again by the fallback pass. I changed the comparison to not-before and added TestRefundSameDayAsCapture, which fails on the old code and passes now. `go test ./internal/reconcile/...` is green."}},
	{104 * time.Second, "turn.end", map[string]any{"turn": "t1", "reason": "done"}},
	{3*time.Minute + 12*time.Second, "prompt", map[string]any{"turn": "t2", "text": "Run it against yesterday's settlement file from the staging API to be sure.", "attachments": []any{}}},
	{3*time.Minute + 16*time.Second, "tool.call", toolCall("c7", "run", map[string]any{"cmd": "go run ./cmd/reconcile --source https://api.staging.lumenfield.internal/v2/settlements/2026-09-24 --dry-run"})},
	{3*time.Minute + 29*time.Second, "proxy.decision", map[string]any{"at": "", "method": "POST", "url": "https://api.staging.lumenfield.internal/v2/settlements/2026-09-24/replay", "verdict": "asked", "policy": "Staging API writes", "allowed": true}},
	{3*time.Minute + 33*time.Second, "proxy.decision", map[string]any{"at": "", "method": "GET", "url": "https://metrics.datadoghq.eu/api/v1/series", "verdict": "unknown", "allowed": false}},
	{3*time.Minute + 41*time.Second, "tool.result", map[string]any{"id": "c7", "status": "ok", "exit": 0}},
	{3*time.Minute + 50*time.Second, "assistant.message", map[string]any{"turn": "t2", "text": "Dry run against 2026-09-24: 18,412 captures, 1,207 refunds, 0 double-counted (the old code reported 23). The job tried to push a metric to Datadog, which no policy covers, so that request was blocked; the reconciliation itself doesn't depend on it."}},
	{3*time.Minute + 50*time.Second, "turn.end", map[string]any{"turn": "t2", "reason": "done"}},
	{6*time.Minute + 2*time.Second, "prompt", map[string]any{"turn": "t3", "text": "Good. Write the changelog entry.", "attachments": []any{}}},
	{6*time.Minute + 9*time.Second, "tool.call", toolCall("c8", "edit", map[string]any{"path": "CHANGELOG.md", "find": "## Unreleased\n", "replace": "## Unreleased\n\n- Reconciliation no longer double-counts refunds that settle on the day of capture.\n"})},
	{6*time.Minute + 9*time.Second, "tool.result", map[string]any{"id": "c8", "status": "ok"}},
	{6*time.Minute + 14*time.Second, "assistant.message", map[string]any{"turn": "t3", "text": "Added under Unreleased. The change is ready to review with /apply."}},
	{6*time.Minute + 14*time.Second, "turn.end", map[string]any{"turn": "t3", "reason": "done"}},
	{8*time.Minute + 40*time.Second, "sandbox.state", map[string]any{"state": "applied", "pending_files": 0}},
	{8*time.Minute + 44*time.Second, "session.close", map[string]string{"reason": "closed by client"}},
}}

// The rest of the list: who worked where, and on what. Each gets a short,
// genuine trace of its own.
var otherSessions = []struct {
	who, repo, prompt string
	hoursAgo          float64
	turns             int
}{
	{"hana", "search-relevance", "Add an offline evaluation for the new ranking features.", 1.1, 2},
	{"jonas", "platform-infra", "Bump the Kubernetes client to 0.34 and fix what breaks.", 2.1, 5},
	{"david", "lumenfield-ios", "The receipt screen truncates long merchant names on small phones.", 2.9, 3},
	{"sofia", "churn-model", "Why did the weekly retrain take twice as long last night?", 3.4, 4},
	{"tomas", "payouts", "Add idempotency keys to the payout retry path.", 4.8, 6},
	{"lukas", "deploy-pipeline", "Cache Go build output between pipeline stages.", 5.5, 3},
	{"aisha", "lumenfield-android", "Migrate the settings screen to Compose.", 20.2, 7},
	{"ravi", "event-ingest", "The consumer lags when a partition rebalances. Investigate.", 21.7, 4},
	{"oscar", "authz-service", "Audit every handler for missing tenant checks.", 23.1, 8},
	{"elin", "ledger-service", "Document the settlement state machine.", 25.4, 2},
	{"ben", "dbt-models", "Split the orders model into staging and marts.", 26.9, 3},
	{"grace", "lumenfield-ios", "Fix the flaky snapshot tests on iOS 26.", 28.3, 4},
	{"priya", "ledger-service", "Profile the monthly close job.", 44.6, 5},
	{"jonas", "platform-infra", "Write a runbook for rotating the ingress certificates.", 46.0, 2},
	{"maya", "john-policies", "Review this week's network access requests against the baseline.", 47.5, 2},
	{"tomas", "payouts", "Retry-with-backoff for the bank file upload.", 49.2, 3},
	{"david", "lumenfield-ios", "Add a widget for the latest transactions.", 51.0, 6},
}

// ================================================================ the program

var (
	now       = time.Now().UTC()
	seedStart = now.Truncate(time.Second).Add(-time.Second)
	rng       = mrand.New(mrand.NewPCG(7, 11)) // fixed seed: the same shots every run
)

func main() {
	dataDir := flag.String("data", "", "directory for the database and traces (created; required)")
	dist := flag.String("dist", "", "serve the console from this vite build instead of the embedded one")
	listen := flag.String("listen", "127.0.0.1:0", "address for the console")
	flag.Parse()
	if *dataDir == "" {
		die("-data is required")
	}
	check(os.MkdirAll(*dataDir, 0o700))

	admin := people[0]
	adminEmail := email(admin)
	identity := devidp.Identity{Subject: "sso|" + admin.key, Email: adminEmail, Name: admin.name}

	backendListen := *listen
	if *dist != "" {
		backendListen = "127.0.0.1:0"
	}
	srv, err := backend.New(backend.Config{
		DBPath:  filepath.Join(*dataDir, "john.db"),
		Listen:  backendListen,
		Version: "1.4.0",
		DevIdP:  &identity,
	})
	check(err)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { srv.Serve(ctx); close(served) }()

	traceID, err := age.GenerateX25519Identity()
	check(err)
	s := &seeder{st: srv.Store(), base: "http://" + srv.Addr(), traceDir: filepath.Join(*dataDir, "traces")}
	s.org(adminEmail, traceID.Recipient().String())
	s.run()

	consoleURL := s.base
	if *dist != "" {
		consoleURL = serveDist(*listen, *dist, s.base)
	}

	out, _ := json.Marshal(map[string]string{"url": consoleURL, "traceKey": traceID.String(), "admin": adminEmail})
	fmt.Println(string(out))

	// Serve until whoever started us closes stdin, or says stop.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() { io.Copy(io.Discard, os.Stdin); stop <- os.Interrupt }()
	<-stop
	cancel()
	<-served
	srv.Close()
}

// serveDist puts a vite build in front of the backend: files from dir,
// index.html for any other page, and the backend for everything it answers.
// The Host header passes through unchanged, so the backend builds its OIDC
// redirect URI — and sets its cookie — for this address, not its own.
func serveDist(listen, dir, backendURL string) string {
	target, _ := url.Parse(backendURL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	files := http.FileServer(http.Dir(dir))
	mux := http.NewServeMux()
	for _, p := range []string{"/api/", "/auth/", "/devidp/", "/releases/"} {
		mux.Handle(p, proxy)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(r.URL.Path))); err == nil && !fi.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
	ln, err := net.Listen("tcp", listen)
	check(err)
	go http.Serve(ln, mux)
	return "http://" + ln.Addr().String()
}

type seeder struct {
	st       *store.Store
	base     string
	traceDir string

	adminToken string
	actor      string                 // whose token configuration changes are made with; adminToken when empty
	users      map[string]*store.User // by person key
	tokens     map[string]string      // machine token by person key
	teamIDs    map[string]string
	groupIDs   map[string]string
	modelIDs   map[string]string
	policyIDs  map[string]string // by policy name
}

func email(p person) string {
	first := strings.ToLower(strings.Fields(p.name)[0])
	first = strings.NewReplacer("á", "a", "ø", "o").Replace(first)
	return first + "@" + domain
}

// org writes the organization row the way standalone's bootstrap does: the
// embedded identity provider as the issuer, a trace key, a signing key.
func (s *seeder) org(adminEmail, traceRecipient string) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	check(err)
	check(s.st.SaveOrg(&store.Org{
		Name:                  orgName,
		SSOProvider:           "Developer IdP",
		OIDCIssuer:            s.base + "/devidp",
		OIDCClientID:          "john-console-shots",
		AdminEmail:            adminEmail,
		TraceKey:              traceRecipient,
		TracePath:             s.traceDir,
		SigningKey:            base64.StdEncoding.EncodeToString(priv),
		MachineSessionMaxDays: 90,
		SetupComplete:         true,
	}))
}

func (s *seeder) run() {
	s.users, s.tokens = map[string]*store.User{}, map[string]string{}
	s.teamIDs, s.groupIDs, s.modelIDs, s.policyIDs = map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}

	// Day one: the administrator signs in, adds the models, writes the
	// baseline and the groups, and sets up the teams.
	s.signIn(people[0], daysAgo(63, 9, 12))
	//
	// Models are the one thing not added through the API, and only because of
	// their ids: the usage chart colours its series in model-id order, and the
	// API's ids are random, so the chart would change colour on every run.
	// store.CreateModel takes an id (standalone seeds its models the same
	// way), and the history entry is the one the API handler writes.
	for i, m := range models {
		row := &store.Model{
			ID: fmt.Sprintf("m_%02d_%s", i+1, m.key), Name: m.name, Provider: m.provider, Hosting: m.hosting,
			Endpoint: m.endpoint, ModelID: m.modelID, ContextWindow: m.context,
			CostIn: m.in, CostCachedIn: m.cached, CostOut: m.out, APIKey: "sk-redacted-" + m.key,
		}
		check(s.st.CreateModel(row))
		hosting := "external"
		if m.hosting == "internal" {
			hosting = "internally hosted"
		}
		check(s.st.RecordChange(s.users[people[0].key].ID, "model", "created", row.ID, row.Name,
			"Added "+hosting+" model "+row.Name+" ("+row.Provider+").", nil))
		s.modelIDs[m.key] = row.ID
		s.stamp(daysAgo(63, 9, 20+i*3))
	}
	for gi, g := range groups {
		if g.key == "pci" {
			continue // written later in the story
		}
		s.writeGroup(g, daysAgo(62-float64(gi)*0.1, 10, 5+gi*7))
	}
	for i, t := range teams {
		var ids []string
		for _, gk := range t.groups {
			if id, ok := s.groupIDs[gk]; ok {
				ids = append(ids, id)
			}
		}
		var out struct{ ID string }
		s.api(s.adminToken, "POST", "/api/v1/teams", map[string]any{
			"name": t.name, "slug": t.slug, "description": t.description, "groupIds": ids,
		}, &out)
		s.teamIDs[t.key] = out.ID
		s.stamp(daysAgo(61, 14, 10+i*4))
	}

	// Over the following weeks people sign in, and are put in their teams.
	for i, p := range people[1:] {
		at := daysAgo(60-float64(i)*1.3, 9+i%6, (i*17)%60)
		s.signIn(p, at)
		s.setTeams(p, at.Add(40*time.Minute))
	}
	s.setTeams(people[0], daysAgo(60, 11, 0))
	var oscar = s.users["oscar"]
	s.api(s.adminToken, "PUT", "/api/v1/users/"+oscar.ID+"/role", map[string]any{"role": "admin"}, nil)
	s.stamp(daysAgo(44, 15, 32))

	// Image builds go through the proxy from here on.
	s.api(s.adminToken, "PATCH", "/api/v1/org", map[string]any{"builds": map[string]any{"network": "proxied"}}, nil)
	s.stamp(daysAgo(40, 11, 3))

	// From here on the second administrator, from Security, does some of it.
	s.actor = s.tokens["oscar"]

	// Payments comes into scope for PCI three weeks ago.
	for _, g := range groups {
		if g.key == "pci" {
			s.writeGroup(g, daysAgo(21, 13, 40))
		}
	}
	s.api(s.as(), "PUT", "/api/v1/teams/"+s.teamIDs["payments"]+"/groups", map[string]any{
		"groupIds": []string{s.groupIDs["registries"], s.groupIDs["internal"], s.groupIDs["pci"]},
	}, nil)
	s.stamp(daysAgo(21, 14, 2))

	s.actor = ""

	// A model tried and switched off again.
	for _, key := range disabledModels {
		s.api(s.adminToken, "PUT", "/api/v1/models/"+s.modelIDs[key]+"/status", map[string]any{"status": "disabled"}, nil)
		s.stamp(daysAgo(35, 10, 18))
	}

	// A policy turned off while an incident was looked into, and back on.
	s.api(s.as(), "PUT", "/api/v1/policies/"+s.policyIDs["Staging API writes"]+"/enabled", map[string]any{"enabled": false}, nil)
	s.stamp(daysAgo(9, 16, 12))
	s.api(s.as(), "PUT", "/api/v1/policies/"+s.policyIDs["Staging API writes"]+"/enabled", map[string]any{"enabled": true}, nil)
	s.stamp(daysAgo(8, 9, 30))

	// Network access requests, filed by the people who were blocked, oldest
	// first so the REQ- numbers run in the order they were asked for.
	filed := append([]accessRequest(nil), requests...)
	sort.SliceStable(filed, func(i, j int) bool { return filed[i].daysAgo > filed[j].daysAgo })
	for _, r := range filed {
		s.fileRequest(r)
	}

	// A late addition to the baseline.
	s.actor = s.tokens["oscar"]
	s.addPolicy("baseline", policy{"Block anonymous webhooks", "Request-catcher services receive whatever is sent to them, from anyone.",
		network("deny", "*.webhook.site", anyVerb, "/*")}, daysAgo(0, 8, 41))

	s.actor = ""

	s.usage()
	s.sessions()

	// When each person was last seen: the sign-ins above said "when they
	// joined", and the sessions say when they last worked.
	for _, p := range people[1:] { // the browser signs in as people[0], which is "just now"
		u := s.users[p.key]
		last := daysAgo(0, 7, 0).Add(-time.Duration(rng.Float64()*20) * time.Hour)
		s.exec(`UPDATE users SET last_active_at = ? WHERE id = ?`, last.Format(time.RFC3339), u.ID)
	}
}

// signIn records a person the identity provider has signed in, exactly as the
// OIDC callback does, and gives them a machine token as `john login` would.
func (s *seeder) signIn(p person, at time.Time) {
	adminEmail := ""
	if p.admin {
		adminEmail = email(p)
	}
	u, err := s.st.UpsertSSOUser("sso|"+p.key, email(p), p.name, adminEmail)
	check(err)
	s.users[p.key] = u
	tok, err := s.st.CreateMachineToken(u.ID, strings.ToLower(strings.Fields(p.name)[0])+"-mbp", 365*24*time.Hour)
	check(err)
	s.tokens[p.key] = tok
	// Their client registers a trace key the first time it opens a session.
	id, err := age.GenerateX25519Identity()
	check(err)
	check(s.st.SetUserTracePubKey(u.ID, id.Recipient().String()))
	u.TracePubKey = id.Recipient().String()
	if p.admin && s.adminToken == "" {
		s.adminToken = tok
	}
	s.stamp(at)
}

func (s *seeder) as() string {
	if s.actor != "" {
		return s.actor
	}
	return s.adminToken
}

func (s *seeder) setTeams(p person, at time.Time) {
	var ids []string
	for _, t := range p.teams {
		ids = append(ids, s.teamIDs[t])
	}
	s.api(s.adminToken, "PUT", "/api/v1/users/"+s.users[p.key].ID+"/teams", map[string]any{"teamIds": ids}, nil)
	s.stamp(at)
}

func (s *seeder) writeGroup(g group, at time.Time) {
	var out struct{ ID string }
	s.api(s.as(), "POST", "/api/v1/groups", map[string]any{
		"name": g.name, "description": g.description, "baseline": g.baseline,
	}, &out)
	s.groupIDs[g.key] = out.ID
	s.stamp(at)
	for i, p := range g.policies {
		s.addPolicy(g.key, p, at.Add(time.Duration(i+1)*90*time.Second))
	}
}

func (s *seeder) addPolicy(groupKey string, p policy, at time.Time) {
	spec := p.spec
	if spec["kind"] == "model-routing" {
		keys := spec["modelIds"].([]string)
		ids := make([]string, len(keys))
		for i, k := range keys {
			ids[i] = s.modelIDs[k]
		}
		spec = copyMap(spec)
		spec["modelIds"] = ids
	}
	var out struct{ ID string }
	s.api(s.as(), "POST", "/api/v1/policies", map[string]any{
		"groupId": s.groupIDs[groupKey], "name": p.name, "description": p.description, "spec": spec,
	}, &out)
	s.policyIDs[p.name] = out.ID
	s.stamp(at)
}

// fileRequest is a developer being blocked, asking for access, and — for the
// older ones — an administrator answering.
func (s *seeder) fileRequest(r accessRequest) {
	who := s.users[r.who]
	created := daysAgo(r.daysAgo, 0, 0)
	var evidence []string
	for i, raw := range r.evidence {
		u, _ := url.Parse(raw)
		n := &store.NetworkRequest{
			At: created.Add(-time.Duration(len(r.evidence)-i) * 3 * time.Minute), UserID: who.ID,
			SessionID: "", Method: "GET", Scheme: u.Scheme, Host: u.Hostname(), Port: 443,
			Path: u.Path, Status: "unknown",
		}
		if methods, _ := r.spec["methods"].([]string); len(methods) == 1 {
			n.Method = methods[0]
		}
		check(s.st.InsertNetworkRequest(n))
		evidence = append(evidence, n.ID)
	}
	var groupID *string
	groupName := "New group"
	for _, g := range groups {
		if g.key == r.group {
			id := s.groupIDs[g.key]
			groupID, groupName = &id, g.name
		}
	}
	var out struct{ ID, Ref string }
	s.api(s.tokens[r.who], "POST", "/api/v1/access-requests", map[string]any{
		"title": r.title, "justification": r.justification, "teamId": s.teamIDs[r.team],
		"groupId": groupID, "groupName": groupName, "spec": r.spec, "evidenceRequestIds": evidence,
	}, &out)
	s.stamp(created)

	for _, c := range r.comments {
		body := map[string]any{"body": c.body, "needsInfo": r.outcome == "needs-info" && c.who != r.who}
		s.api(s.tokens[c.who], "POST", "/api/v1/access-requests/"+out.ID+"/comments", body, nil)
		s.stamp(daysAgo(c.daysAgo, 0, 0))
	}
	switch r.outcome {
	case "approved", "rejected":
		decider := s.adminToken
		if r.decider != "" {
			decider = s.tokens[r.decider]
		}
		s.api(decider, "POST", "/api/v1/access-requests/"+out.ID+"/decision", map[string]any{
			"status": r.outcome, "groupId": groupID,
		}, nil)
		s.stamp(daysAgo(r.decidedAt, 0, 0))
	}
}

// usage writes sixty days of the rollups the agent loop keeps, with the same
// call it uses. Weekdays are busy, weekends are not, adoption grows, and each
// team spends on the models its groups give it.
func (s *seeder) usage() {
	mix := map[string]map[string]float64{
		"platform": {"sonnet": 0.55, "opus": 0.30, "gpt": 0.05, "coder": 0.10},
		"payments": {"sonnet": 0.50, "coder": 0.50},
		"mobile":   {"sonnet": 0.85, "coder": 0.15},
		"data":     {"sonnet": 0.55, "coder": 0.30, "llama": 0.15},
		"security": {"sonnet": 0.35, "opus": 0.55, "coder": 0.10},
	}
	for _, p := range people {
		u := s.users[p.key]
		joined := u.CreatedAt
		if row, err := s.st.UserByID(u.ID); err == nil {
			joined = row.CreatedAt
		}
		for d := 60; d >= 0; d-- {
			day := now.AddDate(0, 0, -d)
			if day.Before(joined) {
				continue
			}
			wd := day.Weekday()
			busy := 1.0
			if wd == time.Saturday || wd == time.Sunday {
				if rng.Float64() < 0.8 {
					continue
				}
				busy = 0.3
			}
			if rng.Float64() < 0.08 {
				continue // a day off
			}
			growth := 0.7 + 0.3*float64(60-d)/60
			requests := p.intensity * busy * growth * (150 + rng.Float64()*200)
			if d == 0 {
				requests *= 0.45 // the day is not over yet
			}
			for _, m := range models { // in a fixed order, so the random numbers are too
				mk, share := m.key, mix[p.teams[0]][m.key]
				r := requests * share * (0.7 + rng.Float64()*0.6)
				if r < 1 {
					continue
				}
				in := r * (22_000 + rng.Float64()*14_000)
				cached := in * (0.78 + rng.Float64()*0.1)
				out := r * (900 + rng.Float64()*700)
				cost := ((in-cached)*m.in + cached*m.cached + out*m.out) / 1e6
				check(s.st.AddUsage(day, u.ID, s.modelIDs[mk], store.UsageDelta{
					Requests: int(r), TokensIn: int(in), TokensOut: int(out), TokensCached: int(cached),
					Sessions: int(math.Max(1, r/45)), Allowed: int(r * 0.4), Approvals: int(r * 0.01),
					Blocked: int(r * 0.015), CostUSD: cost,
				}))
			}
		}
	}
}

// sessions writes the conversations the traces page lists: the session row
// the hub creates, and a trace file sealed exactly as the hub seals one, with
// the organization's key.
func (s *seeder) sessions() {
	w, err := trace.NewWriter(s.traceDir, s.orgRecipient())
	check(err)

	stories := []sessionStory{showcase}
	for _, o := range otherSessions {
		st := sessionStory{who: o.who, workspace: "/Users/" + o.who + "/src/" + o.repo, hoursAgo: o.hoursAgo, model: "sonnet", turns: o.turns}
		st.records = []rec{
			{0, "session.open", map[string]any{"workspace": st.workspace, "client": map[string]string{"version": "1.4.0", "os": "darwin/arm64"}}},
			{5 * time.Second, "sandbox.state", map[string]any{"state": "ready"}},
			{40 * time.Second, "prompt", map[string]any{"turn": "t1", "text": o.prompt, "attachments": []any{}}},
			{44 * time.Second, "tool.call", toolCall("c1", "run", map[string]any{"cmd": "git status --short"})},
			{45 * time.Second, "tool.result", map[string]any{"id": "c1", "status": "ok", "exit": 0}},
			{3 * time.Minute, "turn.end", map[string]any{"turn": "t1", "reason": "done"}},
		}
		stories = append(stories, st)
	}
	for _, st := range stories {
		u := s.users[st.who]
		started := now.Add(-time.Duration(st.hoursAgo * float64(time.Hour))).Truncate(time.Second)
		row, err := s.st.CreateSession(u.ID, "ws-"+st.workspace, st.workspace, st.who+"-mbp", s.modelIDs[st.model])
		check(err)
		check(s.st.AddSessionUsage(row.ID, st.turns, st.turns*48_000, st.turns*1_900, float64(st.turns)*0.41))
		last := st.records[len(st.records)-1].offset
		s.exec(`UPDATE sessions SET started_at = ?, status = 'ended', ended_at = ? WHERE id = ?`,
			started.Format(time.RFC3339), started.Add(last+time.Minute).Format(time.RFC3339), row.ID)

		sess := w.Session(row.ID, started)
		key, _, err := sess.Create(u.ID, u.TracePubKey, started)
		check(err)
		for _, r := range st.records {
			payload := r.payload
			at := started.Add(r.offset)
			// The hub writes ids where the story has names.
			if m, ok := payload.(map[string]any); ok && r.kind == "model.narrow" {
				m = copyMap(m)
				m["policy"] = s.policyIDs[m["policy"].(string)]
				payload = m
			}
			if m, ok := payload.(map[string]any); ok && r.kind == "model.choice" {
				m = copyMap(m)
				m["model"] = s.modelIDs[m["model"].(string)]
				payload = m
			}
			if m, ok := payload.(map[string]any); ok && r.kind == "proxy.decision" {
				m = copyMap(m)
				m["at"] = at.Format(time.RFC3339)
				payload = m
				// The hub records every decision as proxy traffic too.
				u2, _ := url.Parse(m["url"].(string))
				status := map[string]string{"allowed": "allowed", "asked": "approved", "unknown": "unknown", "denied": "denied"}[m["verdict"].(string)]
				check(s.st.InsertNetworkRequest(&store.NetworkRequest{
					At: at, UserID: u.ID, SessionID: row.ID, Method: m["method"].(string), Scheme: u2.Scheme,
					Host: u2.Hostname(), Port: 443, Path: u2.Path, Status: status, Reason: fmt.Sprint(m["policy"]),
				}))
			}
			data, err := json.Marshal(payload)
			check(err)
			check(sess.Append(key, trace.Record{At: at, Session: row.ID, Actor: u.ID, Kind: r.kind, Payload: data}))
		}
	}
}

func (s *seeder) orgRecipient() string {
	org, err := s.st.Org()
	check(err)
	return org.TraceKey
}

// ---------------------------------------------------------------- plumbing

// stamp moves every row the last step wrote back to when the story says it
// happened. "The last step" is every timestamp still later than the moment
// seeding began: anything already stamped is in the past, and stays put.
func (s *seeder) stamp(at time.Time) {
	cut := seedStart.Format(time.RFC3339)
	for _, c := range []struct {
		table, column string
		nano          bool
	}{
		{"changes", "at", true},
		{"request_comments", "at", true},
		{"policies", "updated_at", false},
		{"policy_groups", "updated_at", false},
		{"teams", "created_at", false},
		{"models", "added_at", false},
		{"users", "created_at", false},
		{"users", "last_active_at", false},
		{"access_requests", "created_at", false},
		{"access_requests", "decided_at", false},
	} {
		v := at.UTC().Format(time.RFC3339)
		if c.nano {
			v = at.UTC().Format(time.RFC3339Nano)
		}
		s.exec(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s >= ?`, c.table, c.column, c.column), v, cut)
	}
}

func (s *seeder) exec(q string, args ...any) {
	_, err := s.st.DB().Exec(q, args...)
	check(err)
}

// api calls the backend's HTTP API with a machine token, as the CLI and the
// console do, and fails loudly on anything but a 200.
func (s *seeder) api(token, method, path string, body, out any) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		check(err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	check(err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	check(err)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		die(fmt.Sprintf("%s %s: %d %s", method, path, res.StatusCode, strings.TrimSpace(string(raw))))
	}
	if out != nil {
		check(json.Unmarshal(raw, out))
	}
}

// daysAgo is a moment in the story: that many days back (fractions allowed),
// at hh:mm UTC. With hh and mm both zero, it is simply now minus the days.
func daysAgo(days float64, hh, mm int) time.Time {
	t := now.Add(-time.Duration(days * 24 * float64(time.Hour)))
	if hh == 0 && mm == 0 {
		return t
	}
	t = time.Date(t.Year(), t.Month(), t.Day(), hh, mm, 0, 0, time.UTC)
	if !t.Before(seedStart) {
		t = seedStart.Add(-time.Hour)
	}
	return t
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func check(err error) {
	if err != nil {
		die(err.Error())
	}
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, "console-backend:", msg)
	os.Exit(1)
}
