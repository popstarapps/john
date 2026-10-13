import { useState } from "react";
import { Link } from "react-router-dom";
import { getViewer, listNetworkAccessRequests } from "../api/client";
import { useQuery } from "../lib/useQuery";
import { Card, Empty, Loading, PageHead, Tabs } from "../components/ui";
import { RequestStatusBadge } from "../components/domain";
import { Icon } from "../components/Icon";
import { relative } from "../lib/format";

export default function Requests() {
  const [tab, setTab] = useState<"open" | "rejected">("open");
  const { data: viewer } = useQuery(() => getViewer(), []);
  const uid = viewer?.user.id;
  const { data: all } = useQuery(
    () =>
      uid ? listNetworkAccessRequests({ requesterId: uid }) : Promise.resolve([]),
    [uid],
  );

  const rows = (all ?? []).filter((r) =>
    tab === "open"
      ? r.status === "pending" || r.status === "needs-info"
      : r.status === "rejected",
  );

  return (
    <>
      <PageHead
        title="My network access requests"
        lede="Destinations you have asked for. Approved ones become policy, see them under Policies."
        actions={
          <Link className="btn btn--primary" to="/requests/new">
            <Icon name="plus" size={14} /> New request
          </Link>
        }
      />

      <Tabs
        value={tab}
        onChange={(v) => setTab(v as typeof tab)}
        options={[
          {
            value: "open",
            label: "Open",
            count: (all ?? []).filter(
              (r) => r.status === "pending" || r.status === "needs-info",
            ).length,
          },
          {
            value: "rejected",
            label: "Rejected",
            count: (all ?? []).filter((r) => r.status === "rejected").length,
          },
        ]}
      />

      <Card pad={false}>
        {!all ? (
          <Loading rows={5} />
        ) : rows.length === 0 ? (
          <Empty icon="inbox" title="Nothing here">
            No requests rejected
          </Empty>
        ) : (
          <div className="table-wrap">
            <table className="tbl">
              <thead>
                <tr>
                  <th>Ref</th>
                  <th>Request</th>
                  <th>Status</th>
                  <th>Submitted</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td className="shrink mono small muted">{r.ref}</td>
                    <td>
                      <Link className="cell-main" to={`/requests/${r.id}`}>
                        {r.title}
                      </Link>
                      <div
                        className="cell-sub truncate"
                        style={{ maxWidth: 420 }}
                      >
                        {r.justification}
                      </div>
                    </td>
                    <td>
                      <RequestStatusBadge status={r.status} />
                    </td>
                    <td className="small muted nowrap">
                      {relative(r.createdAt)}
                      {r.decidedAt && (
                        <div className="cell-sub">
                          decided {relative(r.decidedAt)}
                        </div>
                      )}
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
