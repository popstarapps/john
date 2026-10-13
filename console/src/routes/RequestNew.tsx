import { useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { createNetworkAccessRequest, getNetworkRequest, getViewer, groupsForUser } from "../api/client";
import { useAction, useQuery } from "../lib/useQuery";
import { Card, Field, PageHead } from "../components/ui";
import { useToast } from "../components/overlays";
import { SpecEditor, emptySpec, specValid } from "../components/PolicyEditor";
import { Icon } from "../components/Icon";
import type { NetworkSpec } from "../api/types";

/* Asking for network access.
 *
 * Network is the only thing a developer can ask for. Everything else John
 * enforces — what may be redacted, which models a team gets, what the sandbox
 * is allowed to spend — is an organizational decision that starts with an
 * administrator, not a request form. */
export default function RequestNew() {
  const navigate = useNavigate();
  const toast = useToast();
  const [params] = useSearchParams();
  const fromId = params.get("from");

  const { data: viewer } = useQuery(() => getViewer(), []);
  const { data: source } = useQuery(
    () => (fromId ? getNetworkRequest(fromId) : Promise.resolve(undefined)),
    [fromId],
  );
  const { data: groups } = useQuery(
    () => (viewer ? groupsForUser(viewer.user.id) : Promise.resolve([])),
    [viewer?.user.id],
  );

  const [spec, setSpec] = useState<NetworkSpec>(() => emptySpec("network") as NetworkSpec);
  const [title, setTitle] = useState("");
  const [titleTouched, setTitleTouched] = useState(false);
  const [groupId, setGroupId] = useState<string>("");
  const [justification, setJustification] = useState("");
  const [submitted, setSubmitted] = useState(false);

  // Coming from a request the proxy blocked, the form should already describe
  // that request — that is the whole point of asking from there.
  //
  // The path is widened to its parent and the query is left alone. Pinning the
  // parameters the blocked request happened to carry — a page number, a cache
  // buster, a version — would produce a rule that matches nothing at all,
  // which is not restrictive, just broken. Narrow it by hand if the rule
  // genuinely turns on one.
  useEffect(() => {
    if (!source) return;
    setSpec((cur) => ({
      ...cur,
      methods: [source.method],
      host: source.host,
      path: source.path.replace(/[^/]*$/, "*"),
      decision:
        source.method === "GET" || source.method === "HEAD" ? "always-allow" : "ask-developer",
    }));
  }, [source]);

  // A write method can carry a body out; a read one has no reason to.
  const proposed: NetworkSpec = useMemo(
    () => ({
      ...spec,
      maxBodyBytes: spec.methods.some((m) => !["GET", "HEAD"].includes(m)) ? 65_536 : 0,
    }),
    [spec],
  );

  const suggestedTitle = spec.host
    ? `Allow ${spec.methods.join("/")} ${spec.host}${spec.path}`
    : "";
  const effectiveTitle = titleTouched ? title : suggestedTitle;

  const errors = {
    title: !effectiveTitle ? "Give the request a one-line title." : null,
    justification:
      justification.trim().length < 20
        ? "Say why you need this — the administrator deciding it has no other context."
        : null,
  };
  const valid = Object.values(errors).every((e) => e === null) && specValid(spec);

  const [submit, submitting] = useAction(async () => {
    const created = await createNetworkAccessRequest({
      title: effectiveTitle,
      justification,
      teamId: viewer!.user.teamIds[0],
      groupId: groupId || null,
      groupName: groups?.find((g) => g.id === groupId)?.name ?? "New group",
      spec: proposed,
      evidenceRequestIds: source ? [source.id] : [],
    });
    toast({
      tone: "allow",
      title: `${created.ref} submitted`,
      body: "You'll see the decision here and by email.",
    });
    navigate(`/requests/${created.id}`);
  });

  return (
    <>
      <PageHead
        crumbs={[{ label: "My requests", to: "/requests" }, { label: "New" }]}
        title="Request network access"
        lede="Describe the requests the sandbox needs to make. What you submit is the policy an administrator will review, edit and attach to a group."
      />

      {source && (
        <div className="callout callout--warn" style={{ marginBottom: 18 }}>
          Prefilled from a {source.status === "unknown" ? "blocked" : "held"} request:{" "}
          <code>
            {source.method} {source.host}
            {source.path}
          </code>
          .
        </div>
      )}

      <div className="single">
        <Card>
          <SpecEditor
            spec={spec}
            onChange={(next) => setSpec(next as NetworkSpec)}
            showErrors={submitted}
          />

          <hr />

          <Field label="Title" error={submitted ? errors.title : null}>
            <input
              className="input"
              value={effectiveTitle}
              onChange={(e) => {
                setTitleTouched(true);
                setTitle(e.target.value);
              }}
              placeholder="One line an administrator can decide from"
            />
          </Field>

          <Field
            label="Suggested group"
            hint="Groups you already have. Leave unset if none of them fit — an admin will place it."
          >
            <select className="select" value={groupId} onChange={(e) => setGroupId(e.target.value)}>
              <option value="">No suggestion</option>
              {(groups ?? []).map((g) => (
                <option key={g.id} value={g.id}>
                  {g.name}
                  {g.baseline ? " (baseline)" : ""}
                </option>
              ))}
            </select>
          </Field>

          <Field label="Justification" error={submitted ? errors.justification : null}>
            <textarea
              className="textarea"
              value={justification}
              placeholder="What are you doing, and why does it need this exact access?"
              onChange={(e) => setJustification(e.target.value)}
            />
          </Field>

          <div className="row" style={{ gap: 8, marginTop: 4 }}>
            <button
              className="btn btn--primary"
              disabled={submitting}
              onClick={() => {
                setSubmitted(true);
                if (valid) void submit();
              }}
            >
              <Icon name="check" size={14} /> {submitting ? "Submitting…" : "Submit request"}
            </button>
            <button className="btn" onClick={() => navigate("/requests")}>
              Cancel
            </button>
          </div>
        </Card>
      </div>
    </>
  );
}
