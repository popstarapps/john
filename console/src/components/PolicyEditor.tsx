import { useState } from "react";
import { Badge, Field, KV, Row, Tag } from "./ui";
import { ConfirmDialog, Drawer, Modal, useToast } from "./overlays";
import { Icon } from "./Icon";
import { PolicyTypeTag, policyTypeLabel } from "./domain";
import { createPolicy, deletePolicy, listModels, updatePolicy } from "../api/client";
import { useAction, useQuery } from "../lib/useQuery";
import { useDirectory } from "../lib/directory";
import { dateTime } from "../lib/format";
import type { Model, NetworkAttributeMatch, Policy, PolicySpec, PolicyType } from "../api/types";
import { providerLabel } from "./ModelDialog";

/* Editing a policy's matcher.
 *
 * One editor, four callers: the developer asking for network access, the
 * administrator adding a policy to a group, the administrator amending a
 * request before approving it, and the administrator changing a policy that
 * is already in force. A rule should look and behave the same wherever
 * it is written, and there is only one place to change when a matcher grows a
 * field. */

export const METHODS = ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"];

export function emptySpec(type: PolicyType): PolicySpec {
  switch (type) {
    case "network":
      return {
        kind: "network",
        methods: ["GET"],
        scheme: "https",
        host: "",
        port: 443,
        path: "/*",
        headerMatches: [],
        queryMatches: [],
        decision: "always-allow",
        maxBodyBytes: 0,
      };
    case "replacement":
      return { kind: "replacement", pattern: "", replacement: "", example: null };
    case "model-routing":
      return { kind: "model-routing", mode: "assign", modelIds: [], keywords: [] };
    case "workspace":
      return { kind: "workspace", paths: [], mode: "exclude" };
  }
}

/** What a filled-in spec is missing, keyed the way `Field` wants it. */
export function specErrors(spec: PolicySpec): Record<string, string | null> {
  switch (spec.kind) {
    case "network":
      return {
        methods: spec.methods.length === 0 ? "Pick at least one method." : null,
        host: !spec.host.trim() ? "A host is required." : null,
        headerMatches: spec.headerMatches.some((m) => !m.name.trim() || !m.value.trim())
          ? "Complete or remove every header match."
          : null,
        queryMatches: spec.queryMatches.some((m) => !m.name.trim() || !m.value.trim())
          ? "Complete or remove every query match."
          : null,
      };
    case "replacement":
      return {
        pattern: !spec.pattern.trim() ? "A pattern is required." : null,
        replacement: !spec.replacement.trim() ? "A replacement is required." : null,
      };
    case "model-routing":
      return {
        models: spec.modelIds.length === 0 ? "Choose at least one model." : null,
        keywords:
          spec.mode === "restrict" && spec.keywords.length === 0
            ? "Without a keyword this restriction applies to every session — which is a decision worth making on purpose."
            : null,
      };
    case "workspace":
      return { paths: spec.paths.length === 0 ? "At least one path is required." : null };
  }
}

export const specValid = (spec: PolicySpec) =>
  Object.values(specErrors(spec)).every((e) => e === null);

/* Keyword packs.
 *
 * Deliberately not classifiers. A pack drops a handful of ordinary keywords
 * into the box, where the administrator can read them, cut them and add their
 * own. Nothing here is a model, nothing here needs maintaining, and the rule
 * behaves identically on every machine — the same reason every other policy
 * decision in John is made outside the model. */
const KEYWORD_PACKS: Array<{ label: string; keywords: string[] }> = [
  { label: "C / C++", keywords: ["#include <", "std::", "nullptr", "template<", ".cpp", ".hpp"] },
  { label: "Terraform", keywords: ["terraform {", 'resource "', 'provider "', ".tf"] },
  { label: "Kubernetes", keywords: ["apiVersion:", "kind: Deployment", "kubectl"] },
  { label: "Cardholder data", keywords: ["cardholder", "card_number", "pan_", "PAN "] },
  { label: "Keys and certs", keywords: ["-----BEGIN", "PRIVATE KEY", "client_secret"] },
];

const lines = (v: string) =>
  v
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);

