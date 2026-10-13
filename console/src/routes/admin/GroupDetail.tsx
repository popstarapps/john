import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { getPolicyGroup, listPolicies, listTeams, setPolicyEnabled } from "../../api/client";
import { useAction, useQuery } from "../../lib/useQuery";
import { useDirectory } from "../../lib/directory";
import { Badge, Card, Empty, KV, Loading, PageHead, Row, Tag } from "../../components/ui";
import { Drawer, useToast } from "../../components/overlays";
import { AddPolicyDialog } from "../../components/PolicyEditor";
import { PolicyTypeTag, SpecDetail, SpecSummary } from "../../components/domain";
import { Icon } from "../../components/Icon";
import { dateTime, relative } from "../../lib/format";
import type { Policy } from "../../api/types";

export default function GroupDetail() {
  const { id = "" } = useParams();
  const toast = useToast();
  const dir = useDirectory();
  const [open, setOpen] = useState<Policy | null>(null);
  const [adding, setAdding] = useState(false);

  const { data: group, initial } = useQuery(() => getPolicyGroup(id), [id]);
  const { data: policies } = useQuery(() => listPolicies({ groupId: id }), [id]);
  const { data: teams } = useQuery(() => listTeams(), []);
  const [toggle, toggling] = useAction(setPolicyEnabled);

  if (initial) return <Loading rows={6} height={40} />;
  if (!group)
    return (
      <Card>
        <Empty icon="layers" title="Group not found">
          <Link to="/admin/groups">Back to policy groups</Link>
        </Empty>
      </Card>
    );

  const attachedTeams = (teams ?? []).filter((t) => group.teamIds.includes(t.id));

  return (
    <>
      <PageHead
        crumbs={[{ label: "Policy groups", to: "/admin/groups" }, { label: group.name }]}
        title={group.name}
        lede={group.description}
        meta={
          <>
            {group.baseline && <Tag baseline>baseline</Tag>}
            <Badge>v{group.version}</Badge>
          </>
        }
        actions={
          <button className="btn btn--primary" onClick={() => setAdding(true)}>
            <Icon name="plus" size={14} /> Add policy
          </button>
        }
      />

      <div className="stack">
        <Card title="Attached to">
          {group.baseline ? (
            <p className="small muted">
              Baseline. Attached to all teams automatically and cannot be detached. No other group
              can weaken it.
            </p>
          ) : attachedTeams.length === 0 ? (
            <p className="small muted">Not attached to any team — it has no effect yet.</p>
          ) : (
            <div className="row row-wrap" style={{ gap: 5 }}>
              {attachedTeams.map((t) => (
                <Link key={t.id} to={`/admin/teams/${t.slug}`}>
                  <Tag>
                    {t.name} · {t.memberIds.length}
                  </Tag>
                </Link>
              ))}
            </div>
          )}
        </Card>

        <Card title="Policies" sub={`${policies?.length ?? 0} in this group`} pad={false}>
          {!policies ? (
            <Loading rows={5} />
          ) : policies.length === 0 ? (
            <Empty icon="shield" title="No policies in this group yet" />
          ) : (
            <div className="table-wrap">
              <table className="tbl">
                <thead>
                  <tr>
                    <th>Policy</th>
                    <th>Type</th>
                    <th>Matches</th>
                    <th>Updated</th>
                    <th>Enabled</th>
                  </tr>
                </thead>
                <tbody>
                  {policies.map((p) => (
                    <tr key={p.id}>
                      <td>
                        <button
                          className="cell-main"
                          style={{
                            background: "none",
                            border: 0,
                            padding: 0,
                            cursor: "pointer",
                            textAlign: "left",
                          }}
                          onClick={() => setOpen(p)}
                        >
                          {p.name}
                        </button>
                        <div className="cell-sub truncate" style={{ maxWidth: 400 }}>
                          {p.description}
                        </div>
                      </td>
                      <td>
                        <PolicyTypeTag type={p.type} />
                      </td>
                      <td>
                        <SpecSummary spec={p.spec} />
                      </td>
                      <td className="small muted nowrap">
                        {relative(p.updatedAt)}
                        <div className="cell-sub">v{p.version}</div>
                      </td>
                      <td className="shrink">
                        <label className="switch">
                          <input
                            type="checkbox"
                            checked={p.enabled}
                            disabled={toggling}
                            onChange={async () => {
                              await toggle(p.id, !p.enabled);
                              toast({
                                tone: p.enabled ? "warn" : "allow",
                                title: `${p.name} ${p.enabled ? "disabled" : "enabled"}`,
                                body: "Clients pick this up within the policy cache TTL.",
                              });
                            }}
                          />
                        </label>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      </div>

      {adding && (
        <AddPolicyDialog groupId={group.id} groupName={group.name} onClose={() => setAdding(false)} />
      )}

      {open && (
        <Drawer
          title={open.name}
          sub={<PolicyTypeTag type={open.type} plain />}
          onClose={() => setOpen(null)}
          footer={
            <button className="btn" onClick={() => setOpen(null)}>
              Close
            </button>
          }
        >
          <p className="lede" style={{ marginBottom: 16 }}>
            {open.description}
          </p>
          <SpecDetail spec={open.spec} modelName={(mid) => dir.modelName(mid)} />
          <div style={{ marginTop: 16 }}>
            <KV>
              <Row k="Group">{group.name}</Row>
              <Row k="Version">v{open.version}</Row>
              <Row k="Updated">{dateTime(open.updatedAt)}</Row>
              <Row k="Updated by">{dir.userName(open.updatedBy)}</Row>
              <Row k="Status">
                {open.enabled ? <Badge tone="allow">Enabled</Badge> : <Badge>Disabled</Badge>}
              </Row>
            </KV>
          </div>
        </Drawer>
      )}
    </>
  );
}
