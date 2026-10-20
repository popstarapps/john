/* The console's only door to data.
 *
 * Every reader and mutation here is a fetch against the Go backend's
 * /api/v1 endpoints; the signatures and returned shapes are the contract in
 * `types.ts`, and nothing outside this file knows where the data came from.
 *
 * Mutations bump a version counter that `useQuery` subscribes to, so an
 * approval decision refreshes every view that shows it.
 *
 * Session traces are served sealed and opened in this browser: the private
 * key is typed into the Traces page, held in memory, and never sent here or
 * anywhere else.
 */
import type {
  ChangeEntity,
  ChangeRecord,
  TraceEntry,
  TraceRecipient,
  TraceSession,
  ID,
  Model,
  NetworkRequest,
  NetworkStatus,
  OrgSettings,
  Policy,
  PolicyGroup,
  NetworkAccessRequest,
  NetworkSpec,
  PolicySpec,
  PolicyType,
  RequestStatus,
  Role,
  Team,
  UsageDay,
  User,
  Viewer,
} from "./types";

/* ---------- subscriptions ---------- */

type Listener = () => void;
const listeners = new Set<Listener>();
let version = 0;

export function subscribe(fn: Listener): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
export function getVersion(): number {
  return version;
}
function bump() {
  version += 1;
  listeners.forEach((fn) => fn());
}

/* ---------- transport ---------- */

class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

async function api<T>(
  path: string,
  init?: RequestInit & { allowStatuses?: number[] },
): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    credentials: "same-origin",
    headers: init?.body ? { "Content-Type": "application/json" } : undefined,
    ...init,
  });
  if (init?.allowStatuses?.includes(res.status)) {
    return undefined as T;
  }
  if (res.status === 401) {
    // Nobody is signed in. The sign-in page is the only place that is fine.
    if (window.location.pathname !== "/signin") {
      window.location.href = "/signin";
    }
    throw new ApiError(401, "not signed in");
  }
  if (res.status === 503) {
    const body = await res.json().catch(() => null);
    if (body?.error === "setup_required" && window.location.pathname !== "/setup") {
      window.location.href = "/setup";
    }
    throw new ApiError(503, body?.error ?? "backend unavailable");
  }
  if (!res.ok) {
    const body = await res.json().catch(() => null);
    throw new ApiError(res.status, body?.error ?? `${res.status} from ${path}`);
  }
  return (await res.json()) as T;
}

function get<T>(path: string): Promise<T> {
  return api<T>(path);
}

async function send<T>(method: string, path: string, body?: unknown): Promise<T> {
  const out = await api<T>(path, { method, body: body === undefined ? undefined : JSON.stringify(body) });
  bump();
  return out;
}

function query(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") q.set(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : "";
}

/* ---------- identity ---------- */

export async function getViewer(): Promise<Viewer> {
  return get<Viewer>("/viewer");
}

/* ---------- change history ---------- */

export async function listChanges(q?: {
  entity?: ChangeEntity;
  search?: string;
  limit?: number;
}): Promise<ChangeRecord[]> {
  return get<ChangeRecord[]>(
    `/changes${query({ entity: q?.entity, search: q?.search, limit: q?.limit })}`,
  );
}

/* ---------- people & teams ---------- */

export async function listUsers(q?: { search?: string; teamId?: ID }): Promise<User[]> {
  const users = await get<User[]>(`/users${query({ search: q?.search, teamId: q?.teamId })}`);
  return [...users].sort((a, b) => a.name.localeCompare(b.name));
}

export async function getUser(id: ID): Promise<User | undefined> {
  return api<User | undefined>(`/users/${encodeURIComponent(id)}`, { allowStatuses: [404] });
}

export async function listTeams(): Promise<Team[]> {
  return get<Team[]>("/teams");
}

export async function getTeam(idOrSlug: ID): Promise<Team | undefined> {
  return api<Team | undefined>(`/teams/${encodeURIComponent(idOrSlug)}`, { allowStatuses: [404] });
}

export async function createTeam(input: {
  name: string;
  slug: string;
  description: string;
  groupIds: ID[];
}): Promise<Team> {
  return send<Team>("POST", "/teams", input);
}

/** Teams are held here, not mapped from an identity-provider claim, so
 *  membership is something an administrator edits. */
