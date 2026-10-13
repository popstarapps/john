import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { getOrg, parseDomains, updateOrg } from "../../api/client";
import { useAction, useQuery } from "../../lib/useQuery";
import { Badge, Card, CopyLine, Field, KV, Loading, PageHead, Row, Segmented, Tabs } from "../../components/ui";
import { useToast } from "../../components/overlays";
import { Icon } from "../../components/Icon";
import { bytes, date, dateTime } from "../../lib/format";
import type { AttachmentPolicy, BuildNetwork, OrgSettings } from "../../api/types";

const TABS = [
  { value: "identity", label: "Identity" },
  { value: "log", label: "Traces" },
  { value: "sandboxes", label: "Sandboxes" },
  { value: "prompts", label: "Prompts" },
  { value: "backend", label: "Backend" },
];

export default function Settings() {
  const { tab = "identity" } = useParams();
  const navigate = useNavigate();
  const toast = useToast();
  const { data: org } = useQuery(() => getOrg(), []);

  if (!org) return <Loading rows={6} height={44} />;

  const saved = () => toast({ tone: "allow", title: "Saved", body: "The change is versioned and recorded." });

  return (
    <>
      <PageHead
        title="Settings"
        lede="There is no configuration file. Everything here is stored in the database, versioned, and written to the traces."
      />

      <Tabs value={tab} onChange={(v) => navigate(`/admin/settings/${v}`)} options={TABS} />

      {tab === "identity" && (
        <div className="grid grid-side">
          <SingleSignOn org={org} onSaved={saved} />

          <div className="stack">
            <MachineSessions org={org} onSaved={saved} />
            <Card title="Status">
              <KV>
                <Row k="Provider">{org.sso.provider}</Row>
                <Row k="Enforced">
                  {org.sso.enforced ? <Badge tone="allow">yes</Badge> : <Badge tone="deny">no</Badge>}
                </Row>
              </KV>
            </Card>
            <Card title="Redirect URI">
              <CopyLine text={`https://${org.backend.host}/auth/callback`} />
              <p className="small muted" style={{ marginTop: 8 }}>
                Register this with the provider. The CLI uses the same flow — <code>john login</code>{" "}
                opens it in the developer's browser.
              </p>
            </Card>
          </div>
        </div>
      )}

      {tab === "log" && (
        <div className="grid grid-side">
          <Card title="Encrypted session traces">
            <KV>
              <Row k="Public key fingerprint">
                <code className="small">{org.traceKey.fingerprint}</code>
              </Row>
              <Row k="Algorithm">{org.traceKey.algorithm}</Row>
              <Row k="Created">{date(org.traceKey.createdAt)}</Row>
              <Row k="Private key custody">{org.traceKey.custodian}</Row>
              <Row k="Written to">
                <code className="small">{org.traceStore.location}</code>
                <div className="small muted">Local disk on the backend host</div>
              </Row>
              <Row k="Current size">{bytes(org.backend.traceSizeMb * 1024 * 1024)}</Row>
              <Row k="Retention">Kept forever — nothing expires them</Row>
            </KV>
            <hr />
            <TraceStore org={org} onSaved={saved} />
          </Card>

          <div className="stack">
            <div className="callout callout--deny">
              <strong>Rotating the trace key does not re-encrypt history.</strong> Records written
              under the old key still need the old private key to read. Keep every retired key for
              as long as the records it protects.
            </div>
            <Card title="Rotate trace key">
              <Field label="New public key (age recipient)">
                <textarea className="textarea input--mono" placeholder="age1…" />
              </Field>
              <button
                className="btn btn--danger"
                onClick={() =>
                  toast({ tone: "warn", title: "Not wired up yet", body: "Key rotation needs the backend API." })
                }
              >
                <Icon name="key" size={14} /> Rotate
              </button>
            </Card>
          </div>
        </div>
      )}

      {tab === "sandboxes" && (
        <div className="grid grid-side">
          <SandboxBuilds org={org} onSaved={saved} />
          <div className="stack">
            <div className="callout">
              <strong>A build is not a session.</strong> Everything a sandbox does once it is
              running goes through the proxy under the network policies you have written. This
              setting is only about the image build that happens before that — installing
              compilers, language runtimes and the project's own packages — which is the one step
              that has a reason to reach further than a session may.
            </div>
            <Card title="What a developer sees">
              <p className="small muted">
                <code>/policies</code> in the client names the setting in force, so a developer
                watching a build fetch nothing can tell that it is being policed rather than
                broken.
              </p>
            </Card>
          </div>
        </div>
      )}

      {tab === "prompts" && (
        <div className="grid grid-side">
          <PromptAttachments org={org} onSaved={saved} />
          <div className="stack">
            <div className="callout">
              <strong>A control that reads text cannot read a picture.</strong> Replacements
              rewrite what is on its way to a model, and routing keywords refuse to send until the
              session is narrowed — both by looking at the words. Neither can see inside a
              screenshot of the same screen, and nothing about that fails visibly: the file goes,
              the alias table is untouched, and the trace records a file rather than a leak.
            </div>
            <Card title="What is recorded">
              <p className="small muted">
                Every attachment is written to the session trace by name, type, size and SHA-256 —
                enough to say what reached a model and to recognise the same file again. The bytes
                themselves are not: a trace's job is to say what happened, not to hold a second
                copy of every screenshot anyone has pasted.
              </p>
            </Card>
          </div>
        </div>
      )}

      {tab === "backend" && (
        <Card title="Backend">
          <KV>
            <Row k="Organization">{org.name}</Row>
            <Row k="Version">{org.backend.version}</Row>
            <Row k="Started">{dateTime(org.backend.startedAt)}</Row>
            <Row k="Database">{bytes(org.backend.dbSizeMb * 1024 * 1024)}</Row>
            <Row k="Encrypted traces">{bytes(org.backend.traceSizeMb * 1024 * 1024)}</Row>
          </KV>
        </Card>
      )}
    </>
  );
}

