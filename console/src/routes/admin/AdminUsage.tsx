import { useState } from "react";
import { Link } from "react-router-dom";
import { getOrg, getUsage } from "../../api/client";
import { useQuery } from "../../lib/useQuery";
import { Card, Loading, Meter, PageHead, Person, Segmented, Tag } from "../../components/ui";
import { ByModel, SpendPerDay, UsageTiles } from "../../components/usage";
import { compact, money } from "../../lib/format";

type Range = "7" | "30" | "60";

/**
 * The administrator's home.
 *
 * The same question as `/`, one scale up: what the organization spent, on
 * what, and who spent it. Everything an administrator needs to notice a model
 * nobody uses, a team whose spend doubled, or a person running John far harder
 * than anyone else.
 */
export default function AdminUsage() {
  const [range, setRange] = useState<Range>("30");
  const { data: org } = useQuery(() => getOrg(), []);
  const { data } = useQuery(() => getUsage({ days: Number(range), scope: "org" }), [range]);

  if (!data || !org) return <Loading rows={6} height={70} />;

  const topTeamCost = Math.max(...data.byTeam.map((r) => r.costUsd), 1);
  const people = data.byUser.slice(0, 10);

  return (
    <>
      <PageHead
        title="Usage"
        lede={`${org.name} · the whole organization, last ${range} days.`}
        actions={
          <Segmented
            value={range}
            onChange={setRange}
            label="Time range"
            options={[
              { value: "7", label: "7d" },
              { value: "30", label: "30d" },
              { value: "60", label: "60d" },
            ]}
          />
        }
      />

      <div className="stack">
        <UsageTiles summary={data} days={range} />
        <SpendPerDay summary={data} days={range} />

        <div className="grid grid-2">
          <ByModel summary={data} />

          <Card title="By team" sub={`Spend across ${data.byTeam.length} teams`}>
            {data.byTeam.map((row) => (
              <Meter
                key={row.team.id}
                label={<Link to={`/admin/teams/${row.team.slug}`}>{row.team.name}</Link>}
                value={row.costUsd}
                max={topTeamCost}
                display={money(row.costUsd, 2)}
              />
            ))}
            <p className="small muted" style={{ marginTop: 12 }}>
              A team's spend is the sum of its members'. Someone in two teams counts in both.
            </p>
          </Card>
        </div>

        <Card
          title="Heaviest users"
          sub={`Top ${people.length} by spend · last ${range} days`}
          pad={false}
        >
          <div className="table-wrap">
            <table className="tbl">
              <thead>
                <tr>
                  <th>Person</th>
                  <th>Team</th>
                  <th className="right">Sessions</th>
                  <th className="right">Requests</th>
                  <th className="right">Spend</th>
                </tr>
              </thead>
              <tbody>
                {people.map((row) => {
                  const team = data.byTeam.find((t) => t.team.id === row.user.teamIds[0]);
                  return (
                    <tr key={row.user.id}>
                      <td>
                        <Person
                          name={row.user.name}
                          sub={row.user.title}
                          to={`/admin/members/${row.user.id}`}
                        />
                      </td>
                      <td className="small">
                        {team ? (
                          <Link to={`/admin/teams/${team.team.slug}`}>
                            <Tag>{team.team.name}</Tag>
                          </Link>
                        ) : (
                          <span className="muted">—</span>
                        )}
                      </td>
                      <td className="right num">{compact(row.sessions)}</td>
                      <td className="right num">{compact(row.requests)}</td>
                      <td className="right num">{money(row.costUsd, 2)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Card>
      </div>
    </>
  );
}