export async function setUserTeams(userId: ID, teamIds: ID[]): Promise<void> {
  await send("PUT", `/users/${encodeURIComponent(userId)}/teams`, { teamIds });
}

export async function setTeamGroups(teamId: ID, groupIds: ID[]): Promise<void> {
  await send("PUT", `/teams/${encodeURIComponent(teamId)}/groups`, { groupIds });
}

/**
 * Deleting a team takes its membership and its attached groups with it, so
 * everyone who was in it loses whatever those groups held. That cuts both
 * ways — network rules stop allowing, and workspace rules stop protecting —
 * which is why the page asks before calling this.
 */
export async function deleteTeam(teamId: ID): Promise<void> {
  await send("DELETE", `/teams/${encodeURIComponent(teamId)}`);
}

/** Promote to administrator, or back down to member. */
export async function setUserRole(userId: ID, role: Role): Promise<User> {
  return send<User>("PUT", `/users/${encodeURIComponent(userId)}/role`, { role });
}

/**
 * Suspend or reactivate someone.
 *
 * Suspension is how a person is offboarded: there is no delete, because their
 * id is written into sessions, usage and the change history. It takes effect
 * at once — their machines are signed out rather than left running until a
 * token expires — and it is not undone by reactivating, which lets them sign
 * in again but hands nothing back.
 *
 * An administrator cannot do either to themselves; the backend refuses it.
 */
export async function setUserStatus(userId: ID, status: User["status"]): Promise<User> {
  return send<User>("PUT", `/users/${encodeURIComponent(userId)}/status`, { status });
}

/* ---------- policies ---------- */

export async function listPolicyGroups(): Promise<PolicyGroup[]> {
  return get<PolicyGroup[]>("/groups");
}

export async function getPolicyGroup(id: ID): Promise<PolicyGroup | undefined> {
  return api<PolicyGroup | undefined>(`/groups/${encodeURIComponent(id)}`, { allowStatuses: [404] });
}

export async function createPolicyGroup(input: {
  name: string;
  description: string;
  baseline: boolean;
}): Promise<PolicyGroup> {
  return send<PolicyGroup>("POST", "/groups", input);
}

export async function listPolicies(q?: { groupId?: ID; type?: PolicyType }): Promise<Policy[]> {
  return get<Policy[]>(`/policies${query({ groupId: q?.groupId, type: q?.type })}`);
}

export async function getPolicy(id: ID): Promise<Policy | undefined> {
  return api<Policy | undefined>(`/policies/${encodeURIComponent(id)}`, { allowStatuses: [404] });
}

export async function createPolicy(input: {
  groupId: ID;
  name: string;
  description: string;
  spec: PolicySpec;
}): Promise<Policy> {
  return send<Policy>("POST", "/policies", input);
}

/** Rewrites a policy in place. `version` is the one the editor started from:
 *  if someone else saved since, the backend refuses rather than overwrite. */
export async function updatePolicy(
  id: ID,
  input: { name: string; description: string; spec: PolicySpec; version: number },
): Promise<Policy> {
  return send<Policy>("PUT", `/policies/${encodeURIComponent(id)}`, input);
}

export async function deletePolicy(id: ID): Promise<void> {
  await send("DELETE", `/policies/${encodeURIComponent(id)}`);
}

export async function setPolicyEnabled(id: ID, enabled: boolean): Promise<void> {
  await send("PUT", `/policies/${encodeURIComponent(id)}/enabled`, { enabled });
}

/** Every group that applies to a user: baseline ones plus their teams'. */
export async function groupsForUser(userId: ID): Promise<PolicyGroup[]> {
  return get<PolicyGroup[]>(`/users/${encodeURIComponent(userId)}/groups`);
}

/** The effective policy set for a user, flattened across their groups. */
export async function policiesForUser(userId: ID): Promise<Array<Policy & { group: PolicyGroup }>> {
  return get<Array<Policy & { group: PolicyGroup }>>(
    `/users/${encodeURIComponent(userId)}/policies`,
  );
}

/**
 * Models a user may start a session on. Assignments add; unconditional
 * restrictions narrow; keyword restrictions only bite mid-session.
 */
