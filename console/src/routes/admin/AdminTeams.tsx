import { useState } from "react";
import { Link } from "react-router-dom";
import { createTeam, listPolicyGroups, listTeams, listUsers } from "../../api/client";
import { useAction, useQuery } from "../../lib/useQuery";
import { Avatar, Card, Loading, PageHead, Tag } from "../../components/ui";
import { Modal, useToast } from "../../components/overlays";
import { Icon } from "../../components/Icon";
import { relative } from "../../lib/format";

export default function AdminTeams() {
  const [creating, setCreating] = useState(false);
  const { data: teams } = useQuery(() => listTeams(), []);
  const { data: users } = useQuery(() => listUsers(), []);
  const { data: groups } = useQuery(() => listPolicyGroups(), []);

  if (!teams || !users) return <Loading rows={6} height={50} />;

  return (
    <>
      <PageHead
        title="Teams"
        lede="Policy groups attach to teams, which is what makes a team mean something. Membership is managed here."
        actions={
          <button className="btn btn--primary" onClick={() => setCreating(true)}>
            <Icon name="plus" size={14} /> New team
          </button>
        }
      />

      <Card pad={false}>
        <div className="table-wrap">
          <table className="tbl">
            <thead>
              <tr>
                <th>Team</th>
                <th>Members</th>
                <th>Policy groups</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {teams.map((t) => {
                const attached = (groups ?? []).filter((g) => g.teamIds.includes(t.id));
                const members = users.filter((u) => t.memberIds.includes(u.id));
                return (
                  <tr key={t.id}>
                    <td>
                      <Link className="cell-main" to={`/admin/teams/${t.slug}`}>
                        {t.name}
                      </Link>
                      <div className="cell-sub truncate" style={{ maxWidth: 380 }}>
                        {t.description}
                      </div>
                    </td>
                    <td>
                      <span className="row" style={{ gap: 3 }}>
                        {members.slice(0, 5).map((m) => (
                          <Avatar key={m.id} name={m.name} size="sm" />
                        ))}
                        {members.length > 5 && (
                          <span className="small muted">+{members.length - 5}</span>
                        )}
                        {members.length === 0 && <span className="small muted">Nobody yet</span>}
                      </span>
                    </td>
                    <td>
                      <span className="row row-wrap" style={{ gap: 4 }}>
                        {attached.map((g) => (
                          <Tag key={g.id} baseline={g.baseline}>
                            {g.name}
                          </Tag>
                        ))}
                        {attached.length === 0 && (
                          <span className="small muted">None attached</span>
                        )}
                      </span>
                    </td>
                    <td className="small muted nowrap">{relative(t.createdAt)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Card>

      {creating && <NewTeamDialog onClose={() => setCreating(false)} />}
    </>
  );
}

const slugify = (v: string) =>
  v
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "");

function NewTeamDialog({ onClose }: { onClose: () => void }) {
  const toast = useToast();
  const { data: groups } = useQuery(() => listPolicyGroups(), []);
  const { data: teams } = useQuery(() => listTeams(), []);
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugTouched, setSlugTouched] = useState(false);
  const [description, setDescription] = useState("");
  const [groupIds, setGroupIds] = useState<string[]>([]);
  const [submitted, setSubmitted] = useState(false);
  const [create, saving] = useAction(createTeam);

  const effectiveSlug = slugTouched ? slug : slugify(name);
  const taken = (teams ?? []).some((t) => t.slug === effectiveSlug);
  const errors = {
    name: !name.trim() ? "A name is required." : null,
    slug: !effectiveSlug
      ? "A slug is required."
      : taken
        ? "Another team already uses that slug."
        : null,
  };
  const valid = Object.values(errors).every((e) => e === null);

  return (
    <Modal
      title="New team"
      sub="Attach policy groups now or later; members are added from the member's page."
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
              if (!valid) return;
              await create({
                name: name.trim(),
                slug: effectiveSlug,
                description: description.trim(),
                groupIds,
              });
              onClose();
              toast({
                tone: "allow",
                title: `${name.trim()} created`,
                body: "Baseline groups are attached already. Add members next.",
              });
            }}
          >
            {saving ? "Creating…" : "Create team"}
          </button>
        </>
      }
    >
      <div className="field-row">
        <label className="field">
          <span className="label">Name</span>
          <input
            className="input"
            value={name}
            placeholder="Payments"
            onChange={(e) => setName(e.target.value)}
          />
          {submitted && errors.name && <span className="err">{errors.name}</span>}
        </label>
        <label className="field">
          <span className="label">Slug</span>
          <input
            className="input input--mono"
            value={effectiveSlug}
            onChange={(e) => {
              setSlugTouched(true);
              setSlug(slugify(e.target.value));
            }}
          />
          {submitted && errors.slug ? (
            <span className="err">{errors.slug}</span>
          ) : (
            <span className="hint">Used in the team's URL.</span>
          )}
        </label>
      </div>

      <label className="field">
        <span className="label">Description</span>
        <textarea
          className="textarea"
          value={description}
          placeholder="What this team works on, in one sentence."
          onChange={(e) => setDescription(e.target.value)}
        />
      </label>

      <div className="field">
        <span className="label">Policy groups</span>
        <div className="col" style={{ gap: 8 }}>
          {(groups ?? [])
            .filter((g) => !g.baseline)
            .map((g) => (
              <label key={g.id} className={groupIds.includes(g.id) ? "check check--on" : "check"}>
                <input
                  type="checkbox"
                  checked={groupIds.includes(g.id)}
                  onChange={() =>
                    setGroupIds((cur) =>
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
        <span className="hint">
          Baseline groups are attached automatically and are not listed here.
        </span>
      </div>
    </Modal>
  );
}
