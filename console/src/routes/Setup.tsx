import { useState, type ReactNode } from "react";
import { Lockup } from "../components/Logo";
import { Icon } from "../components/Icon";
import { Badge, CopyLine, Field } from "../components/ui";
import { ModelDialog, providerLabel, type ModelDraft } from "../components/ModelDialog";
import { completeSetup, parseDomains } from "../api/client";
import { useAction } from "../lib/useQuery";

/* First run.
 *
 * The backend is started with --setup, prints a one-time URL containing a
 * token, and this is what that URL opens. Everything configured here is
 * written to the database; there is no configuration file.
 *
 * It asks for the things nothing works without — an identity provider, an
 * administrator, a model, somewhere to put the traces. Policies, teams and
 * groups are all configurable afterwards, and are not worth blocking a first
 * run on. */

interface State {
  orgName: string;
  issuer: string;
  clientId: string;
  clientSecret: string;
  adminEmail: string;
  allowedDomains: string;
  models: ModelDraft[];
  traceKey: string;
  tracePath: string;
}

type Setter = <K extends keyof State>(k: K, v: State[K]) => void;

interface Step {
  key: string;
  title: string;
  blurb: string;
  body: (s: State, set: Setter) => ReactNode;
}

function ModelsStep({ state, set }: { state: State; set: Setter }) {
  const [adding, setAdding] = useState(false);
  return (
    <>
      {state.models.length === 0 ? (
        <div className="callout" style={{ marginBottom: 14 }}>
          No models yet. John cannot answer a prompt until at least one is configured, but you can
          add the rest later.
        </div>
      ) : (
        <div className="col" style={{ gap: 8, marginBottom: 14 }}>
          {state.models.map((m, i) => (
            <div key={`${m.name}-${i}`} className="check check--on" style={{ cursor: "default" }}>
              <Icon name="model" size={15} />
              <span className="grow">
                {m.name}
                <span className="hint">
                  {providerLabel(m.provider)} ·{" "}
                  {m.hosting === "internal" ? "internally hosted" : "external"} ·{" "}
                  key stored
                </span>
              </span>
              <button
                className="icon-btn"
                aria-label={`Remove ${m.name}`}
                onClick={() =>
                  set(
                    "models",
                    state.models.filter((_, n) => n !== i),
                  )
                }
              >
                <Icon name="x" size={14} />
              </button>
            </div>
          ))}
        </div>
      )}

      <button className="btn btn--primary" onClick={() => setAdding(true)}>
        <Icon name="plus" size={14} /> Add a model
      </button>

      {adding && (
        <ModelDialog
          onClose={() => setAdding(false)}
          onSave={(draft) => {
            set("models", [...state.models, draft]);
            setAdding(false);
          }}
        />
      )}
    </>
  );
}

const STEPS: Step[] = [
  {
    key: "org",
    title: "Organization",
    blurb: "What this backend is called, and who it serves.",
    body: (s, set) => (
      <>
        <Field label="Organization name">
          <input
            className="input"
            value={s.orgName}
            placeholder="Northwind Financial"
            onChange={(e) => set("orgName", e.target.value)}
          />
        </Field>
        <div className="callout callout--info">
          You are seeing this because the backend was started with <code>--setup</code> and printed
          a one-time URL. Whoever can read that output is trusted to bootstrap — finish this in one
          sitting, and the token is spent.
        </div>
      </>
    ),
  },
  {
    key: "sso",
    title: "Single sign-on",
    blurb: "The only way anyone signs in. There are no local accounts.",
    body: (s, set) => (
      <>
        <Field label="OIDC issuer" hint="Discovery is read from /.well-known/openid-configuration.">
          <input
            className="input input--mono"
            value={s.issuer}
            placeholder="https://acme.okta.com/oauth2/default"
            onChange={(e) => set("issuer", e.target.value)}
          />
        </Field>
        <div className="field-row">
          <Field label="Client ID">
            <input
              className="input input--mono"
              value={s.clientId}
              onChange={(e) => set("clientId", e.target.value)}
            />
          </Field>
          <Field label="Client secret">
            <input
              className="input input--mono"
              type="password"
              value={s.clientSecret}
              onChange={(e) => set("clientSecret", e.target.value)}
            />
          </Field>
        </div>
        <Field
          label="Redirect URI"
          hint={
            window.location.protocol === "https:"
              ? "Register this with your provider."
              : "Register this with your provider, with the HTTPS hostname developers will reach john on. Setup opened over a tunnel or plain HTTP cannot tell what that is."
          }
        >
          <CopyLine
            text={
              window.location.protocol === "https:"
                ? `${window.location.origin}/auth/callback`
                : "https://<your john hostname>/auth/callback"
            }
          />
        </Field>
      </>
    ),
  },
  {
    key: "admin",
    title: "First administrator",
    blurb: "The account that will hold every authority until it delegates some.",
    body: (s, set) => {
      const own = s.adminEmail.includes("@") ? s.adminEmail.split("@").pop() : "";
      return (
        <>
          <Field
            label="Administrator email"
            hint="Must match a user in the identity provider above. They become the first administrator on first sign-in."
          >
            <input
              className="input"
              value={s.adminEmail}
              placeholder="you@acme.example"
              onChange={(e) => set("adminEmail", e.target.value)}
            />
          </Field>
          <Field
            label="Allowed email domains"
            hint={
              <>
                Only these can sign in, whatever else the identity provider lets through.
                {own ? (
                  <>
                    {" "}Leave blank for <code>{own}</code> alone.
                  </>
                ) : (
                  " Leave blank for the administrator's domain alone."
                )}
              </>
            }
          >
            <input
              className="input input--mono"
              value={s.allowedDomains}
              placeholder={own || "acme.example, acme.dk"}
              onChange={(e) => set("allowedDomains", e.target.value)}
            />
          </Field>
        </>
      );
    },
  },
  {
    key: "models",
    title: "Models",
    blurb: "What John is allowed to think with. You can add more at any time.",
    body: (s, set) => <ModelsStep state={s} set={set} />,
  },
  {
    key: "log",
    title: "Session traces",
    blurb:
      "John keeps a trace of every session, encrypted so that only the holder of the private key can read it back. Every organization has one — this step is about who holds it.",
    body: (s, set) => (
      <>
        <div className="callout callout--deny">
          A key has already been generated and printed in the terminal running{" "}
          <code>john backend --setup</code>. Save it now: it was not written to disk, it is not
          recoverable, and it is the only thing that can read your session traces.
        </div>

        <Field
          label="Public key"
          hint="Leave empty to use the key printed in the terminal. Paste a public key here to use one you generated yourself instead."
        >
          <textarea
            className="textarea input--mono"
            value={s.traceKey}
            placeholder="age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
            onChange={(e) => set("traceKey", e.target.value)}
          />
        </Field>

        <Field
          label="Bring your own key"
          hint="Run this somewhere the private key can live safely — not on this host. It prints the public recipient line to paste above."
        >
          <CopyLine text="age-keygen -o john-trace.key" />
        </Field>

        <Field
          label="Where traces are written"
          hint="A directory on persistent storage the backend can write to. Empty puts it beside the database."
        >
          <input
            className="input input--mono"
            value={s.tracePath}
            placeholder="traces/, beside the database"
            onChange={(e) => set("tracePath", e.target.value)}
          />
        </Field>

        <div className="callout callout--warn">
          Traces are kept forever, and there is no recovery path for the private key. Lose it and
          the traces stay encrypted for good — store it the way you store your other break-glass
          secrets. Developers keep their own sessions either way; only reading across them needs
          this key.
        </div>
      </>
    ),
  },
];