export async function modelsForUser(userId: ID): Promise<Model[]> {
  return get<Model[]>(`/users/${encodeURIComponent(userId)}/models`);
}

/* ---------- network access requests ---------- */

export async function listNetworkAccessRequests(q?: {
  status?: RequestStatus | "open";
  requesterId?: ID;
  teamId?: ID;
  search?: string;
}): Promise<NetworkAccessRequest[]> {
  return get<NetworkAccessRequest[]>(
    `/access-requests${query({
      status: q?.status,
      requesterId: q?.requesterId,
      teamId: q?.teamId,
      search: q?.search,
    })}`,
  );
}

export async function getNetworkAccessRequest(id: ID): Promise<NetworkAccessRequest | undefined> {
  return api<NetworkAccessRequest | undefined>(`/access-requests/${encodeURIComponent(id)}`, {
    allowStatuses: [404],
  });
}

export async function createNetworkAccessRequest(input: {
  title: string;
  justification: string;
  teamId: ID;
  groupId: ID | null;
  groupName: string;
  spec: NetworkSpec;
  evidenceRequestIds?: ID[];
}): Promise<NetworkAccessRequest> {
  return send<NetworkAccessRequest>("POST", "/access-requests", input);
}

/**
 * Approve or reject a request. The reviewer decides on the policy in front of
 * them — `edit` carries whatever they changed, and approving writes exactly
 * that into the chosen group.
 */
export async function decideNetworkAccessRequest(
  id: ID,
  status: "approved" | "rejected",
  edit?: {
    spec?: NetworkSpec;
    groupId?: ID | null;
    /** A group made for this approval, and who it applies to. */
    newGroup?: { name: string; baseline: boolean; teamIds: ID[] };
  },
): Promise<void> {
  await send("POST", `/access-requests/${encodeURIComponent(id)}/decision`, {
    status,
    spec: edit?.spec,
    groupId: edit?.groupId ?? undefined,
    newGroup: edit?.newGroup,
  });
}

/**
 * Say something on a request that has not been decided yet.
 *
 * `needsInfo` is the reviewer's half: it hands the request back to the person
 * who filed it and takes it out of the review queue. The requester's reply
 * needs no flag — answering puts it back in front of a reviewer on its own,
 * so neither side has to remember to move it.
 */
export async function commentOnNetworkAccessRequest(
  id: ID,
  body: string,
  opts?: { needsInfo?: boolean },
): Promise<NetworkAccessRequest> {
  return send<NetworkAccessRequest>("POST", `/access-requests/${encodeURIComponent(id)}/comments`, {
    body,
    needsInfo: opts?.needsInfo ?? false,
  });
}

/* ---------- proxy traffic ---------- */

export async function listNetworkRequests(q?: {
  ids?: ID[];
  userId?: ID;
  sessionId?: ID;
  status?: NetworkStatus | "needs-decision";
  search?: string;
  limit?: number;
}): Promise<NetworkRequest[]> {
  return get<NetworkRequest[]>(
    `/network-requests${query({
      ids: q?.ids?.join(","),
      userId: q?.userId,
      sessionId: q?.sessionId,
      status: q?.status,
      search: q?.search,
      limit: q?.limit,
    })}`,
  );
}

export async function getNetworkRequest(id: ID): Promise<NetworkRequest | undefined> {
  return api<NetworkRequest | undefined>(`/network-requests/${encodeURIComponent(id)}`, {
    allowStatuses: [404],
  });
}

/* ---------- models ---------- */

export async function listModels(): Promise<Model[]> {
  return get<Model[]>("/models");
}

/** What an administrator fills in about a model, adding one or editing one. */
export interface ModelInput {
  name: string;
  provider: string;
  hosting: Model["hosting"];
  endpoint: string;
  /** The provider's own identifier, e.g. `claude-opus-5`. */
  modelId?: string;
  contextWindow: number;
  costPerMTokIn: number;
  costPerMTokCachedIn: number;
  costPerMTokOut: number;
  /**
   * Held by the backend and never returned here. On an edit an empty string
   * means "leave the stored credential alone" — the console has nothing to
   * send back but a new one, so a blank field is not an erasure.
   */
  apiKey: string;
  extra?: Record<string, string>;
}

/** The console never sees a stored credential again, so the key goes in and
 *  only the model comes back. */
