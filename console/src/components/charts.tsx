import { useEffect, useRef, useState, type ReactNode } from "react";
import { Icon, type IconName } from "./Icon";

/* Charts.
 *
 * Every chart here plots ONE measure in ONE hue. Where several quantities
 * belong on the same screen they become separate tiles with their own labels
 * and their own baselines, never adjacent fills separated only by colour:
 * allowed / ask-developer / blocked is exactly the green-amber-red set that
 * collapses under deuteranopia, so it is never asked to carry meaning alone.
 *
 * No dependency — a handful of SVG paths is smaller and themes correctly,
 * since every colour is a CSS token resolved at paint time.
 */

/** Element width, tracked so the SVG can be laid out in real pixels. */
function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  const [w, setW] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver(([entry]) => setW(entry.contentRect.width));
    ro.observe(el);
    setW(el.clientWidth);
    return () => ro.disconnect();
  }, []);
  return [ref, w] as const;
}

/** A rect with only its top corners rounded, sitting on the baseline. */
function barPath(x: number, y: number, w: number, h: number, r = 3) {
  const rr = Math.max(0, Math.min(r, w / 2, h));
  return `M${x} ${y + h}V${y + rr}a${rr} ${rr} 0 0 1 ${rr} -${rr}h${w - 2 * rr}a${rr} ${rr} 0 0 1 ${rr} ${rr}V${y + h}Z`;
}

export interface Point {
  label: string;
  value: number;
}

/* ---------- sparkline ---------- */

