import { useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  createModel,
  deleteModel,
  listModels,
  listPolicies,
  setModelStatus,
  testModel,
  updateModel,
} from "../../api/client";
import { useAction, useQuery } from "../../lib/useQuery";
import { Badge, Card, Empty, Loading, PageHead } from "../../components/ui";
import { ConfirmDialog, Menu, useToast } from "../../components/overlays";
import { ModelDialog, providerLabel } from "../../components/ModelDialog";
import { Icon } from "../../components/Icon";
import { compact, date, money } from "../../lib/format";
import type { Model, ModelRoutingSpec } from "../../api/types";

export default function Models() {
  const toast = useToast();
  // null closes the dialog; {} creates a model; { model } edits one.
  // ?add=1 arrives with the dialog open: it is where standalone's first run
  // sends the browser, and adding a model is the only thing to do there.
  const [params, setParams] = useSearchParams();
  const [editing, setEditingState] = useState<{ model?: Model } | null>(() => (params.get("add") ? {} : null));
  const setEditing = (next: { model?: Model } | null) => {
    setEditingState(next);
    // Once it closes it stays closed, reloads included.
    if (!next && params.has("add")) setParams({}, { replace: true });
  };
  const [deleting, setDeleting] = useState<Model | null>(null);
  const { data: models } = useQuery(() => listModels(), []);
  const { data: routing } = useQuery(() => listPolicies({ type: "model-routing" }), []);
  const [toggle, toggling] = useAction(setModelStatus);
  const [save, savingModel] = useAction(async (model: Model | undefined, draft: Parameters<typeof createModel>[0]) =>
    model ? updateModel(model.id, draft) : createModel(draft),
  );
  const [remove, removing] = useAction(deleteModel);

  if (!models) return <Loading rows={5} height={50} />;

  const usedBy = (modelId: string) =>
    (routing ?? []).filter((p) => (p.spec as ModelRoutingSpec).modelIds.includes(modelId));

  return (
    <>
      <PageHead
        title="Models"
        lede="Every model this backend will route to. A model listed here is available only where a model-routing policy assigns it to a team."
        actions={
          <button className="btn btn--primary" onClick={() => setEditing({})}>
            <Icon name="plus" size={14} /> Add model
          </button>
        }
      />

      {editing && (
        <ModelDialog
          model={editing.model}
          saving={savingModel}
          onTest={(draft) => testModel(draft, editing.model?.id)}
          onClose={() => setEditing(null)}
          onSave={async (draft) => {
            const existing = editing.model;
            await save(existing, draft);
            setEditing(null);
            toast({
              tone: "allow",
              title: existing ? `${draft.name} updated` : `${draft.name} added`,
              body: existing
                ? draft.apiKey
                  ? "Its credential was replaced. New requests use the edited configuration."
                  : "New requests use the edited configuration."
                : (routing ?? []).some((p) => p.enabled)
                  ? "Assign it to a team through a model-routing policy before anyone can use it."
                  : "It is available now: until a model-routing policy exists, every enabled model is.",
            });
          }}
        />
      )}

      {deleting && (
        <ConfirmDialog
          title={`Delete ${deleting.name}?`}
          danger
          busy={removing}
          confirmLabel="Delete model"
          body={
            <>
              <p>
                The model and its stored credential are removed. Past usage keeps counting toward
                spend — the figures on the usage page do not change.
              </p>
              <p className="small muted">
                To stop developers reaching it while keeping it configured, switch it off instead.
              </p>
            </>
          }
          onClose={() => setDeleting(null)}
          onConfirm={async () => {
            const model = deleting;
            try {
              await remove(model.id);
              setDeleting(null);
              toast({ tone: "warn", title: `${model.name} deleted` });
            } catch (err) {
              // The usual refusal is a routing policy that still names it, and
              // the message says which — worth showing rather than swallowing.
              setDeleting(null);
              toast({
                tone: "deny",
                title: `${model.name} was not deleted`,
                body: err instanceof Error ? err.message : "The backend refused.",
              });
            }
          }}
        />
      )}

      <Card pad={false}>
        {models.length === 0 ? (
          <Empty
            icon="plus"
            title="No models yet"
            action={
              <button className="btn btn--primary" onClick={() => setEditing({})}>
                <Icon name="plus" size={14} /> Add model
              </button>
            }
          >
            John needs at least one model, and its provider&apos;s API key, before it can work.
          </Empty>
        ) : (
          <div className="table-wrap">
            <table className="tbl">
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Hosting</th>
                  <th>Endpoint</th>
                  <th className="right">Context</th>
                  <th className="right">Cost estimate / Mtok</th>
                  <th>Used by</th>
                  <th>Enabled</th>
                  <th className="shrink"></th>
                </tr>
              </thead>
              <tbody>
                {models.map((m) => (
                  <tr key={m.id}>
                    <td>
                      <div className="cell-main">{m.name}</div>
                      <div className="cell-sub">
                        {providerLabel(m.provider)} · added {date(m.addedAt)}
                      </div>
                    </td>
                    <td>
                      <Badge tone={m.hosting === "internal" ? "allow" : "info"}>
                        {m.hosting === "internal" ? "internal" : "external"}
                      </Badge>
                    </td>
                    <td className="mono small truncate" style={{ maxWidth: 280 }}>
                      {m.endpoint}
                    </td>
                    <td className="right num">{compact(m.contextWindow)}</td>
                    <td className="right num nowrap">
                      {m.costPerMTokIn === 0 && m.costPerMTokOut === 0 ? (
                        <span className="muted">—</span>
                      ) : (
                        <>
                          {money(m.costPerMTokIn, 2)} in
                          <div className="cell-sub">
                            {money(m.costPerMTokCachedIn, 2)} cached · {money(m.costPerMTokOut, 2)} out
                          </div>
                        </>
                      )}
                    </td>
                    <td className="small">
                      {usedBy(m.id).length === 0 ? (
                        <span className="muted">no policy</span>
                      ) : (
                        `${usedBy(m.id).length} routing polic${usedBy(m.id).length === 1 ? "y" : "ies"}`
                      )}
                    </td>
                    <td className="shrink">
                      <label className="switch">
                        <input
                          type="checkbox"
                          checked={m.status === "enabled"}
                          disabled={toggling}
                          onChange={async () => {
                            const next = m.status === "enabled" ? "disabled" : "enabled";
                            await toggle(m.id, next);
                            toast({
                              tone: next === "enabled" ? "allow" : "warn",
                              title: `${m.name} ${next}`,
                              body:
                                next === "disabled"
                                  ? "Requests routed to it will now be denied."
                                  : "Available wherever a routing policy assigns it.",
                            });
                          }}
                        />
                      </label>
                    </td>
                    <td className="shrink">
                      <Menu
                        width={180}
                        trigger={({ toggle: open }) => (
                          <button className="icon-btn" aria-label={`Actions for ${m.name}`} onClick={open}>
                            <Icon name="more" size={15} />
                          </button>
                        )}
                      >
                        {(close) => (
                          <>
                            <button
                              className="menu-item"
                              onClick={() => {
                                close();
                                setEditing({ model: m });
                              }}
                            >
                              <Icon name="settings" size={14} /> Edit…
                            </button>
                            <div className="menu-sep" />
                            <button
                              className="menu-item menu-item--danger"
                              onClick={() => {
                                close();
                                setDeleting(m);
                              }}
                            >
                              <Icon name="trash" size={14} /> Delete…
                            </button>
                          </>
                        )}
                      </Menu>
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
