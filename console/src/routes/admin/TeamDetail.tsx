import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  deleteTeam,
  getTeam,
  getViewer,
  listPolicies,
  listPolicyGroups,
  listUsers,
  setTeamGroups,
  setUserTeams,
} from "../../api/client";
import { useAction, useQuery } from "../../lib/useQuery";
import { useDirectory } from "../../lib/directory";
import { Badge, Card, Empty, Loading, PageHead, Person, Tag } from "../../components/ui";
import { ConfirmDialog, Modal, useToast } from "../../components/overlays";
import { EffectivePolicies } from "../../components/policies";
import { Icon } from "../../components/Icon";
import { relative } from "../../lib/format";

export default function TeamDetail() {
  const { slug = "" } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const dir = useDirectory();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState<string[]>([]);
  const [deleting, setDeleting] = useState(false);

  const { data: viewer } = useQuery(() => getViewer(), []);
  const { data: team, initial } = useQuery(() => getTeam(slug), [slug]);
  const { data: users } = useQuery(() => listUsers(), []);
  const { data: groups } = useQuery(() => listPolicyGroups(), []);
  const { data: policies } = useQuery(() => listPolicies(), []);
  const [save, saving] = useAction(setTeamGroups);
  const [saveTeams, savingTeams] = useAction(setUserTeams);
  const [removeTeam, removingTeam] = useAction(deleteTeam);

  if (initial) return <Loading rows={6} height={40} />;
  if (!team)
    return (
      <Card>
        <Empty icon="users" title="Team not found">
          <Link to="/admin/teams">Back to teams</Link>
        </Empty>
      </Card>
    );

  const members = (users ?? []).filter((u) => team.memberIds.includes(u.id));
  const attached = (groups ?? []).filter((g) => g.teamIds.includes(team.id));
  // The same shape the developer's policy page reads, so it renders the same.
  const attachedPolicies = (policies ?? []).flatMap((p) => {
    const group = attached.find((g) => g.id === p.groupId);
    return group ? [{ ...p, group }] : [];
  });
  const canManage = viewer?.isAdmin ?? false;
  const outsiders = (users ?? []).filter((u) => !team.memberIds.includes(u.id));
  // Baseline groups are attached to every team automatically and keep applying
  // to everyone afterwards, so they are not what a delete takes away.
  const carried = attached.filter((g) => !g.baseline).length;

  // Membership is held here rather than mapped from a claim, so it is edited
  // from either end: a person's page, or the team they belong to.
  const setMembership = async (userId: string, teamIds: string[], message: string) => {
    await saveTeams(userId, teamIds);
    toast({
      tone: "allow",
      title: message,
      body: "Effective access updates on the next policy fetch.",
    });
  };

  return (
    <>
      <PageHead
        crumbs={[
          { label: "Teams", to: "/admin/teams" },
          { label: team.name },
        ]}
        title={team.name}
        lede={team.description}
        meta={<Tag>{team.slug}</Tag>}
        actions={
          canManage ? (
            <>
              <button
                className="btn"
                onClick={() => {
                  setDraft(attached.filter((g) => !g.baseline).map((g) => g.id));
                  setEditing(true);
                }}
              >
                <Icon name="layers" size={14} /> Attach policy groups
              </button>
              <button className="btn btn--danger" onClick={() => setDeleting(true)}>
                <Icon name="trash" size={14} /> Delete team
              </button>
            </>
          ) : undefined
        }
      />

      <div className="grid grid-side">
        <div className="stack">
          <Card
            title="Members"
            sub={`${members.length} ${members.length === 1 ? "person" : "people"}`}
            pad={false}
            foot={
              canManage && outsiders.length > 0 ? (
                <label className="field" style={{ marginBottom: 0 }}>
                  <span className="label">Add somebody</span>
                  <select
                    className="select"
                    value=""
                    disabled={savingTeams}
                    onChange={(e) => {
                      const person = outsiders.find((u) => u.id === e.target.value);
                      if (person) {
                        void setMembership(
                          person.id,
                          [...person.teamIds, team.id],
                          `${person.name} added to ${team.name}`,
                        );
                      }
                    }}
                  >
                    <option value="">Choose a person…</option>
                    {outsiders.map((u) => (
                      <option key={u.id} value={u.id}>
                        {u.name} · {u.email}
                      </option>
                    ))}
                  </select>
                </label>
              ) : undefined
            }
          >
            <div className="table-wrap">
              <table className="tbl">
                <thead>
                  <tr>
                    <th>Person</th>
                    <th>Title</th>
                    <th>Org role</th>
                    <th>Last active</th>
                    {canManage && <th className="shrink"></th>}
                  </tr>
                </thead>
                <tbody>
                  {members.map((m) => (
                    <tr key={m.id}>
                      <td>
                        <Person
                          name={m.name}
                          sub={m.email}
                          to={`/admin/members/${m.id}`}
                        />
                      </td>
                      <td className="small">{m.title}</td>
                      <td>
                        {m.role === "admin" ? (
                          <Badge tone="warn">{m.role}</Badge>
                        ) : (
                          <Badge>member</Badge>
                        )}
                      </td>
                      <td className="small muted nowrap">{relative(m.lastActiveAt)}</td>
                      {canManage && (
                        <td className="shrink">
                          <button
                            className="btn btn--sm"
                            disabled={savingTeams}
                            onClick={() =>
                              setMembership(
                                m.id,
                                m.teamIds.filter((t) => t !== team.id),
                                `${m.name} removed from ${team.name}`,
                              )
                            }
                          >
                            <Icon name="x" size={13} /> Remove
                          </button>
                        </td>
                      )}
                    </tr>
                  ))}
                  {members.length === 0 && (
                    <tr>
                      <td colSpan={canManage ? 5 : 4} className="small muted">
                        Nobody is in this team, so its groups apply to nobody.
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          </Card>

          <div>
            <p className="eyebrow" style={{ margin: "0 0 10px" }}>
              Effective policies — {attachedPolicies.length} from {attached.length} groups
            </p>
            <EffectivePolicies policies={attachedPolicies} modelName={(id) => dir.modelName(id)} />
          </div>
        </div>

        <div className="stack">
          <Card title="Attached groups">
            <div className="row row-wrap" style={{ gap: 5 }}>
              {attached.map((g) => (
                <Link key={g.id} to={`/admin/groups/${g.id}`}>
                  <Tag baseline={g.baseline}>{g.name}</Tag>
                </Link>
              ))}
            </div>
            <p className="small muted" style={{ marginTop: 12 }}>
              Baseline groups are attached automatically and cannot be removed or weakened by any
              other group.
            </p>
          </Card>
        </div>
      </div>

      {deleting && (
        <ConfirmDialog
          title={`Delete ${team.name}?`}
          danger
          busy={removingTeam}
          confirmLabel="Delete team"
          body={
            <>
              <p>
                {members.length === 0
                  ? "Nobody is in it, so nobody's access changes."
                  : carried === 0
                    ? `${members.length} ${members.length === 1 ? "person leaves" : "people leave"} it, but it carries no groups of its own, so their access does not change.`
                    : `${members.length} ${members.length === 1 ? "person" : "people"} stop carrying the ${carried} group${carried === 1 ? "" : "s"} attached to it.`}
              </p>
              <p className="small muted">
                That cuts both ways: rules those groups held stop allowing network access, and stop
                protecting files. Baseline groups apply to everyone regardless of team, so they are
                unaffected.
              </p>
            </>
          }
          onClose={() => setDeleting(false)}
          onConfirm={async () => {
            await removeTeam(team.id);
            setDeleting(false);
            toast({ tone: "warn", title: `${team.name} deleted` });
            navigate("/admin/teams");
          }}
        />
      )}

      {editing && (
        <Modal
          title={`Policy groups for ${team.name}`}
          sub="Baseline groups are always attached and are not listed here."
          onClose={() => setEditing(false)}
          footer={
            <>
              <button className="btn" onClick={() => setEditing(false)}>
                Cancel
              </button>
              <button
                className="btn btn--primary"
                disabled={saving}
                onClick={async () => {
                  await save(team.id, draft);
                  setEditing(false);
                  toast({ tone: "allow", title: "Groups updated", body: `${team.name} · change is versioned and recorded.` });
                }}
              >
                {saving ? "Saving…" : "Save"}
              </button>
            </>
          }
        >
          <div className="col" style={{ gap: 8 }}>
            {(groups ?? [])
              .filter((g) => !g.baseline)
              .map((g) => (
                <label key={g.id} className={draft.includes(g.id) ? "check check--on" : "check"}>
                  <input
                    type="checkbox"
                    checked={draft.includes(g.id)}
                    onChange={() =>
                      setDraft((cur) =>
                        cur.includes(g.id) ? cur.filter((x) => x !== g.id) : [...cur, g.id],
                      )
                    }
                  />
                  <span>
                    {g.name}
                    <span className="hint">{g.description}</span>
                  </span>
                </label>
              ))}
          </div>
        </Modal>
      )}
    </>
  );
}