export default function Setup() {
  const [i, setI] = useState(0);
  const [done, setDone] = useState(false);
  const [state, setState] = useState<State>({
    orgName: "",
    issuer: "",
    clientId: "",
    clientSecret: "",
    adminEmail: "",
    allowedDomains: "",
    models: [],
    traceKey: "",
    tracePath: "",
  });
  const set: Setter = (k, v) => setState((cur) => ({ ...cur, [k]: v }));

  const [failed, setFailed] = useState<string | null>(null);
  const [finish, finishing] = useAction(async () => {
    setFailed(null);
    try {
      await completeSetup({ ...state, allowedDomains: parseDomains(state.allowedDomains) });
      setDone(true);
    } catch (e) {
      setFailed(e instanceof Error ? e.message : String(e));
    }
  });

  const step = STEPS[i];
  const last = i === STEPS.length - 1;

  if (done)
    return (
      <div className="plain-frame">
        <Lockup label="John" />
        <h1 className="sr-only">John first-run setup</h1>
        <div className="plain-card plain-card--narrow">
          <div className="card-body" style={{ padding: 28, textAlign: "center" }}>
            <span style={{ color: "var(--allow)" }}>
              <Icon name="shieldCheck" size={32} />
            </span>
            <h2 style={{ fontSize: 20, margin: "10px 0 8px" }}>Backend configured</h2>
            <p className="muted">
              Restart the backend without <code>--setup</code>. It will read this configuration from
              the database and enforce SSO for sign-in. The setup token is now spent.
            </p>
          </div>
        </div>
      </div>
    );

  return (
    <div className="plain-frame">
      <Lockup label="John" />
      {/* The wizard's visible headings are per step, so the page itself needs
          one for the document outline. */}
      <h1 className="sr-only">John first-run setup</h1>

      <div className="plain-card">
        <div className="card-head">
          <div className="grow">
            <span className="eyebrow">First-run setup</span>
            <h2 style={{ marginTop: 4 }}>{step.title}</h2>
            <div className="sub">{step.blurb}</div>
          </div>
          <Badge>
            Step {i + 1} of {STEPS.length}
          </Badge>
        </div>

        <div className="card-body">
          <div className="split">
            <div className="steps">
              {STEPS.map((s, n) => (
                <button
                  key={s.key}
                  className={`step ${n === i ? "step--on" : ""} ${n < i ? "step--done" : ""}`}
                  onClick={() => setI(n)}
                >
                  <span className="num">{n < i ? "✓" : n + 1}</span>
                  <span className="truncate">{s.title}</span>
                </button>
              ))}
            </div>
            <div>{step.body(state, set)}</div>
          </div>
        </div>

        {failed && (
          <div className="callout callout--deny" role="alert" style={{ margin: "0 20px 14px" }}>
            <strong>Setup was not saved.</strong> {failed}
          </div>
        )}

        <div className="card-foot">
          <button className="btn" disabled={i === 0} onClick={() => setI((n) => n - 1)}>
            <Icon name="chevronLeft" size={14} /> Back
          </button>
          {last ? (
            <button className="btn btn--allow" disabled={finishing} onClick={() => void finish()}>
              <Icon name="check" size={14} />
              {finishing ? "Writing configuration…" : "Finish setup"}
            </button>
          ) : (
            <button className="btn btn--primary" onClick={() => setI((n) => n + 1)}>
              Continue <Icon name="chevronRight" size={14} />
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
