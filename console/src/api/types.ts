/* The shapes the console renders.
 *
 * These are the API contract, not convenient view models: they are what the
 * Go backend's JSON looks like, and `client.ts` is the only place that reads
 * it — everything else in the console reads these.
 *
 * Vocabulary follows the readme exactly — policy *group*, baseline group,
 * network access *request*, and network decisions of unknown, ask developer,
 * always allow or deny — so the console, CLI and docs use one set of words.
 */

export type ID = string;
/** ISO-8601, always UTC with a Z suffix. */
export type Timestamp = string;

/* ---------- people & teams ---------- */

export type Role = "admin" | "member";

export interface User {
  id: ID;
  name: string;
  email: string;
  initials: string;
  /** Organization-wide role. There are two: member and administrator. */
  role: Role;
  title: string;
  status: "active" | "suspended";
  teamIds: ID[];
  ssoSubject: string;
  createdAt: Timestamp;
  lastActiveAt: Timestamp | null;
}

export interface Team {
  id: ID;
  name: string;
  slug: string;
  description: string;
  memberIds: ID[];
  policyGroupIds: ID[];
  createdAt: Timestamp;
}

/* ---------- policies ---------- */

export type PolicyType = "network" | "replacement" | "model-routing" | "workspace";

/** What the proxy does with a request this policy matches. */
export type NetworkDecision = "always-allow" | "ask-developer" | "deny";

export interface NetworkAttributeMatch {
  name: string;
  /** Exact value or a value containing `*` wildcards. */
  value: string;
}

export interface NetworkSpec {
  kind: "network";
  methods: string[];
  scheme: "https" | "http" | "any";
  host: string;
  port: number | null;
  path: string;
  /** Header names are matched case-insensitively. Bodies are never content-matched. */
  headerMatches: NetworkAttributeMatch[];
  queryMatches: NetworkAttributeMatch[];
  decision: NetworkDecision;
  /** Requests with a larger body are refused outright. */
  maxBodyBytes: number | null;
}

export interface ReplacementSpec {
  kind: "replacement";
  pattern: string;
  /**
   * What a match becomes. Applied by the backend to everything on its way to
   * a model, with nothing to opt out of.
   *
   * A `#` in the template is replaced with a stable alias for that particular
   * matched value: the same secret always becomes the same alias, and two
   * different secrets never collide. That keeps the text the model sees
   * internally consistent — it can still reason about "the account in the
   * stack trace" — without the real value ever reaching a model provider.
   */
  replacement: string;
  example: { before: string; after: string } | null;
}

export interface ModelRoutingSpec {
  kind: "model-routing";
  /** `assign` adds models to the set a team may use; `restrict` narrows it. */
  mode: "assign" | "restrict";
  modelIds: ID[];
  /**
   * Restrict only: literal, case-insensitive strings the backend matches
   * against everything it is about to send to a model — the prompt, file
   * contents, command output, a diff.
   *
   * One hit narrows the whole session to `modelIds`, and the developer is
   * asked which of the remaining models to continue on. A session never
   * un-narrows: once C++ has been seen, it has been seen.
   *
   * Deliberately not a classifier. A keyword list is inspectable, testable
   * and identical on every machine — the same reason policy decisions are
   * made outside the model everywhere else in John.
   */
  keywords: string[];
}

export interface WorkspaceSpec {
  kind: "workspace";
  paths: string[];
  mode: "exclude" | "read-only" | "no-delete";
}

export type PolicySpec = NetworkSpec | ReplacementSpec | ModelRoutingSpec | WorkspaceSpec;

export interface Policy {
  id: ID;
  groupId: ID;
  type: PolicyType;
  name: string;
  description: string;
  enabled: boolean;
  version: number;
  updatedAt: Timestamp;
  updatedBy: ID;
  spec: PolicySpec;
}

export interface PolicyGroup {
  id: ID;
  name: string;
  description: string;
  /** Attached to every team automatically; cannot be weakened by any group. */
  baseline: boolean;
  version: number;
  updatedAt: Timestamp;
  updatedBy: ID;
  teamIds: ID[];
}

/* ---------- network access requests ----------
 *
 * The only thing a member can ask for. Everything else — replacements,
 * routing, workspace protection — is an administrator's to write directly,
 * because those are org-wide judgements rather than "I am blocked on this
 * one host".
 */

