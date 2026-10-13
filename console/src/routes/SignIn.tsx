import { useEffect, useState } from "react";

import { Lockup } from "../components/Logo";
import { Icon } from "../components/Icon";
import { getOrg } from "../api/client";
import { useQuery } from "../lib/useQuery";

/** The pre-console page. SSO is the only way in — there are no local accounts. */
export default function SignIn() {
  const [busy, setBusy] = useState(false);
  const { data: org } = useQuery(() => getOrg(), []);

  // Standalone's identity provider signs the local account in without asking,
  // so there is nothing to choose here: go straight through.
  useEffect(() => {
    if (org?.standalone) window.location.replace("/auth/login?next=/");
  }, [org]);
  if (org?.standalone) return null;

  return (
    <div className="plain-frame">
      <Lockup label="John" />

      <div className="plain-card plain-card--narrow">
        <div className="card-body" style={{ padding: 22 }}>
          <h1 style={{ fontSize: 21, marginBottom: 6 }}>Sign in</h1>
          <p className="small muted" style={{ marginBottom: 18 }}>
            {org ? `${org.name} · ${org.backend.host}` : "Loading…"}
          </p>

          <button
            className="btn btn--primary btn--wide"
            disabled={busy}
            onClick={() => {
              setBusy(true);
              // The backend is the OIDC client; the whole dance happens there.
              window.location.href = "/auth/login?next=/";
            }}
          >
            <Icon name="shieldCheck" size={15} />
            {busy ? "Redirecting…" : `Continue with ${org?.sso.provider ?? "SSO"}`}
          </button>

          <p className="small muted" style={{ marginTop: 14 }}>
            Single sign-on is enforced for this backend. Your identity comes from your OIDC
            provider; John holds your role, team membership and effective policies.
          </p>
        </div>
        <div className="card-foot">
          <span>Backend {org?.backend.version}</span>
          <span>Self-hosted</span>
        </div>
      </div>

      <p className="small muted" style={{ marginTop: 20, maxWidth: 420, textAlign: "center" }}>
        Signing in from the CLI instead? Run <code>john login</code> — it opens this same flow and
        stores a token for that machine.
      </p>
    </div>
  );
}
