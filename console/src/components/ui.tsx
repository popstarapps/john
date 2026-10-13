import type { ReactNode } from "react";
import { useState } from "react";
import { Link } from "react-router-dom";
import { Icon, type IconName } from "./Icon";

/* ---------- page furniture ---------- */

export function Crumbs({ items }: { items: Array<{ label: string; to?: string }> }) {
  return (
    <nav className="crumbs" aria-label="Breadcrumb">
      {items.map((item, i) => (
        <span key={i} className="row" style={{ gap: 6 }}>
          {i > 0 && <span className="sep">/</span>}
          {item.to ? <Link to={item.to}>{item.label}</Link> : <span>{item.label}</span>}
        </span>
      ))}
    </nav>
  );
}

export function PageHead({
  title,
  lede,
  actions,
  crumbs,
  meta,
}: {
  title: ReactNode;
  lede?: ReactNode;
  actions?: ReactNode;
  crumbs?: Array<{ label: string; to?: string }>;
  meta?: ReactNode;
}) {
  return (
    <>
      {crumbs && <Crumbs items={crumbs} />}
      <div className="page-head">
        <div className="grow">
          <div className="row row-wrap" style={{ gap: 10 }}>
            <h1>{title}</h1>
            {meta}
          </div>
          {lede && <p className="lede">{lede}</p>}
        </div>
        {actions && <div className="page-head-actions">{actions}</div>}
      </div>
    </>
  );
}

export function Card({
  title,
  sub,
  actions,
  children,
  foot,
  className = "",
  bodyClass = "card-body",
  pad = true,
}: {
  title?: ReactNode;
  sub?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  foot?: ReactNode;
  className?: string;
  bodyClass?: string;
  pad?: boolean;
}) {
  return (
    <section className={`card ${className}`}>
      {(title || actions) && (
        <header className="card-head">
          <div className="grow">
            {typeof title === "string" ? <h2>{title}</h2> : title}
            {sub && <div className="sub">{sub}</div>}
          </div>
          {actions && <div className="row">{actions}</div>}
        </header>
      )}
      {pad ? <div className={bodyClass}>{children}</div> : children}
      {foot && <footer className="card-foot">{foot}</footer>}
    </section>
  );
}

export function Empty({
  icon = "inbox",
  title,
  children,
  action,
}: {
  icon?: IconName;
  title: string;
  children?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="empty">
      <span className="empty-icon">
        <Icon name={icon} size={26} />
      </span>
      <h3>{title}</h3>
      {children && <p>{children}</p>}
      {action}
    </div>
  );
}

export function Loading({ rows = 5, height = 34 }: { rows?: number; height?: number }) {
  return (
    <div className="col" style={{ gap: 6, padding: 12 }} aria-busy="true" aria-label="Loading">
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton" style={{ height, opacity: 1 - i * 0.12 }} />
      ))}
    </div>
  );
}

/* ---------- badges ---------- */

export type Tone = "neutral" | "allow" | "deny" | "warn" | "info";

export function Badge({
  tone = "neutral",
  icon,
  children,
  square,
}: {
  tone?: Tone;
  icon?: IconName;
  children: ReactNode;
  square?: boolean;
}) {
  const cls = ["badge", tone !== "neutral" && `badge--${tone}`, square && "badge--square"]
    .filter(Boolean)
    .join(" ");
  return (
    <span className={cls}>
      {icon && <Icon name={icon} size={11} />}
      {children}
    </span>
  );
}

/** A named thing referenced somewhere else: a group, a team, a policy. */
export function Tag({ children, baseline }: { children: ReactNode; baseline?: boolean }) {
  return <span className={baseline ? "tag tag--baseline" : "tag"}>{children}</span>;
}

export function Avatar({ name, size = "md" }: { name: string; size?: "sm" | "md" | "lg" }) {
  const initials = name
    .split(" ")
    .map((p) => p[0])
    .join("")
    .slice(0, 2);
  return (
    <span
      className={`avatar ${size === "lg" ? "avatar--lg" : size === "sm" ? "avatar--sm" : ""}`}
      title={name}
    >
      {initials}
    </span>
  );
}