const BUILD_NETWORKS: Array<{ value: BuildNetwork; label: string; hint: string }> = [
  {
    value: "direct",
    label: "Direct",
    hint: "Unrestricted, and not recorded. A build reaches whatever it needs — which is what makes a sandbox useful afterwards under a policy that allows no egress at all.",
  },
  {
    value: "proxied",
    label: "Proxied",
    hint: "Held to the same network policies as everything else, so a build cannot fetch what a session could not. A project whose packages are not covered by a policy will fail to build.",
  },
];

/**
 * Where a sandbox image build's traffic goes.
 *
 * It travels in the signed policy snapshot rather than being asked for at
 * build time, because it is a claim about what a machine may do and a machine
 * that could answer it for itself would be deciding whether the proxy sees its
 * build at all. Changing it moves the policy epoch, so live sessions are told
 * rather than finding out when their snapshot expires.
 */
function SandboxBuilds({ org, onSaved }: { org: OrgSettings; onSaved: () => void }) {
  const [where, setWhere] = useState<BuildNetwork>(org.builds.network);
  const [save, saving] = useAction(updateOrg);
  const chosen = BUILD_NETWORKS.find((o) => o.value === where);

  return (
    <Card title="Image builds" sub="Where the network traffic of a sandbox build goes">
      <Field label="Network" hint={chosen?.hint}>
        <Segmented
          value={where}
          options={BUILD_NETWORKS.map((o) => ({ value: o.value, label: o.label }))}
          onChange={setWhere}
          label="Where a build's traffic goes"
        />
      </Field>
      <button
        className="btn btn--primary"
        disabled={saving || where === org.builds.network}
        onClick={() => void save({ builds: { network: where } }).then(onSaved)}
      >
        Save image builds
      </button>
    </Card>
  );
}

const ATTACHMENT_POLICIES: Array<{ value: AttachmentPolicy; label: string; hint: string }> = [
  {
    value: "guarded",
    label: "Guarded",
    hint: "Allowed, except in a session whose content is under a text control — a replacement policy or a routing keyword. The default, and the only setting whose answer depends on what else you have written: an organization with no such policy has nothing for an image to walk around.",
  },
  {
    value: "on",
    label: "Always",
    hint: "Allowed everywhere. The text controls still run on the text; nothing reads what is inside the file.",
  },
  {
    value: "off",
    label: "Never",
    hint: "Refused everywhere. Prompts are text, and a developer who mentions an image is told so rather than left wondering why the model ignored it.",
  },
];

/**
 * What a prompt may carry besides text.
 *
 * It travels in the signed policy snapshot, and both ends ask it: the client
 * so that a developer is told before their file is read off the disk, and the
 * backend because it is the side that knows about routing keywords, which
 * never leave it. The backend's answer is the one that decides.
 */
function PromptAttachments({ org, onSaved }: { org: OrgSettings; onSaved: () => void }) {
  const [mode, setMode] = useState<AttachmentPolicy>(org.prompts.attachments);
  const [save, saving] = useAction(updateOrg);
  const chosen = ATTACHMENT_POLICIES.find((o) => o.value === mode);

  return (
    <Card title="Attachments" sub="Whether a prompt may carry an image or a PDF">
      <Field label="Files on a prompt" hint={chosen?.hint}>
        <Segmented
          value={mode}
          options={ATTACHMENT_POLICIES.map((o) => ({ value: o.value, label: o.label }))}
          onChange={setMode}
          label="Whether a prompt may carry a file"
        />
      </Field>
      <button
        className="btn btn--primary"
        disabled={saving || mode === org.prompts.attachments}
        onClick={() => void save({ prompts: { attachments: mode } }).then(onSaved)}
      >
        Save attachments
      </button>
    </Card>
  );
}

/**
 * How long a machine may go without seeing the identity provider again.
 *
 * A developer's machine renews its own credentials in the background, so
 * without this it would keep working for as long as nobody thought to revoke
 * it — including after the person had left. The cap is what makes the
 * identity provider re-confirm them, and it is the only reason anyone here
 * has to sign in a second time, so it is stated in days and it is honest
 * about what zero means.
 */
