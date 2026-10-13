import type { ReactNode } from "react";
import { Badge, Card, Tag } from "./ui";
import { DecisionBadge, MethodChip } from "./domain";
import type {
  ModelRoutingSpec,
  NetworkSpec,
  Policy,
  PolicyGroup,
  ReplacementSpec,
  WorkspaceSpec,
} from "../api/types";

/* The effective policy set, as tables.
 *
 * One rendering, used by the developer's own policy page and by a team's.
 * Every section is four columns wide and the last two — the state of the rule
 * and the group it came from — hold the same width everywhere, so the tables
 * line up down the page instead of each finding its own grid.
 *
 * Names of things defined elsewhere (a group, a policy, a model, a keyword)
 * are always in a box. Prose is prose; a reference is a tag. */

export type PolicyWithGroup = Policy & { group: PolicyGroup };

const WORKSPACE_MODE: Record<WorkspaceSpec["mode"], { label: string; tone: "deny" | "warn" }> = {
  exclude: { label: "Not mounted", tone: "deny" },
  "read-only": { label: "Read-only", tone: "warn" },
  "no-delete": { label: "Cannot delete", tone: "warn" },
};

function Section({
  title,
  sub,
  head,
  children,
  foot,
}: {
  title: string;
  sub: ReactNode;
  head: [string, string, string];
  children: ReactNode;
  foot?: ReactNode;
}) {
  return (
    <Card title={title} sub={sub} pad={false}>
      <div className="table-wrap">
        <table className="tbl">
          <thead>
            <tr>
              <th>{head[0]}</th>
              <th>{head[1]}</th>
              <th className="col-state">{head[2]}</th>
              <th className="col-group">From group</th>
            </tr>
          </thead>
          <tbody>{children}</tbody>
        </table>
      </div>
      {foot && <div className="pager">{foot}</div>}
    </Card>
  );
}

const GroupCell = ({ group }: { group: PolicyGroup }) => (
  <td className="col-group">
    <Tag>{group.name}</Tag>
  </td>
);

export function EffectivePolicies({
  policies,
  modelName,
  requestLink,
}: {
  policies: PolicyWithGroup[];
  modelName?: (id: string) => string;
  /** Shown under the network table, where "not listed" means "blocked". */
  requestLink?: ReactNode;
}) {
  const of = (type: Policy["type"]) => policies.filter((p) => p.type === type && p.enabled);

  const network = of("network").sort((a, b) =>
    (a.spec as NetworkSpec).host.localeCompare((b.spec as NetworkSpec).host),
  );
  // One row per protected path, so the table reads like the network one.
  const workspace = of("workspace").flatMap((p) =>
    (p.spec as WorkspaceSpec).paths.map((path) => ({ path, policy: p })),
  );
  const replacements = of("replacement");
  const routing = of("model-routing");

  return (
    <div className="stack">
      <Section
        title="Network access"
        sub={`${network.length} policies decide what the sandbox may reach`}
        head={["Methods", "Destination", "Decision"]}
        foot={requestLink}
      >
        {network.map((p) => {
          const s = p.spec as NetworkSpec;
          return (
            <tr key={p.id}>
              <td className="shrink">
                <span className="row" style={{ gap: 3 }}>
                  {s.methods.map((m) => (
                    <MethodChip key={m} method={m} />
                  ))}
                </span>
              </td>
              <td className="mono small">
                {s.scheme === "any" ? "*" : s.scheme}://{s.host}
                {s.path}
                {(s.headerMatches.length > 0 || s.queryMatches.length > 0) && (
                  <div className="cell-sub">
                    {[
                      s.headerMatches.length > 0
                        ? `${s.headerMatches.length} header ${s.headerMatches.length === 1 ? "match" : "matches"}`
                        : null,
                      s.queryMatches.length > 0
                        ? `${s.queryMatches.length} query ${s.queryMatches.length === 1 ? "match" : "matches"}`
                        : null,
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </div>
                )}
              </td>
              <td className="col-state">
                <DecisionBadge decision={s.decision} />
              </td>
              <GroupCell group={p.group} />
            </tr>
          );
        })}
      </Section>

      {workspace.length > 0 && (
        <Section
          title="Workspace protection"
          sub={`${workspace.length} paths the agent cannot see or cannot change`}
          head={["Path", "Policy", "Treatment"]}
        >
          {workspace.map((row) => (
            <tr key={`${row.policy.id}-${row.path}`}>
              <td className="mono small">{row.path}</td>
              <td>
                <Tag>{row.policy.name}</Tag>
              </td>
              <td className="col-state">
                <Badge tone={WORKSPACE_MODE[(row.policy.spec as WorkspaceSpec).mode].tone}>
                  {WORKSPACE_MODE[(row.policy.spec as WorkspaceSpec).mode].label}
                </Badge>
              </td>
              <GroupCell group={row.policy.group} />
            </tr>
          ))}
        </Section>
      )}

      {replacements.length > 0 && (
        <Section
          title="Replacements"
          sub={`${replacements.length} patterns rewritten before anything reaches a model`}
          head={["Pattern", "Becomes", "Policy"]}
        >
          {replacements.map((p) => {
            const s = p.spec as ReplacementSpec;
            return (
              <tr key={p.id}>
                <td className="mono small">{s.pattern}</td>
                <td className="mono small">{s.replacement}</td>
                <td className="col-state">
                  <Tag>{p.name}</Tag>
                </td>
                <GroupCell group={p.group} />
              </tr>
            );
          })}
        </Section>
      )}

      {routing.length > 0 && (
        <Section
          title="Model routing"
          sub="Which models are available, and what narrows them mid-session"
          head={["Triggered by", "Models", "Mode"]}
        >
          {routing.map((p) => {
            const s = p.spec as ModelRoutingSpec;
            return (
              <tr key={p.id}>
                <td>
                  {s.keywords.length === 0 ? (
                    <span className="small muted">Every session</span>
                  ) : (
                    <span className="row row-wrap" style={{ gap: 4 }}>
                      {s.keywords.map((k) => (
                        <Tag key={k}>{k}</Tag>
                      ))}
                    </span>
                  )}
                </td>
                <td>
                  <span className="row row-wrap" style={{ gap: 4 }}>
                    {s.modelIds.map((id) => (
                      <Tag key={id}>{modelName ? modelName(id) : id}</Tag>
                    ))}
                  </span>
                </td>
                <td className="col-state">
                  <Badge tone={s.mode === "restrict" ? "warn" : "allow"}>
                    {s.mode === "restrict" ? "Restrict to" : "Assign"}
                  </Badge>
                </td>
                <GroupCell group={p.group} />
              </tr>
            );
          })}
        </Section>
      )}
    </div>
  );
}
