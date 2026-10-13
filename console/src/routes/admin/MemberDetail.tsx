import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import {
  getUser,
  getViewer,
  groupsForUser,
  listTeams,
  modelsForUser,
  setUserRole,
  setUserStatus,
  setUserTeams,
} from "../../api/client";
import { useAction, useQuery } from "../../lib/useQuery";
import { Avatar, Badge, Card, Empty, KV, Loading, PageHead, Row, Segmented, Tag } from "../../components/ui";
import { ConfirmDialog, useToast } from "../../components/overlays";
import { Icon } from "../../components/Icon";
import { date, relative } from "../../lib/format";

export default function MemberDetail() {
  const { id = "" } = useParams();
  const toast = useToast();
  const [confirmingSuspend, setConfirmingSuspend] = useState(false);

  const { data: viewer } = useQuery(() => getViewer(), []);
  const { data: user, initial } = useQuery(() => getUser(id), [id]);
  const { data: teams } = useQuery(() => listTeams(), []);
  const { data: groups } = useQuery(() => groupsForUser(id), [id]);
  const { data: models } = useQuery(() => modelsForUser(id), [id]);
  const [save, saving] = useAction(setUserTeams);
  const [changeRole, changingRole] = useAction(setUserRole);
  const [changeStatus, changingStatus] = useAction(setUserStatus);

  if (initial) return <Loading rows={6} height={40} />;
  if (!user)
    return (
      <Card>
        <Empty icon="user" title="Member not found">
          <Link to="/admin/members">Back to members</Link>
        </Empty>
      </Card>
    );

  const memberTeams = (teams ?? []).filter((t) => user.teamIds.includes(t.id));
  const available = (teams ?? []).filter((t) => !user.teamIds.includes(t.id));
  // Neither control is offered on your own record. The backend refuses it —
  // an administrator who suspends themselves is locked out of the console
  // that would undo it — so there is no button here to be told no by.
  const isSelf = viewer?.user.id === user.id;

  // Team membership is the only lever here: it decides which policy groups
  // apply and which models they can use, so both are shown beside it.
  const change = async (teamIds: string[], message: string) => {
    await save(user.id, teamIds);
    toast({ tone: "allow", title: message, body: "Effective access updates on the next policy fetch." });
  };

  return (
    <>
      <PageHead
        crumbs={[{ label: "Members", to: "/admin/members" }, { label: user.name }]}
        title={user.name}
        lede={`${user.title} · ${user.email}`}
        meta={<Badge tone={user.role === "member" ? "neutral" : "warn"}>{user.role}</Badge>}
      />

      <div className="grid grid-side">
        <div className="stack">
          <Card>
            <div className="row" style={{ gap: 14 }}>
              <Avatar name={user.name} size="lg" />
              <div className="grow">
                <KV>
                  <Row k="Identity">
                    <code className="small">{user.ssoSubject}</code>
                  </Row>
                  <Row k="Status">
                    {user.status === "active" ? (
                      <Badge tone="allow">active</Badge>
                    ) : (
                      <Badge tone="deny">suspended</Badge>
                    )}
                  </Row>
                  <Row k="Joined">{date(user.createdAt)}</Row>
                  <Row k="Last active">{relative(user.lastActiveAt)}</Row>
                </KV>
              </div>
            </div>
          </Card>

          <Card
            title="Role and access"
            sub={
              isSelf
                ? "Your own record. Another administrator changes these."
                : "Who they are to the identity provider is not ours to change; what they may do here is"
            }
          >
            <div className="field">
              <span className="label">Organization role</span>
              <Segmented
                value={user.role}
                onChange={async (role) => {
                  if (role === user.role) return;
                  await changeRole(user.id, role);
                  toast({
                    tone: role === "admin" ? "warn" : "allow",
                    title:
                      role === "admin"
                        ? `${user.name} is now an administrator`
                        : `${user.name} is now a member`,
                    body:
                      role === "admin"
                        ? "They can see and change every policy, model and person."
                        : "Admin surfaces are closed to them from their next request.",
                  });
                }}
                label="Role"
                options={[
                  { value: "member", label: "Member" },
                  { value: "admin", label: "Administrator" },
                ]}
                disabled={isSelf || changingRole}
              />
              <span className="hint">
                Administrators can read every session trace, change any policy, and manage people.
              </span>
            </div>

            <hr />

            <div className="spread">
              <div className="col" style={{ gap: 2, minWidth: 0 }}>
                <span className="label" style={{ marginBottom: 0 }}>
                  {user.status === "active" ? "Access" : "Suspended"}
                </span>
                <span className="small muted">
                  {user.status === "active"
                    ? "Suspending signs their machines out at once and stops them signing in."
                    : "They cannot sign in, and their machines have been signed out."}
                </span>
              </div>
              {user.status === "active" ? (
                <button
                  className="btn btn--danger"
                  disabled={isSelf || changingStatus}
                  onClick={() => setConfirmingSuspend(true)}
                >
                  <Icon name="lock" size={14} /> Suspend
                </button>
              ) : (
                <button
                  className="btn"
                  disabled={isSelf || changingStatus}
                  onClick={async () => {
                    await changeStatus(user.id, "active");
                    toast({
                      tone: "allow",
                      title: `${user.name} reactivated`,
                      body: "They sign in again from scratch; the old credentials stay revoked.",
                    });
                  }}
                >
                  <Icon name="unlock" size={14} /> Reactivate
                </button>
              )}
            </div>
          </Card>

          <Card title="Teams" sub="What a person is in decides what John may do for them">
            <div className="col" style={{ gap: 8 }}>
              {memberTeams.map((t) => (
                <div key={t.id} className="spread">
                  <div className="col" style={{ gap: 0, minWidth: 0 }}>
                    <Link to={`/admin/teams/${t.slug}`}>{t.name}</Link>
                    <span className="small muted truncate">{t.description}</span>
                  </div>
                  <button
                    className="btn btn--sm"
                    disabled={saving}
                    onClick={() =>
                      change(
                        user.teamIds.filter((x) => x !== t.id),
                        `Removed from ${t.name}`,
                      )
                    }
                  >
                    <Icon name="x" size={13} /> Remove
                  </button>
                </div>
              ))}
              {memberTeams.length === 0 && (
                <p className="small muted">
                  In no team, so only the baseline groups apply to them.
                </p>
              )}
            </div>

            {available.length > 0 && (
              <>
                <hr />
                <label className="field" style={{ marginBottom: 0 }}>
                  <span className="label">Add to a team</span>
                  <select
                    className="select"
                    value=""
                    disabled={saving}
                    onChange={(e) => {
                      const team = available.find((t) => t.id === e.target.value);
                      if (team) void change([...user.teamIds, team.id], `Added to ${team.name}`);
                    }}
                  >
                    <option value="">Choose a team…</option>
                    {available.map((t) => (
                      <option key={t.id} value={t.id}>
                        {t.name}
                      </option>
                    ))}
                  </select>
                </label>
              </>
            )}
          </Card>
        </div>

        <div className="stack">
          <Card title="Effective access">
            <div className="eyebrow" style={{ marginBottom: 6 }}>
              Policy groups
            </div>
            <div className="row row-wrap" style={{ gap: 4, marginBottom: 14 }}>
              {(groups ?? []).map((g) => (
                <Link key={g.id} to={`/admin/groups/${g.id}`}>
                  <Tag baseline={g.baseline}>{g.name}</Tag>
                </Link>
              ))}
            </div>
            <div className="eyebrow" style={{ marginBottom: 6 }}>
              Models
            </div>
            <div className="col" style={{ gap: 4 }}>
              {(models ?? []).map((m) => (
                <div key={m.id} className="spread">
                  <span className="small">{m.name}</span>
                  <Badge tone={m.hosting === "internal" ? "allow" : "info"}>{m.hosting}</Badge>
                </div>
              ))}
              {(models?.length ?? 0) === 0 && (
                <p className="small muted">No models — no team assigns one.</p>
              )}
            </div>
          </Card>
        </div>
      </div>

      {confirmingSuspend && (
        <ConfirmDialog
          title={`Suspend ${user.name}?`}
          danger
          busy={changingStatus}
          confirmLabel="Suspend"
          body={
            <>
              <p>
                Every machine they are signed in on stops working at its next request, and they
                cannot sign in again until an administrator reactivates them.
              </p>
              <p className="small muted">
                This is how someone is offboarded — there is no delete. Their sessions, usage and
                the changes they made stay on the record, which is the point.
              </p>
            </>
          }
          onClose={() => setConfirmingSuspend(false)}
          onConfirm={async () => {
            await changeStatus(user.id, "suspended");
            setConfirmingSuspend(false);
            toast({
              tone: "deny",
              title: `${user.name} suspended`,
              body: "Their machines were signed out.",
            });
          }}
        />
      )}
    </>
  );
}