function AttributeMatches({
  label,
  namePlaceholder,
  value,
  onChange,
  hint,
  error,
}: {
  label: string;
  namePlaceholder: string;
  value: NetworkAttributeMatch[];
  onChange: (next: NetworkAttributeMatch[]) => void;
  hint: string;
  error?: string | null;
}) {
  return (
    <Field label={label} hint={hint} error={error}>
      <div className="stack" style={{ gap: 7 }}>
        {value.map((match, index) => (
          <div className="row" style={{ gap: 7 }} key={index}>
            <input
              className="input input--mono"
              value={match.name}
              placeholder={namePlaceholder}
              aria-label={`${label} name ${index + 1}`}
              onChange={(e) =>
                onChange(value.map((item, i) => (i === index ? { ...item, name: e.target.value } : item)))
              }
            />
            <input
              className="input input--mono"
              value={match.value}
              placeholder="value or wildcard*"
              aria-label={`${label} value ${index + 1}`}
              onChange={(e) =>
                onChange(value.map((item, i) => (i === index ? { ...item, value: e.target.value } : item)))
              }
            />
            <button
              type="button"
              className="btn btn--sm"
              aria-label={`Remove ${label.toLowerCase()} ${index + 1}`}
              onClick={() => onChange(value.filter((_, i) => i !== index))}
            >
              Remove
            </button>
          </div>
        ))}
        <button
          type="button"
          className="btn btn--sm"
          style={{ alignSelf: "flex-start" }}
          onClick={() => onChange([...value, { name: "", value: "" }])}
        >
          Add match
        </button>
      </div>
    </Field>
  );
}