export type RequestStatus = "pending" | "approved" | "rejected" | "needs-info";

export interface RequestComment {
  id: ID;
  authorId: ID;
  at: Timestamp;
  body: string;
}

export interface NetworkAccessRequest {
  id: ID;
  /** Human-facing reference, e.g. `REQ-184`. */
  ref: string;
  title: string;
  requesterId: ID;
  teamId: ID;
  status: RequestStatus;
  justification: string;
  createdAt: Timestamp;
  decidedAt: Timestamp | null;
  decidedById: ID | null;
  decisionNote: string | null;
  /** The policy the requester is asking for, ready to be edited and merged. */
  proposed: { groupId: ID | null; groupName: string; spec: NetworkSpec };
  /** Blocked proxy requests that prompted this. */
  evidenceRequestIds: ID[];
  comments: RequestComment[];
}

/* ---------- proxy traffic ---------- */

/* What became of one request the proxy saw.
 *
 * `unknown` and `denied` are both blocked, and they are kept apart because the
 * remedy is not the same: a denied request was considered by a policy and
 * refused, while an unknown one is the absence of a policy — which is the only
 * case a member can do anything about. */
export type NetworkStatus =
  | "allowed"
  | "unknown"
  | "denied"
  | "awaiting-approval"
  | "approved"
  | "rejected"
  | "granted";

export interface NetworkRequest {
  id: ID;
  at: Timestamp;
  userId: ID;
  sessionId: ID;
  method: string;
  scheme: "https" | "http";
  host: string;
  port: number;
  path: string;
  query: string;
  status: NetworkStatus;
  /** The policy that decided this. Null exactly when the status is unknown. */
  policyId: ID | null;
  reason: string;
  bytesOut: number;
  bytesIn: number;
  durationMs: number;
  responseStatus: number | null;
  headers: Array<[string, string]>;
  bodyPreview: string | null;
  contentType: string | null;
}

/* ---------- sessions ----------
 *
 * A session is what a trace record and a proxy request hang off. The console
 * does not render one: reading what happened inside a session is what the
 * encrypted traces are for, and the sandbox it ran in belongs to the CLI. */

export interface Session {
  id: ID;
  userId: ID;
  project: string;
  workingDir: string;
  branch: string;
  hostname: string;
  sandboxId: ID;
  status: "active" | "idle" | "ended";
  startedAt: Timestamp;
  endedAt: Timestamp | null;
  modelId: ID;
  turns: number;
  tokensIn: number;
  tokensOut: number;
  commands: number;
  networkRequests: number;
  blockedRequests: number;
}

/* ---------- models ---------- */

export interface Model {
  id: ID;
  name: string;
  provider: string;
  /** Where the weights run. Internal means inside the organization. */
  hosting: "internal" | "external";
  endpoint: string;
  /** The provider's own identifier, e.g. `claude-opus-5`. */
  modelId: string;
  contextWindow: number;
  status: "enabled" | "disabled";
  /** Estimate only — what the console multiplies token counts by. */
  costPerMTokIn: number;
  /** Cached input is billed at its own rate by every provider that has it. */
  costPerMTokCachedIn: number;
  costPerMTokOut: number;
  addedAt: Timestamp;
}

/* ---------- usage, traces, org ---------- */

export interface ModelUsage {
  modelId: ID;
  requests: number;
  tokens: number;
  costUsd: number;
}

export interface UsageDay {
  date: string; // YYYY-MM-DD
  requests: number;
  tokensIn: number;
  tokensOut: number;
  /** Prompt tokens served from cache — billed differently, so counted apart. */
  tokensCached: number;
  sessions: number;
  allowed: number;
  approvals: number;
  blocked: number;
  costUsd: number;
  /** The day split by model, in fixed model order. Drives the spend chart. */
  byModel: ModelUsage[];
}

/* ---------- change history ----------
 *
 * "Who changed the controls, and when" is a question the readme promises an
 * answer to, so there has to be somewhere to read it. This is that record: one
 * entry per configuration or policy change, in the clear — it says a rule
 * changed and who changed it, never what a session did, which is what the
 * encrypted traces are for.
 *
 * It is append-only. Nothing edits or deletes an entry, including the code
 * that writes them.
 */

export type ChangeEntity =
  | "policy"
  | "group"
  | "team"
  | "member"
  | "model"
  | "request"
  | "settings"
  | "trace";

