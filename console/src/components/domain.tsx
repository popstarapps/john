import type {
  NetworkStatus,
  Policy,
  PolicySpec,
  PolicyType,
  RequestStatus,
} from "../api/types";
import { Badge, KV, Row, Tag } from "./ui";
import { Icon, type IconName } from "./Icon";
import { bytes } from "../lib/format";

/* John's own vocabulary, rendered consistently. Every state is a word first
   and a colour second — the pills are legible with no colour at all. */

/* Unknown and denied are both blocked, and they are still two badges: the
   first is the absence of a policy and can become a network access request,
   the second is a policy that already considered this and said no. */
export function NetStatusBadge({ status }: { status: NetworkStatus }) {
  switch (status) {
    case "allowed":
      return <Badge tone="allow" icon="check">Allowed</Badge>;
    case "approved":
      return <Badge tone="allow" icon="check">Approved</Badge>;
    case "awaiting-approval":
      return <Badge tone="warn" icon="clock">Awaiting you</Badge>;
    case "rejected":
      return <Badge tone="deny" icon="ban">Rejected</Badge>;
    case "denied":
      return <Badge tone="deny" icon="ban">Denied</Badge>;
    case "unknown":
      return <Badge tone="deny" icon="ban">Unknown</Badge>;
  }
}

export function RequestStatusBadge({ status }: { status: RequestStatus }) {
  switch (status) {
    case "pending":
      return <Badge tone="warn" icon="clock">Pending</Badge>;
    case "approved":
      return <Badge tone="allow" icon="check">Approved</Badge>;
    case "rejected":
      return <Badge tone="deny" icon="ban">Rejected</Badge>;
    case "needs-info":
      return <Badge tone="info" icon="info">Needs info</Badge>;
  }
}

const TYPE_META: Record<PolicyType, { label: string; icon: IconName }> = {
  network: { label: "Network access", icon: "globe" },
  replacement: { label: "Replacement", icon: "shield" },
  "model-routing": { label: "Model routing", icon: "model" },
  workspace: { label: "Workspace protection", icon: "file" },
};

export function PolicyTypeTag({ type, plain }: { type: PolicyType; plain?: boolean }) {
  const meta = TYPE_META[type];
  if (plain) return <>{meta.label}</>;
  return (
    <span className="tag">
      <Icon name={meta.icon} size={11} />
      {meta.label}
    </span>
  );
}

export const policyTypeLabel = (t: PolicyType) => TYPE_META[t].label;
export const policyTypeIcon = (t: PolicyType) => TYPE_META[t].icon;

export function DecisionBadge({ decision }: { decision: "always-allow" | "ask-developer" | "deny" }) {
  if (decision === "always-allow") return <Badge tone="allow" icon="check">Always allow</Badge>;
  if (decision === "ask-developer") return <Badge tone="warn" icon="clock">Ask developer</Badge>;
  return <Badge tone="deny" icon="ban">Deny</Badge>;
}

export function MethodChip({ method }: { method: string }) {
  const write = !["GET", "HEAD", "OPTIONS"].includes(method.toUpperCase());
  return <span className={write ? "method method--write" : "method"}>{method}</span>;
}

/** One line that says what a policy matches — the form used in tables. */
export function SpecSummary({ spec }: { spec: PolicySpec }) {
  switch (spec.kind) {
    case "network":
      return (
        <span className="mono small">
          {spec.methods.join(" ")} {spec.scheme === "any" ? "*" : spec.scheme}://{spec.host}
          {spec.path}
        </span>
      );
    case "replacement":
      return (
        <span className="mono small">
          /{spec.pattern}/ → {spec.replacement}
        </span>
      );
    case "model-routing":
      return (
        <span className="small">
          {spec.mode === "assign" ? "Assign" : "Restrict to"} {spec.modelIds.length} model
          {spec.modelIds.length === 1 ? "" : "s"}
          {spec.keywords.length > 0 &&
            ` · on ${spec.keywords.length} keyword${spec.keywords.length === 1 ? "" : "s"}`}
        </span>
      );
    case "workspace":
      return (
        <span className="mono small">
          {spec.mode} · {spec.paths.slice(0, 2).join(", ")}
          {spec.paths.length > 2 && ` +${spec.paths.length - 2}`}
        </span>
      );
  }
}