export function Person({ name, sub, to }: { name: string; sub?: string; to?: string }) {
  const inner = (
    <span className="row" style={{ gap: 8 }}>
      <Avatar name={name} size="sm" />
      <span className="col" style={{ gap: 0 }}>
        <span>{name}</span>
        {sub && <span className="small muted">{sub}</span>}
      </span>
    </span>
  );
  return to ? (
    <Link to={to} style={{ display: "inline-block" }}>
      {inner}
    </Link>
  ) : (
    inner
  );
}

/* ---------- inputs ---------- */

export function SearchInput({
  value,
  onChange,
  placeholder = "Search",
  autoFocus,
}: {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  autoFocus?: boolean;
}) {
  return (
    <div className="search grow">
      <Icon name="search" size={14} />
      <input
        className="input"
        value={value}
        autoFocus={autoFocus}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        type="search"
      />
    </div>
  );
}

export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: ReactNode;
  hint?: ReactNode;
  error?: string | null;
  children: ReactNode;
}) {
  return (
    <label className={error ? "field field--err" : "field"}>
      <span className="label">{label}</span>
      {children}
      {error ? <span className="err">{error}</span> : hint ? <span className="hint">{hint}</span> : null}
    </label>
  );
}

export function Segmented<T extends string>({
  value,
  options,
  onChange,
  label,
  disabled,
}: {
  value: T;
  options: Array<{ value: T; label: string }>;
  onChange: (v: T) => void;
  label?: string;
  /** Shows the current choice without offering to change it. */
  disabled?: boolean;
}) {
  return (
    <div className="btn-group" role="group" aria-label={label}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={o.value === value}
          disabled={disabled}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function Tabs({
  value,
  options,
  onChange,
}: {
  value: string;
  options: Array<{ value: string; label: string; count?: number }>;
  onChange: (v: string) => void;
}) {
  return (
    <div className="tabs" role="tablist">
      {options.map((o) => (
        <button
          key={o.value}
          role="tab"
          aria-selected={o.value === value}
          className={o.value === value ? "on" : undefined}
          onClick={() => onChange(o.value)}
        >
          {o.label}
          {o.count !== undefined && <span className="muted"> {o.count}</span>}
        </button>
      ))}
    </div>
  );
}

/* ---------- data display ---------- */

export function KV({ children }: { children: ReactNode }) {
  return <dl className="kv">{children}</dl>;
}

export function Row({ k, children }: { k: ReactNode; children: ReactNode }) {
  return (
    <>
      <dt>{k}</dt>
      <dd>{children}</dd>
    </>
  );
}

export function CopyLine({ text, label }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="copyline">
      <span className="grow">{label ?? text}</span>
      <button
        type="button"
        className="icon-btn"
        title="Copy"
        onClick={() => {
          void navigator.clipboard?.writeText(text);
          setCopied(true);
          setTimeout(() => setCopied(false), 1400);
        }}
      >
        <Icon name={copied ? "check" : "copy"} size={13} />
      </button>
    </div>
  );
}

export function Meter({
  label,
  value,
  max,
  display,
  muted,
}: {
  label: ReactNode;
  value: number;
  max: number;
  display: string;
  muted?: boolean;
}) {
  const w = max > 0 ? Math.max(1.5, (value / max) * 100) : 0;
  return (
    <div className="meter">
      <span className="truncate">{label}</span>
      <span className="track">
        <span className={muted ? "fill fill--muted" : "fill"} style={{ width: `${w}%` }} />
      </span>
      <span className="val">{display}</span>
    </div>
  );
}

export function Pager({
  shown,
  total,
  onMore,
}: {
  shown: number;
  total: number;
  onMore?: () => void;
}) {
  return (
    <div className="pager">
      <span>
        Showing <strong>{shown}</strong> of {total}
      </span>
      {onMore && shown < total && (
        <button className="btn btn--sm" onClick={onMore}>
          Show more
        </button>
      )}
    </div>
  );
}
