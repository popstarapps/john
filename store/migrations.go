package store

// A Migration moves the schema forward one version. The list is append-only:
// entries are never edited or removed once they have shipped, because a
// database out in an organization has already run them. Tests assert the
// versions are contiguous from 1.
//
// Everything before the first deployment was folded into version 1.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

var migrations = []Migration{
	{
		Version: 1,
		Name:    "schema",
		SQL: `
-- The organization. A singleton: the backend serves exactly one.
CREATE TABLE org (
	id                   INTEGER PRIMARY KEY CHECK (id = 1),
	name                 TEXT NOT NULL DEFAULT '',
	oidc_issuer          TEXT NOT NULL DEFAULT '',
	oidc_client_id       TEXT NOT NULL DEFAULT '',
	oidc_client_secret   TEXT NOT NULL DEFAULT '',
	sso_provider         TEXT NOT NULL DEFAULT '',
	sso_enforced         INTEGER NOT NULL DEFAULT 1,
	admin_email          TEXT NOT NULL DEFAULT '',
	trace_key            TEXT NOT NULL DEFAULT '',  -- age public recipient; writers can encrypt, never decrypt
	trace_path           TEXT NOT NULL DEFAULT '',
	snapshot_signing_key TEXT NOT NULL DEFAULT '',  -- Ed25519 private key, base64
	setup_complete       INTEGER NOT NULL DEFAULT 0,
	created_at           TEXT NOT NULL,
	-- How long a machine may keep refreshing before the identity provider has
	-- to confirm the person again. Days; 0 turns the cap off entirely.
	machine_session_max_days INTEGER NOT NULL DEFAULT 90,
	-- Where an image build's network traffic goes: direct or proxied. Empty
	-- means direct, which is what an unset value means to the client.
	build_network        TEXT NOT NULL DEFAULT '',
	-- What a prompt may carry besides text: guarded, on or off. Empty means
	-- guarded.
	attachments          TEXT NOT NULL DEFAULT '',
	-- The email domains sign-in admits, comma-separated. The identity provider
	-- is only as narrow as it is configured: a Google client set to External
	-- vouches for every Google account. Empty admits any domain.
	sso_allowed_domains  TEXT NOT NULL DEFAULT ''
);

-- One-time tokens printed by --setup. Stored hashed; spent on use.
CREATE TABLE setup_tokens (
	token_hash TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	used_at    TEXT
);

CREATE TABLE users (
	id             TEXT PRIMARY KEY,
	email          TEXT NOT NULL UNIQUE,
	name           TEXT NOT NULL DEFAULT '',
	initials       TEXT NOT NULL DEFAULT '',
	title          TEXT NOT NULL DEFAULT '',
	role           TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
	status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
	sso_subject    TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	last_active_at TEXT,
	-- The developer's age recipient, registered by their client at sign-in.
	-- The backend holds the public half only: it wraps each session key for
	-- this recipient so the developer can read their own sessions back, and
	-- can never open what it wrapped. Empty until a client has registered one.
	trace_pubkey   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX users_sso_subject ON users (sso_subject);

CREATE TABLE teams (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	slug        TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL
);

CREATE TABLE team_members (
	team_id TEXT NOT NULL REFERENCES teams (id),
	user_id TEXT NOT NULL REFERENCES users (id),
	PRIMARY KEY (team_id, user_id)
);

CREATE TABLE policy_groups (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	baseline    INTEGER NOT NULL DEFAULT 0,
	version     INTEGER NOT NULL DEFAULT 1,
	updated_at  TEXT NOT NULL,
	updated_by  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE policies (
	id          TEXT PRIMARY KEY,
	group_id    TEXT NOT NULL REFERENCES policy_groups (id),
	type        TEXT NOT NULL CHECK (type IN ('network', 'replacement', 'model-routing', 'workspace')),
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	enabled     INTEGER NOT NULL DEFAULT 1,
	version     INTEGER NOT NULL DEFAULT 1,
	spec        TEXT NOT NULL,  -- JSON, the discriminated union the console defines
	updated_at  TEXT NOT NULL,
	updated_by  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX policies_group ON policies (group_id);

CREATE TABLE team_policy_groups (
	team_id  TEXT NOT NULL REFERENCES teams (id),
	group_id TEXT NOT NULL REFERENCES policy_groups (id),
	PRIMARY KEY (team_id, group_id)
);

CREATE TABLE models (
	id             TEXT PRIMARY KEY,
	name           TEXT NOT NULL,
	provider       TEXT NOT NULL DEFAULT '',
	hosting        TEXT NOT NULL DEFAULT 'external' CHECK (hosting IN ('internal', 'external')),
	endpoint       TEXT NOT NULL DEFAULT '',
	model_id       TEXT NOT NULL DEFAULT '',  -- the provider's own identifier
	context_window INTEGER NOT NULL DEFAULT 0,
	status         TEXT NOT NULL DEFAULT 'enabled' CHECK (status IN ('enabled', 'disabled')),
	cost_in        REAL NOT NULL DEFAULT 0,
	cost_cached_in REAL NOT NULL DEFAULT 0,
	cost_out       REAL NOT NULL DEFAULT 0,
	api_key        TEXT NOT NULL DEFAULT '',  -- write-only through the API
	llm_model      TEXT NOT NULL,             -- JSON, unmarshals into agentkit llm.Model
	added_at       TEXT NOT NULL
);

CREATE TABLE machine_tokens (
	id           TEXT PRIMARY KEY,
	user_id      TEXT NOT NULL REFERENCES users (id),
	token_hash   TEXT NOT NULL UNIQUE,
	host         TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	expires_at   TEXT NOT NULL,
	last_used_at TEXT,
	revoked_at   TEXT,
	-- Which refresh chain minted this access token. Revoking a chain revokes
	-- the access tokens it issued at the same moment, rather than leaving them
	-- alive until their own expiry catches up.
	chain_id     TEXT NOT NULL DEFAULT ''
);

-- A machine credential is a pair. The access token in machine_tokens is
-- short-lived and presented on every request; the refresh token here is
-- long-lived, presented only at /api/v1/auth/refresh, and rotated each time
-- it is used.
--
-- A rotated token stays usable for a short grace window, so a client whose
-- network dropped the response it was waiting for is not locked out of its
-- own account. Presenting one after that window has no innocent explanation:
-- it means two copies of the credential exist, and the whole chain back to
-- the sign-in it descends from is revoked.
CREATE TABLE refresh_tokens (
	id            TEXT PRIMARY KEY,
	chain_id      TEXT NOT NULL,          -- every rotation of one sign-in shares this
	parent_id     TEXT NOT NULL DEFAULT '',
	user_id       TEXT NOT NULL REFERENCES users (id),
	token_hash    TEXT NOT NULL UNIQUE,
	host          TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	chain_start   TEXT NOT NULL,          -- the identity provider sign-in this descends from
	expires_at    TEXT NOT NULL,
	superseded_at TEXT,                   -- set when this token minted its successor
	last_used_at  TEXT,
	revoked_at    TEXT
);
CREATE INDEX refresh_tokens_chain ON refresh_tokens (chain_id);
CREATE INDEX refresh_tokens_user ON refresh_tokens (user_id);

CREATE TABLE web_sessions (
	id         TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES users (id),
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);

-- Agent sessions. state is agentkit's State JSON, written at message
-- boundaries; a backend restart loses the turn in flight, never the session.
CREATE TABLE sessions (
	id             TEXT PRIMARY KEY,
	user_id        TEXT NOT NULL REFERENCES users (id),
	workspace_key  TEXT NOT NULL,
	workspace_path TEXT NOT NULL DEFAULT '',
	hostname       TEXT NOT NULL DEFAULT '',
	model_id       TEXT NOT NULL DEFAULT '',
	narrowed       INTEGER NOT NULL DEFAULT 0,
	narrowed_models TEXT NOT NULL DEFAULT '',  -- JSON list; empty = not narrowed
	status         TEXT NOT NULL DEFAULT 'active',
	started_at     TEXT NOT NULL,
	ended_at       TEXT,
	turns          INTEGER NOT NULL DEFAULT 0,
	tokens_in      INTEGER NOT NULL DEFAULT 0,
	tokens_out     INTEGER NOT NULL DEFAULT 0,
	cost_usd       REAL NOT NULL DEFAULT 0,
	state          TEXT NOT NULL DEFAULT '',
	-- The turn the session is in the middle of, by the id its client gave it,
	-- so a backend that stopped mid-turn knows on its return that a turn was
	-- running. The id and nothing else: what the turn was doing is in state.
	turn_id        TEXT NOT NULL DEFAULT '',
	-- The highest turn number the session has started. A client carrying on
	-- a session, after a restart or with /resume, numbers on from here. The
	-- count of finished turns is not enough: a turn cut off and never
	-- finished is not in it, and its number would be given out again.
	last_turn      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_workspace ON sessions (user_id, workspace_key);

-- Daily usage rollups, one row per (day, user, model).
CREATE TABLE usage_days (
	date          TEXT NOT NULL,  -- YYYY-MM-DD
	user_id       TEXT NOT NULL REFERENCES users (id),
	model_id      TEXT NOT NULL DEFAULT '',
	requests      INTEGER NOT NULL DEFAULT 0,
	tokens_in     INTEGER NOT NULL DEFAULT 0,
	tokens_out    INTEGER NOT NULL DEFAULT 0,
	tokens_cached INTEGER NOT NULL DEFAULT 0,
	sessions      INTEGER NOT NULL DEFAULT 0,
	allowed       INTEGER NOT NULL DEFAULT 0,
	approvals     INTEGER NOT NULL DEFAULT 0,
	blocked       INTEGER NOT NULL DEFAULT 0,
	cost_usd      REAL NOT NULL DEFAULT 0,
	PRIMARY KEY (date, user_id, model_id)
);

-- Every decision the proxy reported: what this machine tried to reach.
CREATE TABLE network_requests (
	id              TEXT PRIMARY KEY,
	at              TEXT NOT NULL,
	user_id         TEXT NOT NULL DEFAULT '',
	session_id      TEXT NOT NULL DEFAULT '',
	method          TEXT NOT NULL DEFAULT '',
	scheme          TEXT NOT NULL DEFAULT '',
	host            TEXT NOT NULL DEFAULT '',
	port            INTEGER NOT NULL DEFAULT 0,
	path            TEXT NOT NULL DEFAULT '',
	query           TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL DEFAULT '',
	policy_id       TEXT NOT NULL DEFAULT '',
	reason          TEXT NOT NULL DEFAULT '',
	bytes_out       INTEGER NOT NULL DEFAULT 0,
	bytes_in        INTEGER NOT NULL DEFAULT 0,
	duration_ms     INTEGER NOT NULL DEFAULT 0,
	response_status INTEGER,
	headers         TEXT NOT NULL DEFAULT '[]',  -- JSON [[name, value], …]
	body_preview    TEXT,
	content_type    TEXT
);
CREATE INDEX network_requests_at ON network_requests (at);
CREATE INDEX network_requests_user ON network_requests (user_id, at);

-- Network access requests: a developer asking for a policy.
CREATE TABLE access_requests (
	id                  TEXT PRIMARY KEY,
	ref                 TEXT NOT NULL UNIQUE,  -- REQ-n
	title               TEXT NOT NULL,
	requester_id        TEXT NOT NULL REFERENCES users (id),
	team_id             TEXT NOT NULL DEFAULT '',
	status              TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'needs-info')),
	justification       TEXT NOT NULL DEFAULT '',
	created_at          TEXT NOT NULL,
	decided_at          TEXT,
	decided_by          TEXT,
	decision_note       TEXT,
	proposed_group_id   TEXT,
	proposed_group_name TEXT NOT NULL DEFAULT '',
	proposed_spec       TEXT NOT NULL,        -- JSON NetworkSpec
	evidence            TEXT NOT NULL DEFAULT '[]'  -- JSON list of network_requests ids
);

-- The conversation on an access request. A reviewer who cannot approve what
-- was asked for has somewhere to say why, and the requester has somewhere to
-- answer, which is what the needs-info status is for. Kept apart from the
-- change history: that record is what happened to the controls, this is
-- people talking about a request before anything happened at all.
CREATE TABLE request_comments (
	id         TEXT PRIMARY KEY,
	request_id TEXT NOT NULL REFERENCES access_requests (id),
	author_id  TEXT NOT NULL REFERENCES users (id),
	at         TEXT NOT NULL,
	body       TEXT NOT NULL
);
CREATE INDEX request_comments_request ON request_comments (request_id, at);

-- The change history: every change to the controls, forever, in the clear.
-- Session content never lands here; that is what the encrypted traces are for.
CREATE TABLE changes (
	id          TEXT PRIMARY KEY,
	at          TEXT NOT NULL,
	actor_id    TEXT NOT NULL DEFAULT '',
	entity      TEXT NOT NULL,
	entity_id   TEXT NOT NULL DEFAULT '',
	entity_name TEXT NOT NULL DEFAULT '',
	action      TEXT NOT NULL,
	version     INTEGER,
	summary     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX changes_at ON changes (at);

-- Bumped on any policy, group, team, or model mutation; the policy snapshot's
-- epoch and the trigger for policy.refresh pushes.
CREATE TABLE policy_epoch (
	id    INTEGER PRIMARY KEY CHECK (id = 1),
	epoch INTEGER NOT NULL DEFAULT 1
);
INSERT INTO policy_epoch (id, epoch) VALUES (1, 1);
`,
	},
}
