import { useState } from "react";
import { Link } from "react-router-dom";
import { createPolicyGroup, listPolicies, listPolicyGroups } from "../../api/client";
import { useAction, useQuery } from "../../lib/useQuery";
import { Card, Empty, Loading, PageHead, SearchInput, Tag } from "../../components/ui";
import { Modal, useToast } from "../../components/overlays";
import { Icon } from "../../components/Icon";
import { policyTypeLabel } from "../../components/domain";
import { relative } from "../../lib/format";
import type { PolicyType } from "../../api/types";

export default function Groups() {
  const [q, setQ] = useState("");
  const [creating, setCreating] = useState(false);
  const { data: groups } = useQuery(() => listPolicyGroups(), []);
  const { data: policies } = useQuery(() => listPolicies(), []);

  if (!groups || !policies) return <Loading rows={6} height={44} />;

  // Baseline first — the client sorts them that way, and it is the order an
  // administrator reads the org in: the floor, then everything above it.
  const rows = groups.filter((g) =>
    (g.name + g.description).toLowerCase().includes(q.toLowerCase()),
  );

  return (
    <>
      <PageHead
        title="Policy groups"
        lede="A group bundles related policies so a control is defined once and applied wherever it is needed. Baseline groups attach to every team and cannot be weakened."
        actions={
          <button className="btn btn--primary" onClick={() => setCreating(true)}>
            <Icon name="plus" size={14} /> New group
          </button>
        }
      />

      <Card pad={false}>
        <div className="toolbar">
          <SearchInput value={q} onChange={setQ} placeholder="Search groups" />
        </div>

        {rows.length === 0 ? (
          <Empty icon="layers" title="No groups match" />
        ) : (
          <div className="table-wrap">
            <table className="tbl">
              <thead>
                <tr>
                  <th>Group</th>
                  <th>Contains</th>
                  <th className="right">Policies</th>
                  <th>Attached to</th>
                  <th>Updated</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((g) => {
                  const own = policies.filter((p) => p.groupId === g.id);
                  const types = [...new Set(own.map((p) => p.type))] as PolicyType[];
                  return (
                    <tr key={g.id}>
                      <td>
                        <span className="row" style={{ gap: 8 }}>
                          <Link className="cell-main" to={`/admin/groups/${g.id}`}>
                            {g.name}
                          </Link>
                          {g.baseline && <Tag baseline>baseline</Tag>}
                        </span>
                        <div className="cell-sub truncate" style={{ maxWidth: 420 }}>
                          {g.description}
                        </div>
                      </td>
                      <td>
                        <span className="row row-wrap" style={{ gap: 4 }}>
                          {types.map((t) => (
                            <Tag key={t}>{policyTypeLabel(t)}</Tag>
                          ))}
                          {types.length === 0 && <span className="small muted">Nothing yet</span>}
                        </span>
                      </td>
                      <td className="right num">{own.length}</td>
                      <td className="small">
                        {g.baseline
                          ? "Every team"
                          : g.teamIds.length === 0
                            ? "No team — no effect"
                            : `${g.teamIds.length} team${g.teamIds.length === 1 ? "" : "s"}`}
                      </td>
                      <td className="small muted nowrap">
                        {relative(g.updatedAt)}
                        <div className="cell-sub">v{g.version}</div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {creating && <NewGroupDialog onClose={() => setCreating(false)} />}
    </>
  );
}

function NewGroupDialog({ onClose }: { onClose: () => void }) {
  const toast = useToast();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [baseline, setBaseline] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const [create, saving] = useAction(createPolicyGroup);

  return (
    <Modal
      title="New policy group"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            className="btn btn--primary"
            disabled={saving}
            onClick={async () => {
              setSubmitted(true);
              if (!name.trim()) return;
              await create({ name: name.trim(), description: description.trim(), baseline });
              onClose();
              toast({
                tone: "allow",
                title: `${name.trim()} created`,
                body: baseline
                  ? "Attached to every team. Add policies to it next."
                  : "Attach it to a team, then add policies to it.",
              });
            }}
          >
            {saving ? "Creating…" : "Create group"}
          </button>
        </>
      }
    >
      <label className="field">
        <span className="label">Name</span>
        <input
          className="input"
          value={name}
          placeholder="Internal APIs — write"
          onChange={(e) => setName(e.target.value)}
        />
        {submitted && !name.trim() && <span className="err">A name is required.</span>}
      </label>
      <label className="field">
        <span className="label">Description</span>
        <textarea
          className="textarea"
          value={description}
          placeholder="What this group is for, in one sentence."
          onChange={(e) => setDescription(e.target.value)}
        />
      </label>
      <label className={baseline ? "check check--on" : "check"}>
        <input type="checkbox" checked={baseline} onChange={(e) => setBaseline(e.target.checked)} />
        <span>
          Baseline
          <span className="hint">
            Attached to all teams automatically and cannot be detached.
          </span>
        </span>
      </label>
    </Modal>
  );
}
