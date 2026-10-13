import { Link } from "react-router-dom";
import {
  getViewer,
  groupsForUser,
  listPolicyGroups,
  listTeams,
  modelsForUser,
  policiesForUser,
} from "../api/client";
import { useQuery } from "../lib/useQuery";
import { useDirectory } from "../lib/directory";
import { Badge, Card, Loading, PageHead, Tag } from "../components/ui";
import { EffectivePolicies } from "../components/policies";
import { providerLabel } from "../components/ModelDialog";

export default function MyPolicies() {
  const dir = useDirectory();
  const { data: viewer } = useQuery(() => getViewer(), []);
  const uid = viewer?.user.id;
  const { data: policies } = useQuery(
    () => (uid ? policiesForUser(uid) : Promise.resolve([])),
    [uid],
  );
  const { data: groups } = useQuery(() => (uid ? groupsForUser(uid) : Promise.resolve([])), [uid]);
  const { data: models } = useQuery(() => (uid ? modelsForUser(uid) : Promise.resolve([])), [uid]);
  const { data: teams } = useQuery(() => listTeams(), []);
  const { data: allGroups } = useQuery(() => listPolicyGroups(), []);

  if (!policies || !groups) return <Loading rows={8} height={40} />;

  // Teams are the reason any of this applies, so they are the first thing on
  // the page: a group attaches to a team, and membership is what pulls it in.
  const myTeams = (teams ?? []).filter((t) => viewer?.user.teamIds.includes(t.id));
  const baseline = groups.filter((g) => g.baseline);

  return (
    <>
      <PageHead
        title="Policies that apply to you"
        lede="The effective set, resolved across every group attached to your teams. Where policies conflict, the most restrictive result wins."
      />

      <div className="grid grid-2" style={{ marginBottom: 18 }}>
        <Card title="Your teams" sub="Membership is what pulls a policy group in">
          <div className="col" style={{ gap: 14 }}>
            {myTeams.map((team) => {
              const attached = (allGroups ?? []).filter(
                (g) => !g.baseline && g.teamIds.includes(team.id),
              );
              return (
                <div key={team.id} className="col" style={{ gap: 6 }}>
                  <div>
                    <strong style={{ fontWeight: 500 }}>{team.name}</strong>
                    <div className="small muted">{team.description}</div>
                  </div>
                  <div className="row row-wrap" style={{ gap: 5 }}>
                    {attached.map((g) => (
                      <Tag key={g.id}>{g.name}</Tag>
                    ))}
                    {attached.length === 0 && (
                      <span className="small muted">
                        No groups of its own — only the baseline applies.
                      </span>
                    )}
                  </div>
                </div>
              );
            })}
            {myTeams.length === 0 && (
              <p className="small muted">
                You are not in a team yet, so only the baseline groups apply. Ask an administrator
                to add you.
              </p>
            )}
          </div>

          <hr />

          <div className="eyebrow" style={{ marginBottom: 6 }}>
            Baseline — attached to every team
          </div>
          <div className="row row-wrap" style={{ gap: 5 }}>
            {baseline.map((g) => (
              <Tag key={g.id} baseline>
                {g.name}
              </Tag>
            ))}
          </div>
        </Card>

        <Card title="Models you can use" pad>
          <div className="col" style={{ gap: 6 }}>
            {(models ?? []).map((m) => (
              <div key={m.id} className="spread">
                <span>{m.name}</span>
                <Badge tone={m.hosting === "internal" ? "allow" : "info"}>
                  {m.hosting === "internal" ? "internal" : providerLabel(m.provider)}
                </Badge>
              </div>
            ))}
            {models?.length === 0 && (
              <p className="small muted">No models are assigned to your teams yet.</p>
            )}
          </div>
          <p className="small muted" style={{ marginTop: 12 }}>
            A model-routing policy can narrow this mid-session; the routing table below says when.
          </p>
        </Card>
      </div>

      <EffectivePolicies
        policies={policies}
        modelName={(id) => dir.modelName(id)}
        requestLink={
          <span>
            Anything not listed is <strong>unknown</strong>, which means blocked the moment it is
            made. John offers to file a request when that happens, and you can{" "}
            <Link to="/requests/new">request network access</Link> here too — either way it covers
            the next request, never the one that was blocked.
          </span>
        }
      />
    </>
  );
}
