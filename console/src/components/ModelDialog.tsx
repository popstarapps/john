import { useEffect, useMemo, useState } from "react";
import { Modal } from "./overlays";
import { Field } from "./ui";
import type { Model } from "../api/types";
import { listModelCatalog, type CatalogModel, type ModelTestResult } from "../api/client";
import { useQuery } from "../lib/useQuery";
import { compact, money } from "../lib/format";

/* Adding a model, and editing one.
 *
 * The same dialog serves first-run setup, the admin models screen, and an
 * edit of a model that already exists — the questions are identical, and
 * there is no reason for an operator to meet three different forms for one
 * task. Passing `model` fills the form in and turns it into an edit.
 *
 * A provider john knows by name asks three things: which provider, which of
 * its models, and the key. The models, their limits and their prices come
 * from agentkit's catalog, through the backend. Anything else — a gateway, a
 * self-hosted server, a provider john does not list — is "Other", which asks
 * for everything: the protocol it speaks, where it is, and what it costs. */

export interface ModelDraft {
  name: string;
  /**
   * A named provider (anthropic, openai, google), or for Other the protocol
   * the endpoint speaks (openai-compatible, anthropic-compatible,
   * gemini-compatible). Either way it picks the wire protocol, so it is the
   * identifier — `providerLabel` turns it back into something to read.
   */
  provider: string;
  hosting: "internal" | "external";
  /** Empty for a named provider: the backend knows where it is. */
  endpoint: string;
  /** The provider's own identifier, e.g. `claude-opus-5`. */
  modelId: string;
  /** Zero for a named provider's model: the catalog fills these in. */
  contextWindow: number;
  costPerMTokIn: number;
  costPerMTokCachedIn: number;
  costPerMTokOut: number;
  /**
   * Held by the backend, never returned to the console once saved. Empty on
   * an edit means "keep the stored credential" — the console has nothing to
   * send back but a new one.
   */
  apiKey: string;
  extra: Record<string, string>;
}

const NAMED = [
  { id: "anthropic", label: "Anthropic", keyPlaceholder: "sk-ant-…" },
  { id: "openai", label: "OpenAI", keyPlaceholder: "sk-…" },
  { id: "google", label: "Google Gemini", keyPlaceholder: "AIza…" },
];

const PROTOCOLS = [
  { id: "openai-compatible", label: "OpenAI-compatible (chat completions)", placeholder: "https://llm.example/v1" },
  { id: "anthropic-compatible", label: "Anthropic-compatible (messages)", placeholder: "https://llm.example" },
  { id: "gemini-compatible", label: "Gemini-compatible", placeholder: "https://llm.example" },
];

// Providers older consoles saved. They still work, as the protocol shown.
const LEGACY: Record<string, { label: string; protocol: string }> = {
  azure: { label: "Azure OpenAI", protocol: "openai-compatible" },
  bedrock: { label: "AWS Bedrock", protocol: "openai-compatible" },
  vertex: { label: "Google Vertex AI", protocol: "gemini-compatible" },
  selfhosted: { label: "Self-hosted (OpenAI-compatible)", protocol: "openai-compatible" },
};

const OTHER = "other";

/**
 * A provider id as something to read. An unrecognised value is shown as it
 * stands rather than replaced with a guess.
 */
export function providerLabel(provider: string): string {
  const key = provider.toLowerCase().trim();
  return (
    NAMED.find((p) => p.id === key)?.label ??
    PROTOCOLS.find((p) => p.id === key)?.label ??
    LEGACY[key]?.label ??
    provider
  );
}

/** One line about a catalog model: what it holds, what it costs, what it does. */
function describe(c: CatalogModel): string {
  const can = [c.reasoning && "reasoning", c.vision && "images", c.documents && "PDFs"].filter(Boolean);
  return [
    `${compact(c.contextWindow)} context`,
    `${money(c.costPerMTokIn, 2)} in / ${money(c.costPerMTokOut, 2)} out per Mtok`,
    can.length ? can.join(", ") : null,
  ]
    .filter(Boolean)
    .join(" · ");
}