export function SpecEditor({
  spec,
  onChange,
  models,
  showErrors,
  advanced,
}: {
  spec: PolicySpec;
  onChange: (next: PolicySpec) => void;
  models?: Model[];
  showErrors?: boolean;
  /** The fields an administrator gets and a requester does not. */
  advanced?: boolean;
}) {
  const err = showErrors ? specErrors(spec) : {};

  switch (spec.kind) {
    case "network":
      return (
        <>
          <Field label="Methods" error={err.methods}>
            <div className="row row-wrap" style={{ gap: 6 }}>
              {METHODS.map((m) => (
                <button
                  key={m}
                  type="button"
                  className={spec.methods.includes(m) ? "btn btn--sm btn--primary" : "btn btn--sm"}
                  onClick={() =>
                    onChange({
                      ...spec,
                      methods: spec.methods.includes(m)
                        ? spec.methods.filter((x) => x !== m)
                        : [...spec.methods, m],
                    })
                  }
                >
                  {m}
                </button>
              ))}
            </div>
          </Field>

          <div className="field-row">
            <Field label="Host" error={err.host}>
              <input
                className="input input--mono"
                value={spec.host}
                placeholder="api.internal.example"
                onChange={(e) => onChange({ ...spec, host: e.target.value })}
              />
            </Field>
            <Field label="Path" hint="Wildcards allowed. Keep it as narrow as the work needs.">
              <input
                className="input input--mono"
                value={spec.path}
                onChange={(e) => onChange({ ...spec, path: e.target.value })}
              />
            </Field>
          </div>

          <div className="field-row">
            <AttributeMatches
              label="Header matches"
              namePlaceholder="content-type"
              value={spec.headerMatches}
              onChange={(headerMatches) => onChange({ ...spec, headerMatches })}
              hint="Names are case-insensitive. Values may contain * wildcards."
              error={err.headerMatches}
            />
            <AttributeMatches
              label="Query matches"
              namePlaceholder="per_page"
              value={spec.queryMatches}
              onChange={(queryMatches) => onChange({ ...spec, queryMatches })}
              hint="Match selected query parameters by name and value."
              error={err.queryMatches}
            />
          </div>

          {advanced && (
            <div className="field-row">
              <Field label="Scheme">
                <select
                  className="select"
                  value={spec.scheme}
                  onChange={(e) =>
                    onChange({ ...spec, scheme: e.target.value as typeof spec.scheme })
                  }
                >
                  <option value="https">https</option>
                  <option value="http">http</option>
                  <option value="any">any</option>
                </select>
              </Field>
              <Field label="Port" hint="Blank for any port.">
                <input
                  className="input"
                  type="number"
                  value={spec.port ?? ""}
                  onChange={(e) =>
                    onChange({ ...spec, port: e.target.value ? Number(e.target.value) : null })
                  }
                />
              </Field>
            </div>
          )}

          <Field
            label="Decision"
            hint={
              spec.decision === "always-allow"
                ? "Matching requests are forwarded without interrupting the developer."
                : spec.decision === "ask-developer"
                  ? "The buffered request is shown, and the developer decides each time."
                  : "Matching requests are refused outright."
            }
          >
            <select
              className="select"
              value={spec.decision}
              onChange={(e) =>
                onChange({ ...spec, decision: e.target.value as typeof spec.decision })
              }
            >
              <option value="always-allow">Always allow</option>
              <option value="ask-developer">Ask developer</option>
              <option value="deny">Deny</option>
            </select>
          </Field>

          {advanced && (
            <div className="field-row">
              <Field label="Max body (bytes)" hint="0 permits no body at all.">
                <input
                  className="input"
                  type="number"
                  value={spec.maxBodyBytes ?? ""}
                  placeholder="unrestricted"
                  onChange={(e) =>
                    onChange({
                      ...spec,
                      maxBodyBytes: e.target.value === "" ? null : Number(e.target.value),
                    })
                  }
                />
              </Field>
            </div>
          )}
        </>
      );

    case "replacement":
      return (
        <>
          <div className="field-row">
            <Field label="Pattern" error={err.pattern} hint="RE2 regular expression.">
              <input
                className="input input--mono"
                value={spec.pattern}
                placeholder="NW-[0-9]{9}"
                onChange={(e) => onChange({ ...spec, pattern: e.target.value })}
              />
            </Field>
            <Field
              label="Becomes"
              error={err.replacement}
              hint={
                <>
                  Put a <code>#</code> where the value should be. Each distinct match gets its own
                  alias, and the same value always gets the same one — so the model still sees two
                  mentions of one account as one account.
                </>
              }
            >
              <input
                className="input input--mono"
                value={spec.replacement}
                placeholder="NW-00000#"
                onChange={(e) => onChange({ ...spec, replacement: e.target.value })}
              />
            </Field>
          </div>
          <div className="callout">
            Applies to everything on its way to a model — prompts, file contents, command output and
            diffs alike. There is nowhere to opt a replacement out of, by design.
          </div>
        </>
      );

    case "model-routing":
      return (
        <>
          <Field label="What this policy does">
            <div className="col" style={{ gap: 8 }}>
              <label className={spec.mode === "assign" ? "check check--on" : "check"}>
                <input
                  type="radio"
                  checked={spec.mode === "assign"}
                  onChange={() => onChange({ ...spec, mode: "assign", keywords: [] })}
                />
                <span>
                  Give teams these models
                  <span className="hint">
                    Everyone in a team this group is attached to may use them.
                  </span>
                </span>
              </label>
              <label className={spec.mode === "restrict" ? "check check--on" : "check"}>
                <input
                  type="radio"
                  checked={spec.mode === "restrict"}
                  onChange={() => onChange({ ...spec, mode: "restrict" })}
                />
                <span>
                  Narrow the session when something is seen
                  <span className="hint">
                    A keyword anywhere in the session — prompt, file, command output, diff — leaves
                    only these models, and the developer picks one to continue on.
                  </span>
                </span>
              </label>
            </div>
          </Field>

          {spec.mode === "restrict" && (
            <Field
              label="Keywords"
              error={err.keywords}
              hint="One per line, matched case-insensitively as plain text. A session never un-narrows: once it has been seen, it has been seen."
            >
              <textarea
                className="textarea input--mono"
                value={spec.keywords.join("\n")}
                placeholder={"#include <\nstd::"}
                onChange={(e) => onChange({ ...spec, keywords: lines(e.target.value) })}
              />
              <div className="row row-wrap" style={{ gap: 6, marginTop: 8 }}>
                <span className="small muted">Start from:</span>
                {KEYWORD_PACKS.map((pack) => (
                  <button
                    key={pack.label}
                    type="button"
                    className="btn btn--sm"
                    onClick={() =>
                      onChange({
                        ...spec,
                        keywords: [...new Set([...spec.keywords, ...pack.keywords])],
                      })
                    }
                  >
                    + {pack.label}
                  </button>
                ))}
              </div>
            </Field>
          )}

          <Field
            label={spec.mode === "assign" ? "Models" : "May continue on"}
            error={err.models}
          >
            <div className="col" style={{ gap: 6 }}>
              {(models ?? []).map((m) => (
                <label
                  key={m.id}
                  className={spec.modelIds.includes(m.id) ? "check check--on" : "check"}
                >
                  <input
                    type="checkbox"
                    checked={spec.modelIds.includes(m.id)}
                    onChange={() =>
                      onChange({
                        ...spec,
                        modelIds: spec.modelIds.includes(m.id)
                          ? spec.modelIds.filter((x) => x !== m.id)
                          : [...spec.modelIds, m.id],
                      })
                    }
                  />
                  <span>
                    {m.name}
                    <span className="hint">
                      {providerLabel(m.provider)} ·{" "}
                      {m.hosting === "internal" ? "internally hosted" : "external provider"}
                    </span>
                  </span>
                </label>
              ))}
            </div>
          </Field>

          {spec.mode === "restrict" && spec.keywords.length > 0 && (
            <div className="callout callout--warn">
              A session that sees{" "}
              {spec.keywords.slice(0, 3).map((k) => (
                <Tag key={k}>{k}</Tag>
              ))}
              {spec.keywords.length > 3 && ` and ${spec.keywords.length - 3} more`} continues on{" "}
              {spec.modelIds.length === 0
                ? "nothing — pick at least one model, or the session cannot continue at all"
                : spec.modelIds
                    .map((id) => models?.find((m) => m.id === id)?.name ?? id)
                    .join(", ")}
              .
            </div>
          )}
        </>
      );

    case "workspace":
      return (
        <>
          <Field
            label="Paths"
            error={err.paths}
            hint="One glob per line, relative to the project root."
          >
            <textarea
              className="textarea input--mono"
              value={spec.paths.join("\n")}
              placeholder={"fixtures/pii/**\n**/*.key"}
              onChange={(e) => onChange({ ...spec, paths: lines(e.target.value) })}
            />
          </Field>
          <Field label="Treatment">
            <select
              className="select"
              value={spec.mode}
              onChange={(e) => onChange({ ...spec, mode: e.target.value as typeof spec.mode })}
            >
              <option value="exclude">Exclude from the sandbox entirely</option>
              <option value="read-only">Mount read-only</option>
              <option value="no-delete">Prevent deletion</option>
            </select>
          </Field>
        </>
      );
  }
}

