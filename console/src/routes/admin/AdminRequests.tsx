import { useState } from "react";
import { Link } from "react-router-dom";
import { listNetworkAccessRequests } from "../../api/client";
import { useQuery } from "../../lib/useQuery";
import { useDirectory } from "../../lib/directory";
import { Card, Empty, Loading, PageHead, SearchInput, Segmented } from "../../components/ui";
import { RequestStatusBadge, SpecSummary } from "../../components/domain";
import { relative } from "../../lib/format";

/** Only what still needs a decision. A request that has been decided is a
 *  policy, or it is nothing — either way it is not work.
 *
 *  A request a reviewer handed back is not work either, until it is answered.
 *  So the queue opens on what is actually waiting on a reviewer, and the ones
 *  waiting on their requester are a tab away rather than mixed in. */
export default function AdminRequests() {
  const [q, setQ] = useState("");
  const [scope, setScope] = useState<"pending" | "needs-info" | "open">("pending");
  const dir = useDirectory();
  const { data: rows } = useQuery(
    () => listNetworkAccessRequests({ status: scope, search: q || undefined }),
    [q, scope],
  );

  return (
    <>
      <PageHead
        title="Network access requests"
        lede="Network access developers have asked for and cannot have until someone decides."
      />

      <Card pad={false}>
        <div className="toolbar">
          <SearchInput
            value={q}
            onChange={setQ}
            placeholder="Title, reference or justification"
          />
          <Segmented
            value={scope}
            onChange={setScope}
            label="Waiting on"
            options={[
              { value: "pending", label: "Waiting on us" },
              { value: "needs-info", label: "Waiting on them" },
              { value: "open", label: "All open" },
            ]}
          />
        </div>
        {!rows ? (
          <Loading rows={6} />
        ) : rows.length === 0 ? (
          <Empty
            icon="check"
            title={scope === "needs-info" ? "Nothing waiting on a requester" : "Nothing to review"}
          >
            {scope === "needs-info"
              ? "Requests you have asked a question about appear here until they answer."
              : "Developers submit these when the proxy blocks a request nothing covers — from the CLI as it happens, or from the console later."}
          </Empty>
        ) : (
          <div className="table-wrap">
            <table className="tbl">
              <thead>
                <tr>
                  <th>Ref</th>
                  <th>Request</th>
                  <th>Requester</th>
                  <th>Status</th>
                  <th>Age</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td className="shrink mono small muted">{r.ref}</td>
                    <td>
                      <Link
                        className="cell-main"
                        to={`/admin/requests/${r.id}`}
                      >
                        {r.title}
                      </Link>
                      <div className="cell-sub">
                        <SpecSummary spec={r.proposed.spec} />
                      </div>
                    </td>
                    <td className="small">
                      {dir.userName(r.requesterId)}
                      <div className="cell-sub">{dir.teamName(r.teamId)}</div>
                    </td>
                    <td className="shrink">
                      <RequestStatusBadge status={r.status} />
                    </td>
                    <td className="small muted nowrap">
                      {relative(r.createdAt)}
                    </td>
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
