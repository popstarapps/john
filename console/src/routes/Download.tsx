import { useEffect, useState } from "react";
import { getOrg, listReleases, releaseLink, type Release } from "../api/client";
import { useQuery } from "../lib/useQuery";
import { Badge, Card, CopyLine, Loading, PageHead, Tabs } from "../components/ui";
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

type Platform = "darwin" | "linux" | "windows";

const PLATFORM_LABELS: Record<Platform, string> = { darwin: "macOS", linux: "Linux", windows: "Windows" };

function thisPlatform(): Platform {
  const ua = navigator.userAgent;
  if (/Windows/.test(ua)) return "windows";
  if (/Mac/.test(ua)) return "darwin";
  return "linux";
}

// What a developer pastes into a terminal: fetch the build for this machine
// into a directory of their own, and put that directory on PATH for this
// shell and every one after. Nothing needs admin rights. The link carries a
// short-lived token, because a terminal has no browser session to show.
function installScript(platform: Platform, releases: Release[], origin: string, token: string): string {
  if (platform === "windows") {
    const file = releases.find((r) => r.os === "windows")?.file ?? "john-windows-amd64.exe";
    return [
      `$d = "$env:LOCALAPPDATA\\Programs\\john"; New-Item -ItemType Directory -Force $d | Out-Null`,
      `$ProgressPreference = 'SilentlyContinue'; Invoke-WebRequest "${origin}/releases/${file}?t=${token}" -OutFile "$d\\john.exe"`,
      `$p = [Environment]::GetEnvironmentVariable('Path', 'User')`,
      `if (";$p;" -notlike "*;$d;*") { [Environment]::SetEnvironmentVariable('Path', "$p;$d", 'User') }`,
      `$env:Path += ";$d"`,
    ].join("\n");
  }
  // uname -m says x86_64 and aarch64 where the builds say amd64 and arm64.
  const rc = platform === "darwin" ? "~/.zshrc" : "~/.bashrc";
  return [
    `mkdir -p ~/.local/bin`,
    `curl -fL "${origin}/releases/john-${platform}-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')?t=${token}" -o ~/.local/bin/john`,
    `chmod +x ~/.local/bin/john`,
    `grep -qs '.local/bin' ${rc} || echo 'export PATH="$HOME/.local/bin:$PATH"' >> ${rc}`,
    `export PATH="$HOME/.local/bin:$PATH"`,
  ].join("\n");
}

function CopyBlock({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div style={{ position: "relative" }}>
      <pre className="code" style={{ paddingRight: 44 }}>
        {text}
      </pre>
      <button
        type="button"
        className="icon-btn"
        title="Copy"
        style={{ position: "absolute", top: 6, right: 6 }}
        onClick={() => {
          void navigator.clipboard?.writeText(text);
          setCopied(true);
          setTimeout(() => setCopied(false), 1400);
        }}
      >
        <Icon name={copied ? "check" : "copy"} size={13} />
      </button>
    </div>
  );
}

function InstallFromTerminal({ releases }: { releases: Release[] }) {
  const { data: link, refresh } = useQuery(() => releaseLink(), []);
  // Renewed well inside the link's life, so a page left open still copies
  // one that works.
  useEffect(() => {
    const id = setInterval(refresh, 10 * 60 * 1000);
    return () => clearInterval(id);
  }, [refresh]);

  const platforms = (["darwin", "linux", "windows"] as Platform[]).filter((p) =>
    releases.some((r) => r.os === p),
  );
  const [platform, setPlatform] = useState<Platform>(thisPlatform);
  const shown = platforms.includes(platform) ? platform : platforms[0];
  if (!shown) return null;

  return (
    <Card title="Install from a terminal">
      <Tabs
        value={shown}
        options={platforms.map((p) => ({ value: p, label: PLATFORM_LABELS[p] }))}
        onChange={(v) => setPlatform(v as Platform)}
      />
      <div style={{ marginTop: 12 }}>
        {link ? (
          <CopyBlock text={installScript(shown, releases, window.location.origin, link.token)} />
        ) : (
          <Loading rows={1} height={110} />
        )}
      </div>
      <p className="small muted" style={{ marginTop: 8 }}>
        {shown === "windows"
          ? "Paste into PowerShell. Installs to your user profile and adds it to your PATH; no admin rights needed."
          : `Paste into ${shown === "darwin" ? "Terminal" : "a shell"}. Installs to ~/.local/bin and adds it to your PATH; no sudo needed.`}{" "}
        The link in it works for 15 minutes.
      </p>
    </Card>
  );
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

      {releases && releases.length > 0 && (
        <div style={{ marginTop: 16 }}>
          <InstallFromTerminal releases={releases} />
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