export function Sparkline({
  data,
  height = 34,
  showLast = true,
}: {
  data: number[];
  height?: number;
  showLast?: boolean;
}) {
  const [ref, w] = useWidth<HTMLDivElement>();
  const pad = 3;
  const max = Math.max(...data, 1);
  const min = Math.min(...data, 0);
  const span = max - min || 1;
  const x = (i: number) => pad + (i / Math.max(1, data.length - 1)) * (w - pad * 2);
  const y = (v: number) => pad + (1 - (v - min) / span) * (height - pad * 2);

  const line = data.map((v, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join(" ");
  const area = `${line} L${x(data.length - 1).toFixed(1)} ${height} L${x(0).toFixed(1)} ${height} Z`;

  return (
    <div ref={ref} style={{ width: "100%" }}>
      {w > 0 && (
        <svg className="chart" width={w} height={height} role="img" aria-hidden="true">
          <path className="area" d={area} />
          <path className="line" d={line} />
          {showLast && (
            <circle className="marker" cx={x(data.length - 1)} cy={y(data[data.length - 1])} r={3.2} />
          )}
        </svg>
      )}
    </div>
  );
}

/* ---------- bars over time ---------- */

export function TimeBars({
  data,
  height = 180,
  format = (v: number) => String(v),
  ariaLabel,
}: {
  data: Point[];
  height?: number;
  format?: (v: number) => string;
  ariaLabel: string;
}) {
  const [ref, w] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);

  const padL = 38;
  const padR = 6;
  const padT = 10;
  const padB = 20;
  const plotW = Math.max(0, w - padL - padR);
  const plotH = height - padT - padB;
  const max = Math.max(...data.map((d) => d.value), 1);
  // A round-ish top so the gridline labels read as numbers, not as data.
  const step = Math.pow(10, Math.floor(Math.log10(max)));
  const top = Math.ceil(max / (step / 2)) * (step / 2);
  const slot = plotW / Math.max(1, data.length);
  const barW = Math.max(2, slot - 2); // a 2px surface gap between bars
  const yOf = (v: number) => padT + (1 - v / top) * plotH;

  const ticks = [0, top / 2, top];
  const labelEvery = Math.ceil(data.length / (plotW > 620 ? 10 : 5));

  return (
    <div className="chart-frame" ref={ref}>
      {w > 0 && (
        <svg
          className="chart"
          width={w}
          height={height}
          role="img"
          aria-label={ariaLabel}
          onMouseLeave={() => setHover(null)}
        >
          {ticks.map((t) => (
            <g key={t}>
              <line className="grid-line" x1={padL} x2={w - padR} y1={yOf(t)} y2={yOf(t)} />
              <text className="axis-label" x={padL - 7} y={yOf(t) + 3} textAnchor="end">
                {format(t)}
              </text>
            </g>
          ))}

          {data.map((d, i) => {
            const x = padL + i * slot + (slot - barW) / 2;
            const h = Math.max(1, (d.value / top) * plotH);
            return (
              <g key={d.label}>
                <path
                  className="bar"
                  d={barPath(x, padT + plotH - h, barW, h)}
                  opacity={hover === null || hover === i ? 1 : 0.55}
                />
                {/* A hit area the full column height, so short bars are still
                    easy to hover. */}
                <rect
                  x={padL + i * slot}
                  y={padT}
                  width={slot}
                  height={plotH}
                  fill="transparent"
                  onMouseEnter={() => setHover(i)}
                />
                {i % labelEvery === 0 && (
                  <text
                    className="axis-label"
                    x={x + barW / 2}
                    y={height - 6}
                    textAnchor="middle"
                  >
                    {d.label}
                  </text>
                )}
              </g>
            );
          })}
        </svg>
      )}

      {hover !== null && w > 0 && (
        <div
          className="chart-tip"
          style={{
            left: padL + hover * slot + slot / 2,
            top: yOf(data[hover].value) - 8,
          }}
        >
          <div className="t-label">{data[hover].label}</div>
          <div className="t-value">{format(data[hover].value)}</div>
        </div>
      )}
    </div>
  );
}

/* ---------- stacked bars (the one categorical chart) ---------- */

export interface StackSeries {
  key: string;
  label: string;
  /** One value per label, same order. */
  values: number[];
}

/**
 * Spend per day, split by model.
 *
 * The one place the console uses more than one hue at once. Colour slots are
 * assigned by series index and never cycled, so a model keeps its colour when
 * the range changes; the legend carries the value for each series, which is
 * also the relief the light-theme steps require (three of them sit under 3:1
 * on the surface). Segments are separated by a 2px surface gap so adjacent
 * fills never touch.
 */
export function StackedBars({
  labels,
  series,
  height = 220,
  format,
  ariaLabel,
}: {
  labels: string[];
  series: StackSeries[];
  height?: number;
  format: (v: number) => string;
  ariaLabel: string;
}) {
  const [ref, w] = useWidth<HTMLDivElement>();
  const [hover, setHover] = useState<number | null>(null);

  const padL = 46;
  const padR = 6;
  const padT = 10;
  const padB = 20;
  const plotW = Math.max(0, w - padL - padR);
  const plotH = height - padT - padB;

  const totals = labels.map((_, i) => series.reduce((a, s) => a + (s.values[i] ?? 0), 0));
  const max = Math.max(...totals, 0.0001);
  const step = Math.pow(10, Math.floor(Math.log10(max)));
  const top = Math.max(step, Math.ceil(max / (step / 2)) * (step / 2));

  const slot = plotW / Math.max(1, labels.length);
  const barW = Math.max(2, slot - 2);
  const yOf = (v: number) => padT + (1 - v / top) * plotH;
  const ticks = [0, top / 2, top];
  const labelEvery = Math.ceil(labels.length / (plotW > 620 ? 10 : 5));

  return (
    <div className="chart-frame" ref={ref}>
      {w > 0 && (
        <svg
          className="chart"
          width={w}
          height={height}
          role="img"
          aria-label={ariaLabel}
          onMouseLeave={() => setHover(null)}
        >
          {ticks.map((t) => (
            <g key={t}>
              <line className="grid-line" x1={padL} x2={w - padR} y1={yOf(t)} y2={yOf(t)} />
              <text className="axis-label" x={padL - 7} y={yOf(t) + 3} textAnchor="end">
                {format(t)}
              </text>
            </g>
          ))}

          {labels.map((label, i) => {
            const x = padL + i * slot + (slot - barW) / 2;
            let acc = 0;
            const lastNonZero = series.reduce(
              (best, s, si) => ((s.values[i] ?? 0) > 0 ? si : best),
              -1,
            );
            return (
              <g key={label}>
                {series.map((s, si) => {
                  const v = s.values[i] ?? 0;
                  if (v <= 0) return null;
                  const yTop = yOf(acc + v);
                  const yBottom = yOf(acc);
                  acc += v;
                  const gap = si === lastNonZero ? 0 : 2;
                  const h = Math.max(1, yBottom - yTop - gap);
                  return (
                    <path
                      key={s.key}
                      d={
                        si === lastNonZero
                          ? barPath(x, yTop, barW, h, 3)
                          : `M${x} ${yTop}h${barW}v${h}h${-barW}Z`
                      }
                      fill={`var(--series-${(si % 5) + 1})`}
                      opacity={hover === null || hover === i ? 1 : 0.5}
                    />
                  );
                })}
                <rect
                  x={padL + i * slot}
                  y={padT}
                  width={slot}
                  height={plotH}
                  fill="transparent"
                  onMouseEnter={() => setHover(i)}
                />
                {i % labelEvery === 0 && (
                  <text className="axis-label" x={x + barW / 2} y={height - 6} textAnchor="middle">
                    {label}
                  </text>
                )}
              </g>
            );
          })}
        </svg>
      )}

      {hover !== null && w > 0 && (
        <div
          className="chart-tip"
          style={{
            left: Math.min(Math.max(padL + hover * slot + slot / 2, 90), w - 90),
            top: yOf(totals[hover]) - 8,
          }}
        >
          <div className="t-label">{labels[hover]}</div>
          {series.map((s, si) => (
            <div key={s.key} className="tip-row">
              <span className="swatch" style={{ background: `var(--series-${(si % 5) + 1})` }} />
              <span className="grow">{s.label}</span>
              <span className="t-value">{format(s.values[hover] ?? 0)}</span>
            </div>
          ))}
          <div className="tip-row tip-row--total">
            <span className="grow">Total</span>
            <span className="t-value">{format(totals[hover])}</span>
          </div>
        </div>
      )}
    </div>
  );
}

/** Identity for the stacked chart, with each series' value alongside it. */
export function Legend({
  items,
}: {
  items: Array<{ key: string; label: string; value: string }>;
}) {
  return (
    <ul className="legend">
      {items.map((item, i) => (
        <li key={item.key}>
          <span className="swatch" style={{ background: `var(--series-${(i % 5) + 1})` }} />
          <span className="legend-label">{item.label}</span>
          <span className="legend-value">{item.value}</span>
        </li>
      ))}
    </ul>
  );
}

/* ---------- stat tile ---------- */

export function Stat({
  label,
  value,
  unit,
  icon,
  foot,
  delta,
  spark,
  tone,
}: {
  label: ReactNode;
  value: ReactNode;
  unit?: string;
  icon?: IconName;
  foot?: ReactNode;
  /** Percentage change against the previous window; null hides it. */
  delta?: number | null;
  spark?: number[];
  tone?: "allow" | "deny" | "warn";
}) {
  const dir = delta === null || delta === undefined ? null : delta > 1 ? "up" : delta < -1 ? "down" : "flat";
  return (
    <div className="card stat">
      <div className="stat-label">
        {icon && <Icon name={icon} size={13} />}
        {label}
      </div>
      <div className="stat-value" style={tone ? { color: `var(--${tone})` } : undefined}>
        {value}
        {unit && <span className="unit">{unit}</span>}
      </div>
      {(foot || dir) && (
        <div className="stat-foot">
          {dir && (
            <span className={`delta delta--${dir}`}>
              {dir === "up" ? "▲" : dir === "down" ? "▼" : "→"} {Math.abs(delta!).toFixed(0)}%
            </span>
          )}
          {foot}
        </div>
      )}
      {spark && spark.length > 1 && (
        <div className="stat-spark">
          <Sparkline data={spark} height={30} />
        </div>
      )}
    </div>
  );
}