export type ChangeAction =
  | "created"
  | "updated"
  | "enabled"
  | "disabled"
  | "deleted"
  | "attached"
  | "detached"
  | "approved"
  | "rejected";

export interface ChangeRecord {
  id: ID;
  at: Timestamp;
  actorId: ID;
  entity: ChangeEntity;
  entityId: ID;
  /** The name the thing had when it changed, so a rename stays readable. */
  entityName: string;
  action: ChangeAction;
  /** The version this change produced, where the thing carries one. */
  version: number | null;
  /** One line, in the console's own vocabulary. */
  summary: string;
}

export type TraceCategory =
  | "session"
  | "prompt"
  | "tool"
  | "network"
  | "model"
  | "workspace";

/**
 * One session an administrator can open. Everything here is metadata the
 * database holds in the clear — who, when, how long. What was said is in the
 * sealed file and never passes through this shape.
 */
export interface TraceSession {
  id: ID;
  actorId: ID;
  actorName: string;
  actorEmail: string;
  project: string;
  startedAt: Timestamp;
  status: string;
  turns: number;
}

/**
 * One record out of a session's trace, after it has been decrypted in the
 * browser. It never exists on the wire.
 */
export interface TraceEntry {
  id: ID;
  at: Timestamp;
  sessionId: ID;
  actorId: ID;
  category: TraceCategory;
  /** The record's kind on the wire, e.g. "tool.call". */
  action: string;
  /** A one-line rendering of the payload. */
  summary: string;
  /** The payload itself, for the drawer. */
  payload: unknown;
  /** The line's position in the file: a gap means a line was removed. */
  seq: number;
}

/** Someone an administrator can grant a session to. */
export interface TraceRecipient {
  id: ID;
  name: string;
  email: string;
  /** Empty when their client has never registered a key. */
  recipient: string;
}

export interface OrgSettings {
  name: string;
  /**
   * The backend `john` runs in its own process for one person, to try john
   * before an organization runs one. Sign-in is automatic and fixed.
   */
  standalone: boolean;
  /**
   * Membership and teams are held here, not mapped from a groups claim, and
   * nothing is synced from the provider — the identity provider says who a
   * person is, and john says what they may do.
   */
  sso: {
    provider: string;
    issuer: string;
    clientId: string;
    enforced: boolean;
    /**
     * The email domains sign-in admits, lowercase, no @. Empty admits anyone
     * the identity provider vouches for. With Google, the account must also
     * belong to the Workspace that owns the domain.
     */
    allowedDomains: string[];
  };
  traceKey: {
    fingerprint: string;
    algorithm: string;
    createdAt: Timestamp;
    custodian: string;
  };
  /**
   * The directory encrypted session traces are written to. They are kept
   * forever: nothing expires them, and removing history is a deliberate act
   * by whoever operates the store.
   */
  traceStore: { location: string };
  /**
   * How long a developer's machine may keep renewing its own credentials
   * before the identity provider has to confirm the person again. Zero turns
   * the cap off: the machine keeps working until someone revokes it.
   */
  machineSessions: { maxDays: number };
  /**
   * Where the network traffic of a sandbox image build goes. A build is how a
   * project gets its compilers and its internal packages installed, so it is
   * the one thing in john with a reason to want the network wide open — and
   * `direct` is the default for that reason. `proxied` holds it to the same
   * network policies as everything else.
   */
  builds: { network: BuildNetwork };
  /**
   * What a prompt may carry besides text. john's content controls read text:
   * replacements rewrite what is on its way to a model and routing keywords
   * hold it back, and neither can see inside a screenshot. `guarded` — the
   * default — allows attachments except in a session that is under one of
   * those controls, `on` allows them always, `off` never.
   */
  prompts: { attachments: AttachmentPolicy };
  backend: {
    version: string;
    /** Only used to show the OIDC redirect URI. */
    host: string;
    startedAt: Timestamp;
    dbSizeMb: number;
    traceSizeMb: number;
  };
}

/* ---------- session identity ---------- */

export type BuildNetwork = "direct" | "proxied";

export type AttachmentPolicy = "guarded" | "on" | "off";

export interface Viewer {
  user: User;
  /** Admin surfaces are hidden entirely when false. */
  isAdmin: boolean;
}