export function ModelDialog({
  model,
  onClose,
  onSave,
  onTest,
  saving,
}: {
  /** The model being edited. Absent adds a new one. */
  model?: Model;
  onClose: () => void;
  onSave: (draft: ModelDraft) => void;
  /**
   * Tries the model with one short prompt before it is saved. Absent where
   * nothing can be reached yet — first-run setup, before there is a sign-in.
   */
  onTest?: (draft: ModelDraft) => Promise<ModelTestResult>;
  saving?: boolean;
}) {
  const editing = model !== undefined;
  const { data: catalog } = useQuery(() => listModelCatalog(), []);

  const saved = model?.provider.toLowerCase().trim() ?? "";
  const [choice, setChoice] = useState(
    !model ? NAMED[0].id : NAMED.some((p) => p.id === saved) ? saved : OTHER,
  );
  const named = choice !== OTHER;
  const [protocol, setProtocol] = useState(
    PROTOCOLS.some((p) => p.id === saved) ? saved : (LEGACY[saved]?.protocol ?? PROTOCOLS[0].id),
  );

  const [modelId, setModelId] = useState(model?.modelId ?? "");
  const [apiKey, setApiKey] = useState("");
  // Other only: everything a named provider gets from the catalog.
  const [name, setName] = useState(model?.name ?? "");
  const [endpoint, setEndpoint] = useState(model?.endpoint ?? "");
  const [hosting, setHosting] = useState<"internal" | "external">(model?.hosting ?? "external");
  const [context, setContext] = useState(model?.contextWindow ?? 128_000);
  const [costIn, setCostIn] = useState(model?.costPerMTokIn ?? 0);
  const [costCached, setCostCached] = useState(model?.costPerMTokCachedIn ?? 0);
  const [costOut, setCostOut] = useState(model?.costPerMTokOut ?? 0);

  const [submitted, setSubmitted] = useState(false);
  const [testing, setTesting] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  // A failure is about the settings it was tried with; changing one retires it.
  useEffect(() => setFailure(null), [choice, protocol, modelId, endpoint, apiKey]);

  const choices = useMemo(() => (catalog ?? []).filter((c) => c.provider === choice), [catalog, choice]);
  const chosen = choices.find((c) => c.id === modelId);

  // A named provider starts on its first model; the one being edited stays.
  useEffect(() => {
    if (named && !choices.some((c) => c.id === modelId) && !(editing && choice === saved)) {
      setModelId(choices[0]?.id ?? "");
    }
  }, [choices]); // on a change of list only: picking a model is not a reason to reset it

  function choose(next: string) {
    setChoice(next);
    if (next === OTHER) setModelId(editing && !NAMED.some((p) => p.id === saved) ? model.modelId : "");
  }

  const provider = NAMED.find((p) => p.id === choice);
  const errors = named
    ? {
        modelId: !modelId ? "Choose a model." : null,
        // On an edit the stored credential is already there; a blank field
        // means the administrator was changing something else.
        apiKey: !editing && !apiKey.trim() ? "The API key is required." : null,
      }
    : {
        modelId: !modelId.trim() ? "The model identifier is required." : null,
        endpoint: !/^https?:\/\/[^<>\s]+$/.test(endpoint.trim()) ? "An http(s) address is required." : null,
      };
  const valid = Object.values(errors).every((e) => e === null);

  const draft = (): ModelDraft =>
    named
      ? {
          // The catalog names it; an edit that keeps its model keeps its name.
          name: editing && modelId === model.modelId ? model.name : (chosen?.name ?? modelId),
          provider: choice,
          hosting: "external",
          endpoint: "",
          modelId,
          contextWindow: 0,
          costPerMTokIn: 0,
          costPerMTokCachedIn: 0,
          costPerMTokOut: 0,
          apiKey,
          extra: {},
        }
      : {
          name: name.trim() || modelId.trim(),
          provider: protocol,
          hosting,
          endpoint: endpoint.trim(),
          modelId: modelId.trim(),
          contextWindow: context,
          costPerMTokIn: costIn,
          costPerMTokCachedIn: costCached,
          costPerMTokOut: costOut,
          apiKey,
          extra: {},
        };

  // A model that does not answer is found here, where its settings are in
  // front of the person who typed them, rather than in the first session.
  async function testThenSave() {
    setSubmitted(true);
    if (!valid) return;
    const d = draft();
    if (onTest) {
      setTesting(true);
      let result: ModelTestResult;
      try {
        result = await onTest(d);
      } catch (err) {
        result = { ok: false, ms: 0, error: err instanceof Error ? err.message : "The test did not run." };
      } finally {
        setTesting(false);
      }
      if (!result.ok) {
        setFailure(result.error ?? "No answer.");
        return;
      }
    }
    onSave(d);
  }

  const keyField = (
    <Field
      label="API key"
      error={submitted ? (errors as { apiKey?: string | null }).apiKey : null}
      hint={editing ? "Leave blank to keep the key already stored." : named ? undefined : "If the endpoint needs one."}
    >
      <input
        className="input input--mono"
        type="password"
        autoComplete="off"
        value={apiKey}
        placeholder={editing ? "unchanged" : (provider?.keyPlaceholder ?? "optional")}
        onChange={(e) => setApiKey(e.target.value)}
      />
    </Field>
  );

  return (
    <Modal
      title={editing ? `Edit ${model.name}` : "Add a model"}
      sub="Credentials are stored by the backend and never returned to this console."
      wide={!named}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          {failure && (
            <button className="btn" disabled={saving} onClick={() => onSave(draft())}>
              Save anyway
            </button>
          )}
          <button className="btn btn--primary" disabled={saving || testing} onClick={testThenSave}>
            {testing
              ? "Testing…"
              : saving
                ? "Saving…"
                : failure
                  ? "Test again"
                  : editing
                    ? "Save changes"
                    : "Add model"}
          </button>
        </>
      }
    >
      <Field label="Provider">
        <select className="select" value={choice} onChange={(e) => choose(e.target.value)}>
          {NAMED.map((p) => (
            <option key={p.id} value={p.id}>
              {p.label}
            </option>
          ))}
          <option value={OTHER}>Other…</option>
        </select>
      </Field>

      {named ? (
        <>
          <Field
            label="Model"
            error={submitted ? errors.modelId : null}
            hint={chosen ? describe(chosen) : catalog ? undefined : "Loading models…"}
          >
            <select className="select" value={modelId} onChange={(e) => setModelId(e.target.value)}>
              {choices.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
              {/* A model saved before the catalog stopped listing it stays
                  selectable, so an edit does not silently change it. */}
              {modelId && !chosen && catalog && <option value={modelId}>{modelId}</option>}
            </select>
          </Field>
          {keyField}
        </>
      ) : (
        <>
          <Field label="Protocol" hint="What the endpoint speaks. Most gateways and self-hosted servers are OpenAI-compatible.">
            <select className="select" value={protocol} onChange={(e) => setProtocol(e.target.value)}>
              {PROTOCOLS.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.label}
                </option>
              ))}
            </select>
          </Field>

          <Field
            label="Endpoint"
            error={submitted ? (errors as { endpoint?: string | null }).endpoint : null}
            hint="Every request goes through your backend to this address — nothing in the sandbox talks to it directly."
          >
            <input
              className="input input--mono"
              value={endpoint}
              placeholder={PROTOCOLS.find((p) => p.id === protocol)?.placeholder}
              onChange={(e) => setEndpoint(e.target.value)}
            />
          </Field>

          <div className="field-row">
            <Field label="Model identifier" error={submitted ? errors.modelId : null}>
              <input className="input input--mono" value={modelId} onChange={(e) => setModelId(e.target.value)} />
            </Field>
            <Field label="Display name" hint="What developers see. Defaults to the identifier.">
              <input className="input" value={name} placeholder={modelId} onChange={(e) => setName(e.target.value)} />
            </Field>
          </div>

          {keyField}

          <div className="field-row">
            <Field label="Runs">
              <select
                className="select"
                value={hosting}
                onChange={(e) => setHosting(e.target.value as "internal" | "external")}
              >
                <option value="external">At an outside provider</option>
                <option value="internal">Inside your network</option>
              </select>
            </Field>
            <Field label="Context window (tokens)">
              <input
                className="input"
                type="number"
                value={context}
                onChange={(e) => setContext(Number(e.target.value))}
              />
            </Field>
          </div>

          {/* Nothing is billed here — these rates are what the console
              multiplies token counts by to show spend. */}
          <div className="field">
            <span className="label">Cost estimate, per Mtok</span>
            <div className="field-row field-row--3">
              <Field label="Tokens in">
                <input
                  className="input"
                  type="number"
                  step="0.01"
                  value={costIn}
                  onChange={(e) => setCostIn(Number(e.target.value))}
                />
              </Field>
              <Field label="Cached in">
                <input
                  className="input"
                  type="number"
                  step="0.01"
                  value={costCached}
                  onChange={(e) => setCostCached(Number(e.target.value))}
                />
              </Field>
              <Field label="Tokens out">
                <input
                  className="input"
                  type="number"
                  step="0.01"
                  value={costOut}
                  onChange={(e) => setCostOut(Number(e.target.value))}
                />
              </Field>
            </div>
            <span className="hint">
              {hosting === "internal"
                ? "Your internal compute rate, used for chargeback."
                : "Used for the spend figures in the console. Not a bill."}
            </span>
          </div>
        </>
      )}

      {failure && (
        <div className="callout callout--deny" style={{ marginTop: 16 }}>
          <strong>The model did not answer a test prompt.</strong>
          <div className="mono small" style={{ margin: "6px 0", wordBreak: "break-word" }}>
            {failure}
          </div>
          {named ? "Check the key, then test again." : "Check the protocol, endpoint, model and key, then test again."}
        </div>
      )}
    </Modal>
  );
}

/** What a saved draft looks like as a listed model, for optimistic display. */
export const draftToModel = (draft: ModelDraft, id: string, addedAt: string): Model => ({
  id,
  name: draft.name,
  provider: draft.provider,
  hosting: draft.hosting,
  endpoint: draft.endpoint,
  modelId: draft.modelId,
  contextWindow: draft.contextWindow,
  status: "enabled",
  costPerMTokIn: draft.costPerMTokIn,
  costPerMTokCachedIn: draft.costPerMTokCachedIn,
  costPerMTokOut: draft.costPerMTokOut,
  addedAt,
});
