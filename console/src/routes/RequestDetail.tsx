import { useEffect, useState } from "react";
import { Link, useLocation, useParams } from "react-router-dom";
import {
  commentOnNetworkAccessRequest,
  decideNetworkAccessRequest,
  getNetworkAccessRequest,
  getViewer,
  listNetworkRequests,
  listPolicyGroups,
  listTeams,
} from "../api/client";
import { useAction, useQuery } from "../lib/useQuery";
import { useDirectory } from "../lib/directory";
import { Avatar, Card, Empty, KV, Loading, PageHead, Row, Tag } from "../components/ui";
import { useToast } from "../components/overlays";
import { MethodChip, NetStatusBadge, RequestStatusBadge, SpecDetail } from "../components/domain";
import { SpecEditor, specValid } from "../components/PolicyEditor";
import { Icon } from "../components/Icon";
import { dateTime, relative } from "../lib/format";
import type { PolicySpec } from "../api/types";

// The group picker's value for "make a new group for this".
const NEW_GROUP = "__new__";

export default function RequestDetail() {
  const { id = "" } = useParams();
  // Which side of the request you are on follows the route, not the role: an
  // administrator opening their own request from /requests is looking at it as
  // the person who asked, and gets the requester's view.
  const reviewer = useLocation().pathname.startsWith("/admin");
  const toast = useToast();
  const dir = useDirectory();
  const [targetGroup, setTargetGroup] = useState<string | null>(null);
  // A group made on the spot: its name, and whether it applies to everyone or
  // to chosen teams. Null until touched, so the defaults follow the request.
  const [newName, setNewName] = useState<string | null>(null);
  const [newScope, setNewScope] = useState<"baseline" | "teams" | null>(null);
  const [newTeams, setNewTeams] = useState<string[] | null>(null);
  const [spec, setSpec] = useState<PolicySpec | null>(null);
  const [submitted, setSubmitted] = useState(false);
  const [comment, setComment] = useState("");

  const { data: viewer } = useQuery(() => getViewer(), []);
  const { data: request, initial } = useQuery(() => getNetworkAccessRequest(id), [id]);
  const { data: groups } = useQuery(() => listPolicyGroups(), []);
  const { data: teams } = useQuery(() => listTeams(), []);
  // The requests that prompted this one. They are the argument for the rule,
  // so both sides see them: the requester to remember what they hit, the
  // reviewer to check the rule against what actually happened.
  const { data: evidence } = useQuery(
    () => listNetworkRequests({ ids: request?.evidenceRequestIds ?? [] }),
    [request?.id],
  );
  const [decide, deciding] = useAction(decideNetworkAccessRequest);
  const [say, saying] = useAction(commentOnNetworkAccessRequest);

  // The reviewer edits a copy of what was proposed; it is only written back if
  // they approve it.
  useEffect(() => {
    if (request) setSpec(request.proposed.spec);
  }, [request?.id]);

  if (initial) return <Loading rows={7} height={40} />;
  if (!request)
    return (
      <Card>
        <Empty icon="inbox" title="Request not found">
          <Link to="/requests">Back to requests</Link>
        </Empty>
      </Card>
    );

  const isAdmin = (viewer?.isAdmin ?? false) && reviewer;
  const open = request.status === "pending" || request.status === "needs-info";
  // Both sides talk here. A reviewer's question hands the request back to the
  // requester; the requester's answer hands it forward again, without either
  // of them having to move it.
  const canComment = open && (isAdmin || request.requesterId === viewer?.user.id);
  const editing = isAdmin && open && spec !== null;
  const draft = spec ?? request.proposed.spec;

  // The group the policy goes in. It defaults to one that applies to the
  // person who asked — the group they proposed, else a baseline group, which
  // applies to everyone, else one attached to their teams — because a policy
  // in a group that does not reach them is an approval that changes nothing.
  const requesterTeams = (teams ?? []).filter((t) => t.memberIds.includes(request.requesterId)).map((t) => t.id);
  const defaultGroup =
    (groups ?? []).find((g) => g.id === request.proposed.groupId)?.id ??
    (groups ?? []).find((g) => g.baseline)?.id ??
    (groups ?? []).find((g) => g.teamIds.some((t) => requesterTeams.includes(t)))?.id ??
    NEW_GROUP;
  const groupId = targetGroup ?? defaultGroup;
  const creating = groupId === NEW_GROUP;
  const host = draft.kind === "network" ? draft.host : "";
  const name = newName ?? (host ? `${host} access` : "");
  const scope = newScope ?? (requesterTeams.length > 0 ? "teams" : "baseline");
  const chosenTeams = newTeams ?? requesterTeams;
  const newGroupError = !name.trim()
    ? "The new group needs a name."
    : scope === "teams" && chosenTeams.length === 0
      ? "Choose at least one team, or apply it to everyone."
      : null;
  const reachesRequester = scope === "baseline" || chosenTeams.some((t) => requesterTeams.includes(t));
  const targetName = creating ? name : (groups ?? []).find((g) => g.id === groupId)?.name;

  return (
    <>
      <PageHead
        crumbs={[
          reviewer
            ? { label: "Network access requests", to: "/admin/requests" }
            : { label: "My requests", to: "/requests" },
          { label: request.ref },
        ]}
        title={request.title}
        meta={<RequestStatusBadge status={request.status} />}
        lede={
          <>
            <span className="mono">{request.ref}</span> · requested by{" "}
            {dir.userName(request.requesterId)} for {dir.teamName(request.teamId)} ·{" "}
            {relative(request.createdAt)}
          </>
        }
      />

      {/* The requester sees what they asked for and what came back. The
          reviewer gets the same thing as a form, with the decision attached to
          the bottom of it — you decide on the rule in front of you, so the
          buttons belong to the rule and not to a panel beside it. */}
      <div className={isAdmin ? "grid grid-side" : undefined}>
        <div className="stack">
          <Card title="Justification">
            <p style={{ whiteSpace: "pre-wrap" }}>{request.justification}</p>
          </Card>

          <Card
            title={editing ? "Policy" : "Proposed policy"}
            sub={
              editing ? (
                "Edit it into the rule you are willing to approve"
              ) : (
                "Network access"
              )
            }
            foot={
              editing ? (
                <div className="decide">
                  <label className="field decide-group">
                    <span className="label">Add to group</span>
                    <select
                      className="select"
                      value={groupId}
                      onChange={(e) => setTargetGroup(e.target.value)}
                    >
                      {(groups ?? []).map((g) => (
                        <option key={g.id} value={g.id}>
                          {g.name}
                          {g.baseline ? " (baseline)" : ""}
                        </option>
                      ))}
                      <option value={NEW_GROUP}>Create a new group…</option>
                    </select>
                    <span className="hint">
                      Approving writes the policy above into this group, exactly as it reads now.
                    </span>
                  </label>
                  {creating && (
                    <div className="stack" style={{ gap: 10 }}>
                      <label className="field">
                        <span className="label">New group name</span>
                        <input className="input" value={name} onChange={(e) => setNewName(e.target.value)} />
                      </label>
                      <label className="field">
                        <span className="label">Applies to</span>
                        <select
                          className="select"
                          value={scope}
                          onChange={(e) => setNewScope(e.target.value as "baseline" | "teams")}
                        >
                          <option value="baseline">Everyone (a baseline group)</option>
                          <option value="teams">Chosen teams</option>
                        </select>
                      </label>
                      {scope === "teams" &&
                        (teams ?? []).map((t) => (
                          <label key={t.id} className={chosenTeams.includes(t.id) ? "check check--on" : "check"}>
                            <input
                              type="checkbox"
                              checked={chosenTeams.includes(t.id)}
                              onChange={() =>
                                setNewTeams(
                                  chosenTeams.includes(t.id)
                                    ? chosenTeams.filter((x) => x !== t.id)
                                    : [...chosenTeams, t.id],
                                )
                              }
                            />
                            <span>
                              {t.name}
                              {requesterTeams.includes(t.id) && (
                                <span className="hint">{dir.userName(request.requesterId)} is a member</span>
                              )}
                            </span>
                          </label>
                        ))}
                      {submitted && newGroupError ? (
                        <span className="err">{newGroupError}</span>
                      ) : (
                        !reachesRequester && (
                          <span className="hint">
                            None of these teams includes {dir.userName(request.requesterId)}, so this does not
                            unblock the request they made.
                          </span>
                        )
                      )}
                    </div>
                  )}
                  <div className="decide-actions">
                    <button
                      className="btn btn--allow"
                      disabled={deciding}
                      onClick={async () => {
                        setSubmitted(true);
                        // Requests are network access only, so the editor
                        // cannot hand back another kind of spec here.
                        if (!specValid(draft) || draft.kind !== "network") return;
                        if (creating && newGroupError) return;
                        try {
                          await decide(
                            request.id,
                            "approved",
                            creating
                              ? {
                                  spec: draft,
                                  newGroup: {
                                    name: name.trim(),
                                    baseline: scope === "baseline",
                                    teamIds: scope === "teams" ? chosenTeams : [],
                                  },
                                }
                              : { spec: draft, groupId },
                          );
                        } catch (err) {
                          toast({
                            tone: "deny",
                            title: `${request.ref} was not approved`,
                            body: err instanceof Error ? err.message : "The backend refused.",
                          });
                          return;
                        }
                        toast({
                          tone: "allow",
                          title: `${request.ref} approved`,
                          body: `Added to ${targetName ?? "the group"}. Running sessions pick it up straight away.`,
                        });
                      }}
                    >
                      <Icon name="check" size={14} /> Approve and add policy
                    </button>
                    <button
                      className="btn btn--danger"
                      disabled={deciding}
                      onClick={async () => {
                        await decide(request.id, "rejected");
                        toast({ tone: "deny", title: `${request.ref} rejected` });
                      }}
                    >
                      <Icon name="ban" size={14} /> Reject
                    </button>
                  </div>
                </div>
              ) : undefined
            }
          >
            {editing ? (
              <SpecEditor
                spec={draft}
                onChange={setSpec}
                models={dir.models}
                showErrors={submitted}
                advanced
              />
            ) : (
              <SpecDetail spec={draft} modelName={(mid) => dir.modelName(mid)} />
            )}
          </Card>

          {(request.comments.length > 0 || canComment) && (
            <Card
              title="Conversation"
              sub={
                isAdmin
                  ? "Ask before deciding, rather than rejecting something you only need explained"
                  : "Anything a reviewer asks lands here"
              }
            >
              {request.comments.length === 0 ? (
                <p className="small muted">Nothing said yet.</p>
              ) : (
                <div className="col" style={{ gap: 14 }}>
                  {request.comments.map((c) => (
                    <div key={c.id} className="row" style={{ gap: 10, alignItems: "flex-start" }}>
                      <Avatar name={dir.userName(c.authorId)} size="sm" />
                      <div className="col grow" style={{ gap: 2, minWidth: 0 }}>
                        <span className="small">
                          <strong>{dir.userName(c.authorId)}</strong>{" "}
                          <span className="muted">{relative(c.at)}</span>
                        </span>
                        <p style={{ margin: 0, whiteSpace: "pre-wrap" }}>{c.body}</p>
                      </div>
                    </div>
                  ))}
                </div>
              )}

              {canComment && (
                <>
                  <hr />
                  <label className="field">
                    <span className="label">
                      {isAdmin ? "Reply or ask the requester something" : "Reply"}
                    </span>
                    <textarea
                      className="textarea"
                      value={comment}
                      placeholder={
                        isAdmin
                          ? "Which build needs this, and does it have to be write access?"
                          : "Answer the question here."
                      }
                      onChange={(e) => setComment(e.target.value)}
                    />
                  </label>
                  <div className="row" style={{ gap: 8 }}>
                    <button
                      className="btn"
                      disabled={saying || !comment.trim()}
                      onClick={async () => {
                        await say(request.id, comment.trim());
                        setComment("");
                        toast({
                          tone: "allow",
                          title: "Comment added",
                          body:
                            !isAdmin && request.status === "needs-info"
                              ? `${request.ref} is back with the reviewers.`
                              : undefined,
                        });
                      }}
                    >
                      <Icon name="check" size={14} /> Comment
                    </button>
                    {isAdmin && request.status !== "needs-info" && (
                      <button
                        className="btn"
                        disabled={saying || !comment.trim()}
                        onClick={async () => {
                          await say(request.id, comment.trim(), { needsInfo: true });
                          setComment("");
                          toast({
                            tone: "warn",
                            title: `${request.ref} needs more information`,
                            body: "It is out of your queue until they answer.",
                          });
                        }}
                      >
                        <Icon name="info" size={14} /> Ask and hand back
                      </button>
                    )}
                  </div>
                </>
              )}
            </Card>
          )}

          {!isAdmin && request.decidedAt && (
            <Card title={request.status === "approved" ? "Approved" : "Rejected"}>
              <p className="small muted">
                {dateTime(request.decidedAt)} · {dir.userName(request.decidedById)}
              </p>
              {request.decisionNote && <p style={{ marginTop: 8 }}>{request.decisionNote}</p>}
            </Card>
          )}

          {!isAdmin && open && (
            <div className="callout callout--info">
              {request.status === "needs-info"
                ? "A reviewer has asked for more information. Answering above puts it back in front of them."
                : "Waiting on an administrator. You'll be notified here and by email when it is decided."}
            </div>
          )}
          {evidence && evidence.length > 0 && (
            <Card
              title="What was blocked"
              sub="The proxy requests this asks to cover. Approving a policy does not retry them."
              pad={false}
            >
              <div className="table-wrap">
                <table className="tbl evidence">
                  <tbody>
                    {evidence.map((n) => (
                      <tr key={n.id}>
                        <td className="shrink">
                          <MethodChip method={n.method} />
                        </td>
                        <td className="evidence-url mono small" title={n.host + n.path}>
                          {n.host}
                          {n.path}
                        </td>
                        <td className="shrink">
                          <NetStatusBadge status={n.status} />
                        </td>
                        <td className="evidence-at small muted nowrap">{relative(n.at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </Card>
          )}
        </div>

        {isAdmin && (
          <div className="stack">
            <Card title="Details">
              <KV>
                <Row k="Reference">
                  <code>{request.ref}</code>
                </Row>
                <Row k="Requester">{dir.userName(request.requesterId)}</Row>
                <Row k="Team">
                  <Link to={`/admin/teams/${dir.team(request.teamId)?.slug ?? ""}`}>
                    <Tag>{dir.teamName(request.teamId)}</Tag>
                  </Link>
                </Row>
                <Row k="Submitted">{dateTime(request.createdAt)}</Row>
                {request.decidedAt && <Row k="Decided">{dateTime(request.decidedAt)}</Row>}
                {request.decidedById && (
                  <Row k="Decided by">{dir.userName(request.decidedById)}</Row>
                )}
              </KV>
            </Card>
          </div>
        )}
      </div>
    </>
  );
}
