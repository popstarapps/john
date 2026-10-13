import { useState } from "react";
import { listChanges } from "../../api/client";
import { useQuery } from "../../lib/useQuery";
import { useDirectory } from "../../lib/directory";
import { Badge, Card, Empty, Loading, PageHead, Pager, SearchInput, Tag } from "../../components/ui";
import { Icon, type IconName } from "../../components/Icon";
import { dateTime, relative } from "../../lib/format";
import type { ChangeAction, ChangeEntity } from "../../api/types";

/* The record behind "who changed the controls, and when".
 *
 * It is deliberately not the traces. Traces are a session's contents, held
 * encrypted and read with a key kept outside the backend; this is the
 * configuration's history, and it has to be legible to any administrator
 * without unlocking anything — an auditor asking who loosened a rule should
 * not need the key that reads what developers wrote.
 *
 * Nothing here is editable, for the same reason the log is worth keeping.
 */

const ENTITIES: Array<ChangeEntity | "all"> = [
  "all",
  "policy",
  "group",
  "team",
  "member",
  "model",
  "request",
  "settings",
];

const ENTITY_ICON: Record<ChangeEntity, IconName> = {
  policy: "shield",
  group: "layers",
  team: "users",
  member: "user",
  model: "model",
  request: "inbox",
  settings: "settings",
  trace: "book",
};

/* Loosening and tightening do not look the same at a glance. Anything that
   creates or grants reach is neutral, anything that removes it reads as a
   restriction, and a rejection is the one red word on the page. */
function ActionBadge({ action }: { action: ChangeAction }) {
  switch (action) {
    case "approved":
      return <Badge tone="allow" icon="check">Approved</Badge>;
    case "rejected":
      return <Badge tone="deny" icon="ban">Rejected</Badge>;
    case "disabled":
    case "detached":
    case "deleted":
      return <Badge tone="warn">{action}</Badge>;
    default:
      return <Badge>{action}</Badge>;
  }
}

export default function History() {
  const dir = useDirectory();
  const [q, setQ] = useState("");
  const [entity, setEntity] = useState<ChangeEntity | "all">("all");
  const [limit, setLimit] = useState(50);

  const { data: changes, initial } = useQuery(
    () => listChanges({ search: q || undefined, entity: entity === "all" ? undefined : entity }),
    [q, entity],
  );

  return (
    <>
      <PageHead
        title="Change history"
        lede="Every configuration and policy change, in the order it happened. There is no configuration file to drift and nothing here can be amended — which is what makes “who loosened this, and when” answerable."
      />

      <Card pad={false}>
        <div className="toolbar">
          <SearchInput value={q} onChange={setQ} placeholder="Policy, group, team or model" />
          <select
            className="select"
            style={{ width: 150 }}
            value={entity}
            onChange={(e) => setEntity(e.target.value as ChangeEntity | "all")}
          >
            {ENTITIES.map((e) => (
              <option key={e} value={e}>
                {e === "all" ? "Everything" : e}
              </option>
            ))}
          </select>
        </div>

        {initial ? (
          <Loading rows={8} />
        ) : !changes || changes.length === 0 ? (
          <Empty icon="book" title="Nothing has changed yet" />
        ) : (
          <>
            <table className="tbl">
              <thead>
                <tr>
                  <th>When</th>
                  <th>Who</th>
                  <th>What</th>
                  <th>Change</th>
                  <th className="shrink">Version</th>
                </tr>
              </thead>
              <tbody>
                {changes.slice(0, limit).map((c) => (
                  <tr key={c.id}>
                    <td className="small muted nowrap" title={dateTime(c.at)}>
                      {relative(c.at)}
                    </td>
                    <td className="small">{dir.userName(c.actorId)}</td>
                    <td>
                      <span className="tag">
                        <Icon name={ENTITY_ICON[c.entity]} size={11} />
                        {c.entityName}
                      </span>
                    </td>
                    <td className="small">
                      <div className="row" style={{ gap: 8 }}>
                        <ActionBadge action={c.action} />
                        <span>{c.summary}</span>
                      </div>
                    </td>
                    <td className="shrink small muted">
                      {c.version === null ? "—" : <Tag>v{c.version}</Tag>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            <Pager
              shown={Math.min(limit, changes.length)}
              total={changes.length}
              onMore={() => setLimit((n) => n + 50)}
            />
          </>
        )}
      </Card>
    </>
  );
}
