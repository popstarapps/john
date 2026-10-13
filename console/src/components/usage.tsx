import { Card } from "./ui";
import { Legend, Stat, StackedBars } from "./charts";
import { change, compact, money, num, shortDay } from "../lib/format";
import type { UsageSummary } from "../api/client";

/* The usage view, shared.
 *
 * `/` is one person's and `/admin` is the whole organization's, but they are
 * the same question at two scales — so they are the same tiles, the same
 * chart, and the same table, and a number means the same thing on both. */

/** Models that actually did something in the window, in fixed chart order. */
export const usedModels = (summary: UsageSummary) =>
  summary.byModel.filter((row) => row.costUsd > 0 || row.requests > 0);

export function UsageTiles({ summary, days }: { summary: UsageSummary; days: string }) {
  const { totals, previous } = summary;
  return (
    <div className="stat-row">
      {/* Spend and sessions are single numbers; tokens are three numbers that
          only mean something together, so they share one tile. */}
      <div className="card stat">
        <div className="stat-label">Tokens</div>
        <div className="token-split">
          <div>
            <span className="tk-value">{compact(totals.tokensIn)}</span>
            <span className="tk-label">in</span>
          </div>
          <div>
            <span className="tk-value">{compact(totals.tokensCached)}</span>
            <span className="tk-label">cached in</span>
          </div>
          <div>
            <span className="tk-value">{compact(totals.tokensOut)}</span>
            <span className="tk-label">out</span>
          </div>
        </div>
        <div className="stat-foot">
          {Math.round((totals.tokensCached / Math.max(1, totals.tokensIn)) * 100)}% of input served
          from cache
        </div>
      </div>

      <Stat
        label="Sessions"
        icon="terminal"
        value={num(totals.sessions)}
        delta={change(totals.sessions, previous.sessions)}
        foot={`vs previous ${days} days`}
      />

      <Stat
        label="Spend"
        icon="zap"
        value={money(totals.costUsd)}
        delta={change(totals.costUsd, previous.costUsd)}
        foot={`vs previous ${days} days`}
      />
    </div>
  );
}

export function SpendPerDay({ summary, days }: { summary: UsageSummary; days: string }) {
  const used = usedModels(summary);
  return (
    <Card title="Spend per day" sub={`By model · last ${days} days`}>
      <Legend
        items={used.map((row) => ({
          key: row.model.id,
          label: row.model.name,
          value: money(row.costUsd, 2),
        }))}
      />
      <StackedBars
        ariaLabel={`Daily spend by model over the last ${days} days`}
        labels={summary.days.map((d) => shortDay(d.date))}
        series={used.map((row) => ({
          key: row.model.id,
          label: row.model.name,
          values: summary.days.map(
            (d) => d.byModel.find((m) => m.modelId === row.model.id)?.costUsd ?? 0,
          ),
        }))}
        format={(v) => (v >= 10 ? `$${Math.round(v)}` : `$${v.toFixed(2)}`)}
        height={240}
      />
    </Card>
  );
}

export function ByModel({ summary }: { summary: UsageSummary }) {
  const used = usedModels(summary);
  return (
    <Card
      title="By model"
      sub={`${used.length} model${used.length === 1 ? "" : "s"} answered in this window`}
      pad={false}
    >
      <div className="table-wrap">
        <table className="tbl">
          <thead>
            <tr>
              <th>Model</th>
              <th className="right">Requests</th>
              <th className="right">Tokens</th>
              <th className="right">Spend</th>
            </tr>
          </thead>
          <tbody>
            {used.map((row, i) => (
              <tr key={row.model.id}>
                <td>
                  <span className="row" style={{ gap: 8 }}>
                    <span className="swatch" style={{ background: `var(--series-${(i % 5) + 1})` }} />
                    <span>{row.model.name}</span>
                    {row.model.hosting === "internal" && (
                      <span className="small muted">internal rate</span>
                    )}
                  </span>
                </td>
                <td className="right num">{compact(row.requests)}</td>
                <td className="right num">{compact(row.tokens)}</td>
                <td className="right num">{money(row.costUsd, 2)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="pager">
        <span>
          Spend is an estimate: token counts multiplied by the rates configured for each model.
          Internally hosted models carry the organization's own compute rate.
        </span>
      </div>
    </Card>
  );
}