export async function createModel(input: ModelInput): Promise<Model> {
  return send<Model>("POST", "/models", input);
}

/**
 * A model john knows by name, from agentkit's catalog: what the console
 * offers for Anthropic, OpenAI and Google, with its limits and list prices.
 */
export interface CatalogModel {
  provider: string;
  id: string;
  name: string;
  contextWindow: number;
  maxOutput: number;
  costPerMTokIn: number;
  costPerMTokCachedIn: number;
  costPerMTokOut: number;
  reasoning: boolean;
  vision: boolean;
  documents: boolean;
}

/** Served before sign-in too: first-run setup lists models from it. */
export async function listModelCatalog(): Promise<CatalogModel[]> {
  return api<CatalogModel[]>("/models/catalog");
}

/** What a model said to one short prompt, asked before it is saved. */
export interface ModelTestResult {
  ok: boolean;
  reply?: string;
  /** The provider's refusal, or why it could not be reached. */
  error?: string;
  ms: number;
}

/**
 * Send a model, as the dialog has it, one short prompt. `id` is the model
 * being edited: with the key left blank, its stored key is used.
 */
export async function testModel(input: ModelInput, id?: ID): Promise<ModelTestResult> {
  // Not send(): nothing changes, so nothing needs refetching.
  return api<ModelTestResult>("/models/test", { method: "POST", body: JSON.stringify({ ...input, id }) });
}

/**
 * Rewrite a model's configuration. Its id, its enabled/disabled status and
 * when it was added stay as they were; everything else is what is sent, and
 * the backend rebuilds the config it actually calls with from it.
 */
export async function updateModel(id: ID, input: ModelInput): Promise<Model> {
  return send<Model>("PUT", `/models/${encodeURIComponent(id)}`, input);
}

/**
 * Remove a model outright. The backend refuses while a model-routing policy
 * still names it, because a policy pointing at a model that is gone can
 * narrow a session to nothing at all.
 */
export async function deleteModel(id: ID): Promise<void> {
  await send("DELETE", `/models/${encodeURIComponent(id)}`);
}

export async function setModelStatus(id: ID, status: Model["status"]): Promise<void> {
  await send("PUT", `/models/${encodeURIComponent(id)}/status`, { status });
}

/* ---------- usage ---------- */

interface UsageTotals {
  requests: number;
  tokensIn: number;
  tokensOut: number;
  tokensCached: number;
  sessions: number;
  allowed: number;
  approvals: number;
  blocked: number;
  costUsd: number;
}

export interface UsageSummary {
  days: UsageDay[];
  totals: UsageTotals;
  /** Same window, immediately before it — for the change indicators. */
  previous: UsageTotals;
  /** Fixed model order, so a model keeps its chart colour across filters. */
  byModel: Array<{ model: Model; requests: number; tokens: number; costUsd: number }>;
  byTeam: Array<{ team: Team; requests: number; sessions: number; costUsd: number }>;
  /** Heaviest users first — the org view's answer to "who is this?". */
  byUser: Array<{ user: User; requests: number; sessions: number; costUsd: number }>;
}

export async function getUsage(opts?: {
  days?: number;
  scope?: "org" | "me";
}): Promise<UsageSummary> {
  return get<UsageSummary>(`/usage${query({ days: opts?.days, scope: opts?.scope })}`);
}

/* ---------- session traces ----------
 *
 * The backend hands out sealed bytes; everything readable is made readable
 * here. The key lives in this module variable for the life of the tab and is
 * deliberately not put in localStorage: a trace read key that outlives the
 * session it was typed into is a key waiting to be found on a shared machine.
 */

let traceIdentity: string | null = null;

/* The decryption code is loaded the first time someone opens a trace, not on
   every page load: it carries a cryptography library that no other view
   needs, and most sessions in the console never unlock anything. */
const traceLib = () => import("../lib/trace");

export async function tracesUnlocked(): Promise<boolean> {
  return traceIdentity !== null;
}

/**
 * Hold the private read key for this tab. It is checked against a real
 * session where there is one, so a typo is caught here rather than looking
 * like an empty trace later.
 */