/** The full matcher, for a detail view. */
export function SpecDetail({
  spec,
  modelName,
}: {
  spec: PolicySpec;
  modelName?: (id: string) => string;
}) {
  switch (spec.kind) {
    case "network":
      return (
        <KV>
          <Row k="Methods">
            <span className="row row-wrap" style={{ gap: 4 }}>
              {spec.methods.map((m) => (
                <MethodChip key={m} method={m} />
              ))}
            </span>
          </Row>
          <Row k="Scheme">{spec.scheme}</Row>
          <Row k="Host">
            <code>{spec.host}</code>
          </Row>
          <Row k="Port">{spec.port ?? "any"}</Row>
          <Row k="Path">
            <code>{spec.path}</code>
          </Row>
          <Row k="Decision">
            <DecisionBadge decision={spec.decision} />
          </Row>
          {spec.headerMatches.length > 0 && (
            <Row k="Headers">
              <div className="col" style={{ gap: 2 }}>
                {spec.headerMatches.map((m) => (
                  <code key={m.name}>
                    {m.name}: {m.value}
                  </code>
                ))}
              </div>
            </Row>
          )}
          {spec.queryMatches.length > 0 && (
            <Row k="Query">
              <div className="col" style={{ gap: 2 }}>
                {spec.queryMatches.map((m) => (
                  <code key={m.name}>
                    {m.name}={m.value}
                  </code>
                ))}
              </div>
            </Row>
          )}
          <Row k="Max body">
            {spec.maxBodyBytes === null
              ? "unrestricted"
              : spec.maxBodyBytes === 0
                ? "no body permitted"
                : bytes(spec.maxBodyBytes)}
          </Row>
        </KV>
      );
    case "replacement":
      return (
        <KV>
          <Row k="Pattern">
            <code>{spec.pattern}</code>
          </Row>
          <Row k="Replacement">
            <code>{spec.replacement}</code>
            {spec.replacement.includes("#") && (
              <div className="small muted">
                <code>#</code> becomes a stable alias per distinct value.
              </div>
            )}
          </Row>
          <Row k="Applies to">Everything on its way to a model</Row>
          {spec.example && (
            <Row k="Example">
              <span className="mono small">
                <span className="muted">{spec.example.before}</span> → {spec.example.after}
              </span>
            </Row>
          )}
        </KV>
      );
    case "model-routing":
      return (
        <KV>
          <Row k="Mode">
            {spec.mode === "assign" ? "Assign models to the team" : "Restrict the session to"}
          </Row>
          <Row k="Models">
            <span className="row row-wrap" style={{ gap: 4 }}>
              {spec.modelIds.map((id) => (
                <Tag key={id}>{modelName ? modelName(id) : id}</Tag>
              ))}
            </span>
          </Row>
          <Row k="Triggered by">
            {spec.keywords.length === 0 ? (
              "Every session"
            ) : (
              <>
                <span className="row row-wrap" style={{ gap: 4 }}>
                  {spec.keywords.map((k) => (
                    <Tag key={k}>{k}</Tag>
                  ))}
                </span>
                <div className="small muted" style={{ marginTop: 6 }}>
                  Seen anywhere in the session — prompt, file, command output or diff. The developer
                  is then asked which of the remaining models to continue on.
                </div>
              </>
            )}
          </Row>
        </KV>
      );
    case "workspace":
      return (
        <KV>
          <Row k="Mode">
            {spec.mode === "exclude"
              ? "Excluded from the sandbox"
              : spec.mode === "read-only"
                ? "Mounted read-only"
                : "Deletion prevented"}
          </Row>
          <Row k="Paths">
            <div className="col" style={{ gap: 2 }}>
              {spec.paths.map((p) => (
                <code key={p}>{p}</code>
              ))}
            </div>
          </Row>
        </KV>
      );
  }
}

export function PolicyLine({ policy, groupName }: { policy: Policy; groupName?: string }) {
  return (
    <div className="col" style={{ gap: 2 }}>
      <div className="row" style={{ gap: 8 }}>
        <span className="cell-main">{policy.name}</span>
        {!policy.enabled && <Badge>Disabled</Badge>}
      </div>
      <SpecSummary spec={policy.spec} />
      {groupName && <span className="small muted">{groupName}</span>}
    </div>
  );
}
