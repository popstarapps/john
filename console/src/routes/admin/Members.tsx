import { useState } from "react";
import { Link } from "react-router-dom";
import { getOrg, listTeams, listUsers } from "../../api/client";
import { useQuery } from "../../lib/useQuery";
import { Badge, Card, Empty, Loading, PageHead, Person, SearchInput, Segmented, Tag } from "../../components/ui";
import { date, relative } from "../../lib/format";
import type { Role } from "../../api/types";

const ROLE_TONE: Record<Role, "warn" | "neutral"> = {
  admin: "warn",
  member: "neutral",
};

export default function Members() {
  const [q, setQ] = useState("");
  const [scope, setScope] = useState<"all" | "admins">("all");
  const { data: users } = useQuery(() => listUsers({ search: q || undefined }), [q]);
  const { data: teams } = useQuery(() => listTeams(), []);
  const { data: org } = useQuery(() => getOrg(), []);

  const rows = (users ?? []).filter((u) =>
    scope === "all" ? true : u.role === "admin",
  );

  return (
    <>
      <PageHead
        title="Members"
        lede="Everyone who can sign in. Accounts and names come from the identity provider; teams and roles are held here."
      />

      {org?.standalone && (
        <div className="callout callout--info" style={{ marginBottom: 16 }}>
          Standalone has one member: you. In an organization, people appear here the first time
          they sign in.
        </div>
      )}

      <Card pad={false}>
        <div className="toolbar">
          <SearchInput value={q} onChange={setQ} placeholder="Name, email or title" />
          <Segmented
            value={scope}
            onChange={setScope}
            label="Filter"
            options={[
              { value: "all", label: "All" },
              { value: "admins", label: "Administrators" },
            ]}
          />
        </div>

        {!users ? (
          <Loading rows={8} />
        ) : rows.length === 0 ? (
          <Empty icon="user" title="No members match" />
        ) : (
          <div className="table-wrap">
            <table className="tbl">
              <thead>
                <tr>
                  <th>Person</th>
                  <th>Teams</th>
                  <th>Role</th>
                  <th>Status</th>
                  <th>Last active</th>
                  <th>Joined</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((u) => (
                  <tr key={u.id}>
                    <td>
                      <Person name={u.name} sub={u.email} to={`/admin/members/${u.id}`} />
                    </td>
                    <td>
                      <span className="row row-wrap" style={{ gap: 4 }}>
                        {u.teamIds.map((tid) => {
                          const team = teams?.find((t) => t.id === tid);
                          return team ? (
                            <Link key={tid} to={`/admin/teams/${team.slug}`}>
                              <Tag>{team.name}</Tag>
                            </Link>
                          ) : null;
                        })}
                      </span>
                    </td>
                    <td>
                      <Badge tone={ROLE_TONE[u.role]}>{u.role}</Badge>
                    </td>
                    <td>
                      {u.status === "active" ? (
                        <Badge tone="allow">active</Badge>
                      ) : (
                        <Badge tone="deny">suspended</Badge>
                      )}
                    </td>
                    <td className="small muted nowrap">{relative(u.lastActiveAt)}</td>
                    <td className="small muted nowrap">{date(u.createdAt)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}