export async function unlockTraces(key: string): Promise<{ ok: boolean; error?: string }> {
  const { decryptTrace, TraceKeyError, looksLikeIdentity } = await traceLib();
  const candidate = key.trim();
  if (!looksLikeIdentity(candidate)) {
    return { ok: false, error: "That does not look like an age private key (AGE-SECRET-KEY-1…)." };
  }
  try {
    const sessions = await listTraceSessions();
    if (sessions.length > 0) {
      const file = await traceFile(sessions[0].id);
      if (file !== null) await decryptTrace(file, candidate);
    }
  } catch (err) {
    if (err instanceof TraceKeyError) {
      return { ok: false, error: "That key does not open this organization's traces." };
    }
    return { ok: false, error: err instanceof Error ? err.message : "Could not unlock." };
  }
  traceIdentity = candidate;
  bump();
  return { ok: true };
}

export async function lockTraces(): Promise<void> {
  traceIdentity = null;
  bump();
}

/** The sessions an administrator can open, newest first. */
export function listTraceSessions(): Promise<TraceSession[]> {
  return get<TraceSession[]>("/traces");
}

/** Registered public keys, for granting a session to someone. */
export function traceRecipients(): Promise<TraceRecipient[]> {
  return get<TraceRecipient[]>("/traces/recipients");
}

/** The sealed file, or null when no trace was recorded for that session. */
async function traceFile(sessionId: ID): Promise<string | null> {
  const res = await fetch(`/api/v1/traces/${encodeURIComponent(sessionId)}/file`, {
    credentials: "same-origin",
  });
  if (res.status === 404) return null;
  if (!res.ok) throw new ApiError(res.status, `${res.status} reading the trace`);
  return await res.text();
}

/**
 * Read one session: fetch it sealed, open it here, and render each record as
 * a line. Throws TraceKeyError when the held key does not open this session,
 * which is the case an administrator can fix with a grant.
 */
export async function readTrace(sessionId: ID): Promise<TraceEntry[]> {
  if (!traceIdentity) throw new Error("Traces are locked.");
  const { decryptTrace } = await traceLib();
  const file = await traceFile(sessionId);
  if (file === null) return [];
  const records = await decryptTrace(file, traceIdentity);
  return records.map((rec, i) => ({
    id: `${sessionId}:${rec.seq || i + 1}`,
    at: rec.at,
    sessionId: rec.session || sessionId,
    actorId: rec.actor,
    category: categoryOf(rec.kind),
    action: rec.kind,
    summary: summarize(rec.kind, rec.payload),
    payload: rec.payload,
    seq: rec.seq || i + 1,
  }));
}

/**
 * Give someone access to a session they cannot read. The key is unwrapped and
 * re-wrapped in this browser; the backend is handed a sealed copy it cannot
 * open, and only appends it to the file.
 */
export async function grantTrace(
  sessionId: ID,
  userId: ID,
  recipient: string,
): Promise<{ ok: boolean; error?: string }> {
  if (!traceIdentity) return { ok: false, error: "Traces are locked." };
  if (!recipient) {
    return { ok: false, error: "That person's client has not registered a key yet." };
  }
  try {
    const { regrant } = await traceLib();
    const file = await traceFile(sessionId);
    if (file === null) return { ok: false, error: "No trace was recorded for that session." };
    const wrapped = await regrant(file, traceIdentity, recipient);
    await send<{ ok: boolean }>("POST", `/traces/${encodeURIComponent(sessionId)}/grant`, {
      userId,
      wrapped,
    });
    return { ok: true };
  } catch (err) {
    return { ok: false, error: err instanceof Error ? err.message : "Could not grant access." };
  }
}

/** The record kinds the backend writes, grouped the way the page filters. */
function categoryOf(kind: string): TraceEntry["category"] {
  if (kind.startsWith("session.")) return "session";
  if (kind.startsWith("tool.")) return "tool";
  if (kind.startsWith("model.")) return "model";
  if (kind.startsWith("proxy.")) return "network";
  if (kind.startsWith("sandbox.")) return "workspace";
  if (kind === "prompt" || kind === "assistant.message" || kind === "turn.end") return "prompt";
  return "session";
}