const TYPES: PolicyType[] = ["network", "workspace", "model-routing", "replacement"];

/** Adding a policy to a group. */
export function AddPolicyDialog({
  groupId,
  groupName,
  onClose,
}: {
  groupId: string;
  groupName: string;
  onClose: () => void;
}) {
  const toast = useToast();
  const { data: models } = useQuery(() => listModels(), []);
  const [type, setType] = useState<PolicyType>("network");
  const [spec, setSpec] = useState<PolicySpec>(() => emptySpec("network"));
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [submitted, setSubmitted] = useState(false);
  const [save, saving] = useAction(createPolicy);

  const valid = name.trim().length > 0 && specValid(spec);

  return (
    <Modal
      title="Add a policy"
      sub={`Into ${groupName}. The change is versioned and recorded.`}
      wide
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            className="btn btn--primary"
            disabled={saving}
            onClick={async () => {
              setSubmitted(true);
              if (!valid) return;
              await save({ groupId, name: name.trim(), description: description.trim(), spec });
              onClose();
              toast({
                tone: "allow",
                title: `${name.trim()} added`,
                body: "Clients pick this up within the policy cache TTL.",
              });
            }}
          >
            {saving ? "Adding…" : "Add policy"}
          </button>
        </>
      }
    >
      {/* Name and description first: they are the same two questions whatever
          the policy turns out to be. */}
      <Field
        label="Name"
        error={submitted && !name.trim() ? "Give the policy a name." : null}
        hint="What this rule is, in a few words."
      >
        <input
          className="input"
          value={name}
          placeholder={`${policyTypeLabel(type)} — …`}
          onChange={(e) => setName(e.target.value)}
        />
      </Field>
      <Field label="Description" hint="Why it exists. The next administrator will need it.">
        <textarea
          className="textarea"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </Field>

      <hr />

      <Field label="What kind of policy?">
        <div
          className="grid"
          style={{ gridTemplateColumns: "repeat(auto-fit,minmax(168px,1fr))", gap: 8 }}
        >
          {TYPES.map((t) => (
            <button
              key={t}
              type="button"
              className={t === type ? "check check--on" : "check"}
              style={{ textAlign: "left" }}
              onClick={() => {
                setType(t);
                setSpec(emptySpec(t));
              }}
            >
              <PolicyTypeTag type={t} />
            </button>
          ))}
        </div>
      </Field>

      <SpecEditor spec={spec} onChange={setSpec} models={models} showErrors={submitted} advanced />
    </Modal>
  );
}