function SingleSignOn({ org, onSaved }: { org: OrgSettings; onSaved: () => void }) {
  const [issuer, setIssuer] = useState(org.sso.issuer);
  const [clientId, setClientId] = useState(org.sso.clientId);
  const [secret, setSecret] = useState("");
  const [domains, setDomains] = useState(org.sso.allowedDomains.join(", "));
  const [error, setError] = useState<string | null>(null);
  const [save, saving] = useAction(updateOrg);

  const submit = () => {
    setError(null);
    void save({
      sso: {
        issuer,
        clientId,
        ...(secret ? { clientSecret: secret } : {}),
        allowedDomains: parseDomains(domains),
      },
    })
      .then(() => {
        setSecret("");
        onSaved();
      })
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  };

  return (
    <Card title="Single sign-on" sub={`${org.sso.provider} · enforced`}>
      {org.standalone && (
        <div className="callout callout--info" style={{ marginBottom: 14 }}>
          In standalone, john signs you in as your local account, so there is nothing to set up
          here. An organization connects its OIDC provider on this page.
        </div>
      )}
      <Field label="OIDC issuer">
        <input
          className="input input--mono"
          value={issuer}
          readOnly={org.standalone}
          onChange={(e) => setIssuer(e.target.value)}
        />
      </Field>
      <div className="field-row">
        <Field label="Client ID">
          <input
            className="input input--mono"
            value={clientId}
            readOnly={org.standalone}
            onChange={(e) => setClientId(e.target.value)}
          />
        </Field>
        <Field label="Client secret" hint="Write-only. Leave blank to keep the current one.">
          <input
            className="input input--mono"
            type="password"
            placeholder="••••••••••••"
            value={secret}
            readOnly={org.standalone}
            onChange={(e) => setSecret(e.target.value)}
          />
        </Field>
      </div>
      {!org.standalone && (
        <Field
          label="Allowed email domains"
          error={error}
          hint={
            parseDomains(domains).length === 0
              ? "Blank admits anyone the identity provider vouches for. Name your domains to hold sign-in to them."
              : "Only these can sign in, whatever else the identity provider lets through. With Google, only accounts of the Workspace that owns the domain."
          }
        >
          <input
            className="input input--mono"
            value={domains}
            placeholder="acme.example, acme.dk"
            onChange={(e) => setDomains(e.target.value)}
          />
        </Field>
      )}
      {!org.standalone && (
        <button className="btn btn--primary" disabled={saving} onClick={submit}>
          Save identity settings
        </button>
      )}
    </Card>
  );
}

/**
 * Where the backend writes session traces from now on.
 *
 * The backend checks it can write there before it saves, as setup does, so a
 * typo is refused here rather than losing the next trace. Moving it moves
 * nothing already written: older traces stay where they are, and whoever
 * reads them needs both places.
 */
function TraceStore({ org, onSaved }: { org: OrgSettings; onSaved: () => void }) {
  const [where, setWhere] = useState(org.traceStore.location);
  const [error, setError] = useState<string | null>(null);
  const [save, saving] = useAction(updateOrg);

  const submit = () => {
    setError(null);
    void save({ traceStore: { location: where } })
      .then(onSaved)
      .catch((e: unknown) => setError(e instanceof Error ? e.message : String(e)));
  };

  return (
    <>
      <Field
        label="Where traces are written"
        error={error}
        hint="A directory on the backend host. Traces already written stay where they are."
      >
        <input
          className="input input--mono"
          value={where}
          onChange={(e) => setWhere(e.target.value)}
        />
      </Field>
      <button
        className="btn btn--primary"
        disabled={saving || where.trim() === "" || where === org.traceStore.location}
        onClick={submit}
      >
        Save trace store
      </button>
    </>
  );
}

function MachineSessions({ org, onSaved }: { org: OrgSettings; onSaved: () => void }) {
  const [days, setDays] = useState(String(org.machineSessions.maxDays));
  const [save, saving] = useAction(updateOrg);
  const n = Number(days);
  const valid = Number.isInteger(n) && n >= 0;

  return (
    <Card title="Machine sessions" sub="How long a signed-in machine keeps working on its own">
      <Field
        label="Maximum lifetime (days)"
        hint={
          valid && n === 0
            ? "No limit: a machine renews indefinitely until someone revokes it."
            : "After this, the developer signs in through the identity provider again."
        }
      >
        <input
          className="input input--mono"
          inputMode="numeric"
          value={days}
          onChange={(e) => setDays(e.target.value)}
        />
      </Field>
      <button
        className="btn btn--primary"
        disabled={saving || !valid || n === org.machineSessions.maxDays}
        onClick={() => void save({ machineSessions: { maxDays: n } }).then(onSaved)}
      >
        Save machine sessions
      </button>
    </Card>
  );
}