/** One line for the table. Long payloads are cut; the drawer has all of it. */
function summarize(kind: string, payload: unknown): string {
  if (payload === undefined || payload === null) return "";
  const p = payload as Record<string, unknown>;
  const pick = (k: string) => (typeof p[k] === "string" ? (p[k] as string) : "");
  let text = "";
  switch (kind) {
    case "prompt":
    case "assistant.message":
      text = pick("text");
      break;
    case "tool.call":
      text = `${pick("tool")} ${JSON.stringify(p.args ?? "")}`;
      break;
    case "tool.result":
      text = `${pick("status")}${p.exit ? ` exit ${String(p.exit)}` : ""}`;
      break;
    case "proxy.decision":
      text = `${pick("status")} ${pick("method")} ${pick("url")}`.trim();
      break;
    default:
      text = JSON.stringify(payload);
  }
  text = text.replace(/\s+/g, " ").trim();
  return text.length > 160 ? text.slice(0, 160) + "…" : text;
}

/* ---------- org & setup ---------- */

const EMPTY_ORG: OrgSettings = {
  name: "",
  standalone: false,
  sso: { provider: "SSO", issuer: "", clientId: "", enforced: true, allowedDomains: [] },
  traceKey: { fingerprint: "", algorithm: "", createdAt: "", custodian: "" },
  traceStore: { location: "" },
  machineSessions: { maxDays: 0 },
  builds: { network: "direct" },
  prompts: { attachments: "guarded" },
  backend: { version: "", host: "", startedAt: "", dbSizeMb: 0, traceSizeMb: 0 },
};

export async function getOrg(): Promise<OrgSettings> {
  // Before sign-in (the /signin page) only the public sliver is available;
  // merge it over an empty shape so callers keep one type.
  const res = await fetch("/api/v1/org", { credentials: "same-origin" });
  if (res.ok) {
    return (await res.json()) as OrgSettings;
  }
  const pub = await fetch("/api/v1/org/public");
  if (!pub.ok) {
    if ((await pub.json().catch(() => null))?.error === "setup_required"
      && window.location.pathname !== "/setup") {
      window.location.href = "/setup";
    }
    throw new ApiError(pub.status, "organization unavailable");
  }
  const partial = (await pub.json()) as {
    name: string;
    standalone?: boolean;
    sso: { provider: string };
    backend: { version: string; host: string };
  };
  return {
    ...EMPTY_ORG,
    name: partial.name,
    standalone: partial.standalone ?? false,
    sso: { ...EMPTY_ORG.sso, provider: partial.sso.provider },
    backend: { ...EMPTY_ORG.backend, ...partial.backend },
  };
}

/** A change to some settings. The client secret is write-only, so it is only
 * ever in a patch, never in what the backend answers. */
export type OrgPatch = { [K in keyof OrgSettings]?: Partial<OrgSettings[K]> } & {
  sso?: Partial<OrgSettings["sso"]> & { clientSecret?: string };
};

/** Splits what an administrator typed into domains: commas, spaces or lines. */
export function parseDomains(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((d) => d.trim().replace(/^@/, "").toLowerCase())
    .filter(Boolean);
}

export async function updateOrg(patch: OrgPatch): Promise<void> {
  await send("PATCH", "/org", patch);
}

/* ---------- client downloads ---------- */

export interface Release {
  file: string; // john-darwin-arm64
  os: string; // darwin | linux | windows
  arch: string; // amd64 | arm64
  sizeBytes: number;
}

/** The client builds this backend has embedded and can serve, one per
 *  platform. Empty when the backend was built without them bundled. */
export async function listReleases(): Promise<Release[]> {
  return get<Release[]>("/releases");
}

/** A download token for the install snippets: a terminal has no session, so
 *  the link carries its own permission. It lasts minutes. */
export async function releaseLink(): Promise<{ token: string; expiresAt: string }> {
  return get("/releases/link");
}

/** The first-run wizard posts this, authenticated by the one-time token in
 *  the setup URL the backend printed. */
export async function completeSetup(payload: unknown): Promise<{ ok: true }> {
  const token = new URLSearchParams(window.location.search).get("token") ?? "";
  const res = await fetch("/api/v1/setup", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Setup-Token": token },
    body: JSON.stringify(payload),
  });
  if (!res.ok) {
    const body = await res.json().catch(() => null);
    throw new ApiError(res.status, body?.error ?? "setup failed");
  }
  return { ok: true };
}