/** Editing a policy that already exists — active or not. The kind is fixed:
 *  a rule of another kind is a different policy, added on its own. */
export function EditPolicyDrawer({
  policy,
  groupName,
  onClose,
}: {
  policy: Policy;
  groupName: string;
  onClose: () => void;
}) {
  const toast = useToast();
  const dir = useDirectory();
  const { data: models } = useQuery(() => listModels(), []);
  const [spec, setSpec] = useState<PolicySpec>(policy.spec);
  const [name, setName] = useState(policy.name);
  const [description, setDescription] = useState(policy.description);
  const [submitted, setSubmitted] = useState(false);
  const [save, saving] = useAction(updatePolicy);
  const [remove, removing] = useAction(deletePolicy);
  const [confirmingDelete, setConfirmingDelete] = useState(false);

  const valid = name.trim().length > 0 && specValid(spec);
  const changed =
    name.trim() !== policy.name ||
    description.trim() !== policy.description ||
    JSON.stringify(spec) !== JSON.stringify(policy.spec);

  return (
    <>
      <Drawer
        title={policy.name}
        sub={<PolicyTypeTag type={policy.type} plain />}
        onClose={onClose}
        footer={
          <>
            <button
              className="btn btn--danger"
              style={{ marginRight: "auto" }}
              disabled={saving || removing}
              onClick={() => setConfirmingDelete(true)}
            >
              <Icon name="trash" size={14} /> Delete
            </button>
            <button className="btn" onClick={onClose}>
              {changed ? "Discard" : "Close"}
            </button>
            <button
              className="btn btn--primary"
              disabled={saving || !changed}
              onClick={async () => {
                setSubmitted(true);
                if (!valid) return;
                try {
                  await save(policy.id, {
                    name: name.trim(),
                    description: description.trim(),
                    spec,
                    version: policy.version,
                  });
                } catch (err) {
                  toast({
                    tone: "deny",
                    title: `${policy.name} was not saved`,
                    body: err instanceof Error ? err.message : "The backend refused.",
                  });
                  return;
                }
                onClose();
                toast({
                  tone: "allow",
                  title: `${name.trim()} saved`,
                  body: policy.enabled
                    ? "It is in force as edited. Running sessions pick it up straight away."
                    : "It stays disabled until you enable it.",
                });
              }}
            >
              {saving ? "Saving…" : "Save changes"}
            </button>
          </>
        }
      >
        {policy.enabled && (
          <div className="callout callout--info" style={{ marginBottom: 16 }}>
            This policy is enabled. Saving changes what it does for everyone it applies to, straight away.
          </div>
        )}
        <Field label="Name" error={submitted && !name.trim() ? "Give the policy a name." : null}>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="Description" hint="Why it exists. The next administrator will need it.">
          <textarea className="textarea" value={description} onChange={(e) => setDescription(e.target.value)} />
        </Field>

        <hr />

        <SpecEditor spec={spec} onChange={setSpec} models={models} showErrors={submitted} advanced />

        <hr />

        <KV>
          <Row k="Group">{groupName}</Row>
          <Row k="Version">v{policy.version}</Row>
          <Row k="Updated">{dateTime(policy.updatedAt)}</Row>
          <Row k="Updated by">{dir.userName(policy.updatedBy)}</Row>
          <Row k="Status">
            {policy.enabled ? <Badge tone="allow">Enabled</Badge> : <Badge>Disabled</Badge>}
          </Row>
        </KV>
      </Drawer>

      {confirmingDelete && (
        <ConfirmDialog
          title={`Delete ${policy.name}?`}
          body={
            policy.enabled
              ? `It is in force now. Deleting it stops it applying to everyone in ${groupName} straight away, and it cannot be undone — disabling it keeps it around instead.`
              : "It cannot be undone. The change history keeps a record that it existed."
          }
          confirmLabel="Delete policy"
          danger
          busy={removing}
          onClose={() => setConfirmingDelete(false)}
          onConfirm={async () => {
            try {
              await remove(policy.id);
            } catch (err) {
              toast({
                tone: "deny",
                title: `${policy.name} was not deleted`,
                body: err instanceof Error ? err.message : "The backend refused.",
              });
              return;
            }
            onClose();
            toast({ tone: "warn", title: `${policy.name} deleted`, body: `Removed from ${groupName}.` });
          }}
        />
      )}
    </>
  );
}
