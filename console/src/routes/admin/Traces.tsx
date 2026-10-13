import { useState } from "react";
import {
  getOrg,
  grantTrace,
  listTraceSessions,
  lockTraces,
  readTrace,
  traceRecipients,
  tracesUnlocked,
  unlockTraces,
} from "../../api/client";
import { useQuery } from "../../lib/useQuery";
import { Badge, Card, Empty, KV, Loading, PageHead, Pager, Row, SearchInput } from "../../components/ui";
import { Drawer, useToast } from "../../components/overlays";
import { Icon } from "../../components/Icon";
import { dateTime, relative } from "../../lib/format";
import type { TraceCategory, TraceEntry, TraceSession } from "../../api/types";

const CATEGORIES: Array<TraceCategory | "all"> = [
  "all",
  "session",
  "prompt",
  "tool",
  "network",
  "model",
  "workspace",
];

export default function Traces() {
  const { data: unlocked } = useQuery(() => tracesUnlocked(), []);
  if (unlocked === undefined) return <Loading rows={4} height={44} />;
  return unlocked ? <Browse /> : <Unlock />;
}

/* ---------- locked ---------- */

function Unlock() {
  const toast = useToast();
  const [key, setKey] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const { data: org } = useQuery(() => getOrg(), []);

  return (
    <>
      <PageHead
        title="Session traces"
        lede="Everything that happens inside a John session — prompts, model exchanges, tool calls, network decisions and applied changes — encrypted with a key the backend cannot read. It appends records and can never open them."
      />
      <div className="grid grid-side">
        <Card title="Unlock the traces" sub="Requires the private read key, which is held outside this backend">
          <label className="field">
            <span className="label">Trace read key</span>
            <textarea
              className="textarea input--mono"
              value={key}
              placeholder="AGE-SECRET-KEY-1…"
              onChange={(e) => {
                setKey(e.target.value);
                setError(null);
              }}
            />
            {error ? <span className="err">{error}</span> : (
              <span className="hint">
                The key is used in this browser tab only, to decrypt records as they are read.
                It is never sent to the backend and never stored.
              </span>
            )}
          </label>
          <button
            className="btn btn--primary"
            disabled={busy || !key.trim()}
            onClick={async () => {
              setBusy(true);
              const res = await unlockTraces(key);
              setBusy(false);
              if (!res.ok) return setError(res.error ?? "Could not unlock.");
              setKey("");
              toast({ tone: "allow", title: "Traces unlocked", body: "Records decrypt as you read them." });
            }}
          >
            <Icon name="unlock" size={14} /> {busy ? "Checking…" : "Unlock"}
          </button>
        </Card>

        <div className="stack">
          <Card title="How this works">
            <KV>
              <Row k="Public key">
                <code className="small">{org?.traceKey.fingerprint || "not configured"}</code>
              </Row>
              <Row k="Algorithm">{org?.traceKey.algorithm}</Row>
              <Row k="Custodian">{org?.traceKey.custodian}</Row>
              <Row k="Store">
                <code className="small">{org?.traceStore.location || "none"}</code>
              </Row>
              <Row k="Size">{(org?.backend.traceSizeMb ?? 0).toFixed(1)} MB</Row>
              <Row k="Retention">Kept forever</Row>
            </KV>
          </Card>
          <div className="callout callout--warn">
            Each session is encrypted with its own key, wrapped for this organization and for the
            developer it belongs to. An attacker with the backend host, the database and the trace
            store still cannot read a session — but neither can you, without the private key.
          </div>
        </div>
      </div>
    </>
  );
}

/* ---------- unlocked ---------- */

