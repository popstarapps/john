/* Formatting. One place, so a token count looks the same everywhere. */

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

/** "4m ago", "in 3d" — compact, for tables and timestamps in lists. */
export function relative(iso: string | null | undefined): string {
  if (!iso) return "—";
  const diff = new Date(iso).getTime() - Date.now();
  const abs = Math.abs(diff);
  const min = 60_000;
  const units: Array<[number, Intl.RelativeTimeFormatUnit]> = [
    [min, "minute"],
    [60 * min, "hour"],
    [24 * 60 * min, "day"],
    [7 * 24 * 60 * min, "week"],
    [30 * 24 * 60 * min, "month"],
    [365 * 24 * 60 * min, "year"],
  ];
  if (abs < min) return "just now";
  for (let i = 0; i < units.length; i++) {
    const [ms, unit] = units[i];
    const next = units[i + 1]?.[0] ?? Infinity;
    if (abs < next) return rtf.format(Math.round(diff / ms), unit);
  }
  return rtf.format(Math.round(diff / units[5][0]), "year");
}

const dtfDateTime = new Intl.DateTimeFormat(undefined, {
  year: "numeric",
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
});
const dtfDate = new Intl.DateTimeFormat(undefined, {
  year: "numeric",
  month: "short",
  day: "numeric",
});
const dtfTime = new Intl.DateTimeFormat(undefined, {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
});

export const dateTime = (iso: string | null | undefined) =>
  iso ? dtfDateTime.format(new Date(iso)) : "—";
export const date = (iso: string | null | undefined) =>
  iso ? dtfDate.format(new Date(iso)) : "—";
export const time = (iso: string | null | undefined) =>
  iso ? dtfTime.format(new Date(iso)) : "—";

/** Axis labels and short date chips: "14 Aug". */
export const shortDay = (isoDate: string) =>
  new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short" }).format(
    new Date(isoDate + "T00:00:00Z"),
  );

const nf = new Intl.NumberFormat();
export const num = (n: number) => nf.format(Math.round(n));

/** 12.4k / 3.1M — for counts that get large but need to stay narrow. */
export function compact(n: number): string {
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(n < 10_000_000 ? 1 : 0)}M`;
  return `${(n / 1_000_000_000).toFixed(1)}B`;
}

export function bytes(n: number): string {
  if (n === 0) return "0 B";
  const units = ["B", "kB", "MB", "GB", "TB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const v = n / Math.pow(1024, i);
  return `${v.toFixed(v < 10 && i > 0 ? 1 : 0)} ${units[i]}`;
}

export function duration(ms: number): string {
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

/** Elapsed between two instants, second open-ended. */
export function span(from: string, to: string | null): string {
  return duration((to ? new Date(to).getTime() : Date.now()) - new Date(from).getTime());
}

export const pct = (part: number, whole: number) =>
  whole === 0 ? "0%" : `${((part / whole) * 100).toFixed(part / whole < 0.1 ? 1 : 0)}%`;

/**
 * Dollars.
 *
 * Headline figures adapt — cents on a small number, none on a large one. In a
 * column or a list, pass `digits` so every row has the same number of them and
 * the decimal points line up.
 */
export function money(usd: number, digits?: number): string {
  return new Intl.NumberFormat(undefined, {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: digits,
    maximumFractionDigits: digits ?? (usd < 100 ? 2 : 0),
  }).format(usd);
}

/** Signed change between two totals, as a percentage. Returns null if there
 *  is no meaningful baseline to compare against. */
export function change(current: number, previous: number): number | null {
  if (!previous) return null;
  return ((current - previous) / previous) * 100;
}

export const initials = (name: string) =>
  name
    .split(" ")
    .map((p) => p[0])
    .join("")
    .slice(0, 2)
    .toUpperCase();
