import { getOrg, listReleases, type Release } from "../api/client";
import { useQuery } from "../lib/useQuery";
import { Badge, Card, CopyLine, Loading, PageHead } from "../components/ui";
import { Icon } from "../components/Icon";

// How each platform the backend can serve is labelled. The list of what is
// actually available comes from the backend — a build with no clients bundled
// simply shows none.
const LABELS: Record<string, { label: string; sub: string }> = {
  "darwin-arm64": { label: "macOS", sub: "Apple silicon" },
  "darwin-amd64": { label: "macOS", sub: "Intel" },
  "windows-amd64": { label: "Windows", sub: "x86-64" },
  "linux-amd64": { label: "Linux", sub: "x86-64" },
  "linux-arm64": { label: "Linux", sub: "arm64" },
};

function labelFor(r: Release) {
  return LABELS[`${r.os}-${r.arch}`] ?? { label: r.os, sub: r.arch };
}

function sizeMB(bytes: number) {
  return bytes > 0 ? `${(bytes / (1 << 20)).toFixed(0)} MB` : "";
}

export default function Download() {
  const { data: org } = useQuery(() => getOrg(), []);
  const { data: releases } = useQuery(() => listReleases(), []);
  if (!org) return <Loading rows={3} height={110} />;

  const version = org.backend.version;

  return (
    <>
      <PageHead
        title="Download John"
        lede="One install puts the CLI, the sandbox host and the local network proxy on your machine. It needs hardware virtualization, and nothing else."
        meta={<Badge tone="allow">{version}</Badge>}
      />

      {org.standalone && (
        <div className="callout callout--info" style={{ marginBottom: 16 }}>
          You are running john already. In an organization, developers download it from this page
          and sign in to the organization&apos;s backend.
        </div>
      )}

      {releases && releases.length === 0 ? (
        <Card>
          <p className="small muted">
            {org.standalone ? (
              <>
                This build has no clients to hand out. The <code>full</code> builds on the release page
                carry one for every platform.
              </>
            ) : (
              <>
                This backend was built without client binaries bundled. Rebuild it with{" "}
                <code>make release</code> to embed a client for every platform.
              </>
            )}
          </p>
        </Card>
      ) : (
        <div className="grid grid-3">
          {(releases ?? []).map((r) => {
            const { label, sub } = labelFor(r);
            return (
              <Card key={r.file}>
                <div className="row" style={{ gap: 10, marginBottom: 10 }}>
                  <Icon name="download" size={18} />
                  <div className="col" style={{ gap: 0 }}>
                    <strong>{label}</strong>
                    <span className="small muted">{sub}</span>
                  </div>
                </div>
                <a className="btn btn--primary btn--wide" href={`/releases/${r.file}`} download>
                  Download {version}
                </a>
                <p className="small muted" style={{ marginTop: 8 }}>
                  {r.file}
                  {r.sizeBytes ? ` · ${sizeMB(r.sizeBytes)}` : ""}
                </p>
              </Card>
            );
          })}
        </div>
      )}

      {/* Full width under the cards: it is the next step, not a footnote. */}
      <div style={{ marginTop: 16 }}>
        <Card title="Then sign in">
          {/* The address this page is on is the backend's, as the developer's
              browser reaches it — which is the one their client needs too. */}
          <CopyLine
            text={org.standalone ? "john login standalone" : `john login ${window.location.origin}`}
          />
          <p className="small muted" style={{ marginTop: 8 }}>
            Opens {org.sso.provider} in your browser and stores a token for this machine. After
            that, run <code>john</code> from any project directory.
          </p>
        </Card>
      </div>
    </>
  );
}