function Browse() {
  const toast = useToast();
  const [q, setQ] = useState("");
  const [limit, setLimit] = useState(50);
  const [open, setOpen] = useState<TraceSession | null>(null);
  const { data: sessions } = useQuery(() => listTraceSessions(), []);

  const shown = (sessions ?? []).filter((s) => {
    if (!q) return true;
    const hay = `${s.id} ${s.project} ${s.actorName} ${s.actorEmail}`.toLowerCase();
    return hay.includes(q.toLowerCase());
  });

  return (
    <>
      <PageHead
        title="Session traces"
        lede="Sessions are fetched sealed and decrypted in this browser; the backend cannot read what it stored."
        meta={<Badge tone="allow" icon="unlock">Unlocked</Badge>}
        actions={
          <button
            className="btn"
            onClick={async () => {
              await lockTraces();
              toast({ tone: "warn", title: "Traces locked" });
            }}
          >
            <Icon name="lock" size={14} /> Lock
          </button>
        }
      />

      <Card pad={false}>
        <div className="toolbar">
          <SearchInput value={q} onChange={setQ} placeholder="Filter by session, project or developer" />
        </div>

        {!sessions ? (
          <Loading rows={10} height={26} />
        ) : shown.length === 0 ? (
          <Empty icon="book" title="No sessions match" />
        ) : (
          <div className="table-wrap">
            <table className="tbl">
              <thead>
                <tr>
                  <th>Started</th>
                  <th>Session</th>
                  <th>Developer</th>
                  <th>Turns</th>
                  <th>Status</th>
                </tr>
              </thead>
              <tbody>
                {shown.slice(0, limit).map((s) => (
                  <tr key={s.id} className="clickable" onClick={() => setOpen(s)}>
                    <td className="small muted nowrap">{dateTime(s.startedAt)}</td>
                    <td>
                      <span className="small">{s.project}</span>
                      <div className="cell-sub mono">{s.id}</div>
                    </td>
                    <td className="small">{s.actorName}</td>
                    <td className="small">{s.turns}</td>
                    <td className="small muted">{s.status}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {shown.length > 0 && (
          <Pager shown={Math.min(limit, shown.length)} total={shown.length} onMore={() => setLimit((n) => n + 50)} />
        )}
      </Card>

      {open && <SessionDrawer session={open} onClose={() => setOpen(null)} />}
    </>
  );
}

/* ---------- one session ---------- */

function SessionDrawer({ session, onClose }: { session: TraceSession; onClose: () => void }) {
  const toast = useToast();
  const [category, setCategory] = useState<TraceCategory | "all">("all");
  const [record, setRecord] = useState<TraceEntry | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [granting, setGranting] = useState(false);

  const { data: records } = useQuery(
    () =>
      readTrace(session.id).catch((err: unknown) => {
        setError(err instanceof Error ? err.message : "Could not read this session.");
        return [] as TraceEntry[];
      }),
    [session.id],
  );
  const { data: recipients } = useQuery(() => traceRecipients(), []);
  const owner = (recipients ?? []).find((r) => r.id === session.actorId);

  const shown = (records ?? []).filter((r) => category === "all" || r.category === category);
  // The numbers run from one with no gaps, or a line was removed.
  const gap = (records ?? []).some((r, i) => i > 0 && r.seq !== (records ?? [])[i - 1].seq + 1);

  return (
    <Drawer title={session.project || session.id} sub={relative(session.startedAt)} onClose={onClose}>
      <KV>
        <Row k="Session">
          <code className="small">{session.id}</code>
        </Row>
        <Row k="Developer">
          {session.actorName} <span className="muted small">{session.actorEmail}</span>
        </Row>
        <Row k="Started">{dateTime(session.startedAt)}</Row>
        <Row k="Turns">{session.turns}</Row>
        <Row k="Records">{records ? records.length : "…"}</Row>
      </KV>

      {error && <div className="callout callout--warn" style={{ marginTop: 12 }}>{error}</div>}
      {gap && (
        <div className="callout callout--warn" style={{ marginTop: 12 }}>
          The sequence numbers in this file are not contiguous: a line has been removed.
        </div>
      )}

      {owner && !owner.recipient && (
        <div className="callout" style={{ marginTop: 12 }}>
          {session.actorName}&rsquo;s client has not registered a trace key, so they cannot read
          their own sessions yet.
        </div>
      )}
      {owner && owner.recipient && !error && (
        <button
          className="btn"
          style={{ marginTop: 12 }}
          disabled={granting}
          onClick={async () => {
            setGranting(true);
            const res = await grantTrace(session.id, owner.id, owner.recipient);
            setGranting(false);
            if (!res.ok) {
              toast({ tone: "deny", title: "Could not grant access", body: res.error });
              return;
            }
            toast({
              tone: "allow",
              title: "Access granted",
              body: `${session.actorName} can read this session with their current key.`,
            });
          }}
        >
          <Icon name="unlock" size={14} /> {granting ? "Granting…" : "Grant to current key"}
        </button>
      )}

      <div className="toolbar" style={{ marginTop: 16 }}>
        <select
          className="select"
          style={{ width: 170 }}
          value={category}
          onChange={(e) => setCategory(e.target.value as TraceCategory | "all")}
        >
          {CATEGORIES.map((c) => (
            <option key={c} value={c}>
              {c === "all" ? "All categories" : c}
            </option>
          ))}
        </select>
      </div>

      {!records ? (
        <Loading rows={8} height={24} />
      ) : shown.length === 0 ? (
        <Empty icon="book" title={error ? "Nothing could be decrypted" : "No records match"} />
      ) : (
        <div className="table-wrap">
          <table className="tbl">
            <thead>
              <tr>
                <th>When</th>
                <th>Action</th>
                <th>Detail</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((r) => (
                <tr key={r.id} className="clickable" onClick={() => setRecord(r)}>
                  <td className="small muted nowrap">{dateTime(r.at)}</td>
                  <td>
                    <span className="mono small">{r.action}</span>
                    <div className="cell-sub">{r.category}</div>
                  </td>
                  <td className="small truncate" style={{ maxWidth: 320 }}>
                    {r.summary}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {record && (
        <Drawer title={record.action} sub={dateTime(record.at)} onClose={() => setRecord(null)}>
          <KV>
            <Row k="Record">
              <code className="small">{record.id}</code>
            </Row>
            <Row k="Line">{record.seq}</Row>
            <Row k="Category">{record.category}</Row>
          </KV>
          <pre className="pre small" style={{ marginTop: 14, whiteSpace: "pre-wrap" }}>
            {JSON.stringify(record.payload, null, 2)}
          </pre>
        </Drawer>
      )}
    </Drawer>
  );
}
