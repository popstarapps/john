import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import { Icon } from "./Icon";

/* ---------- escape / outside-click helpers ---------- */

function useEscape(active: boolean, onClose: () => void) {
  useEffect(() => {
    if (!active) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [active, onClose]);
}

/* ---------- modal & drawer ---------- */

interface SheetProps {
  title: ReactNode;
  sub?: ReactNode;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
}

function Sheet({
  variant,
  title,
  sub,
  onClose,
  children,
  footer,
  wide,
}: SheetProps & { variant: "modal" | "drawer" }) {
  useEscape(true, onClose);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // Move focus into the sheet so Escape and Tab behave, without trapping.
    const first = ref.current?.querySelector<HTMLElement>(
      "input, textarea, select, button:not([data-close])",
    );
    (first ?? ref.current)?.focus();
    const { overflow } = document.body.style;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = overflow;
    };
  }, []);

  return (
    <div
      className={`overlay ${variant === "modal" ? "overlay--center" : "overlay--right"}`}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={ref}
        className={variant === "modal" ? `modal ${wide ? "modal--wide" : ""}` : "drawer"}
        role="dialog"
        aria-modal="true"
        aria-label={typeof title === "string" ? title : undefined}
        tabIndex={-1}
      >
        <header className="sheet-head">
          <div className="grow">
            <h2>{title}</h2>
            {sub && <div className="small muted" style={{ marginTop: 2 }}>{sub}</div>}
          </div>
          <button className="icon-btn" onClick={onClose} data-close aria-label="Close">
            <Icon name="x" size={16} />
          </button>
        </header>
        <div className="sheet-body">{children}</div>
        {footer && <footer className="sheet-foot">{footer}</footer>}
      </div>
    </div>
  );
}

export const Modal = (p: SheetProps) => <Sheet variant="modal" {...p} />;
export const Drawer = (p: SheetProps) => <Sheet variant="drawer" {...p} />;

export function ConfirmDialog({
  title,
  body,
  confirmLabel = "Confirm",
  danger,
  busy,
  onConfirm,
  onClose,
}: {
  title: string;
  body: ReactNode;
  confirmLabel?: string;
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onClose: () => void;
}) {
  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            className={danger ? "btn btn--danger" : "btn btn--primary"}
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? "Working…" : confirmLabel}
          </button>
        </>
      }
    >
      {body}
    </Modal>
  );
}

/* ---------- dropdown menu ---------- */

export function Menu({
  trigger,
  children,
  align = "right",
  width,
}: {
  trigger: (props: { open: boolean; toggle: () => void }) => ReactNode;
  children: (close: () => void) => ReactNode;
  align?: "left" | "right";
  width?: number;
}) {
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<CSSProperties | null>(null);
  const wrap = useRef<HTMLDivElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const close = useCallback(() => setOpen(false), []);
  useEscape(open, close);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (!wrap.current?.contains(t) && !menu.current?.contains(t)) setOpen(false);
    };
    // Placed against the viewport, so it would drift from its trigger on a
    // scroll; closing is what menus do then anyway.
    const onMove = (e: Event) => {
      if (!menu.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    window.addEventListener("scroll", onMove, true);
    window.addEventListener("resize", onMove);
    return () => {
      document.removeEventListener("mousedown", onDown);
      window.removeEventListener("scroll", onMove, true);
      window.removeEventListener("resize", onMove);
    };
  }, [open]);

  // The menu is rendered on <body> rather than beside its trigger, because a
  // trigger inside a scrolling container (a table row) would have it clipped
  // at the container's edge. It opens below the trigger, or above when there
  // is no room below.
  useLayoutEffect(() => {
    if (!open) {
      setPos(null);
      return;
    }
    const r = wrap.current!.getBoundingClientRect();
    const h = menu.current?.offsetHeight ?? 0;
    const top = window.innerHeight - r.bottom < h + 10 && r.top > h + 10 ? r.top - 5 - h : r.bottom + 5;
    setPos(align === "right" ? { top, right: window.innerWidth - r.right } : { top, left: r.left });
  }, [open, align]);

  return (
    <div ref={wrap} style={{ position: "relative" }}>
      {trigger({ open, toggle: () => setOpen((v) => !v) })}
      {open &&
        createPortal(
          <div
            ref={menu}
            className="menu"
            style={{ position: "fixed", width, ...(pos ?? { top: 0, left: 0, visibility: "hidden" }) }}
            role="menu"
          >
            {children(close)}
          </div>,
          document.body,
        )}
    </div>
  );
}

/* ---------- toasts ---------- */

interface Toast {
  id: number;
  tone: "allow" | "deny" | "warn";
  title: string;
  body?: string;
}

const ToastCtx = createContext<(t: Omit<Toast, "id">) => void>(() => {});

export function useToast() {
  return useContext(ToastCtx);
}

export function ToastHost({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);
  const push = useCallback((t: Omit<Toast, "id">) => {
    const id = Date.now() + Math.random();
    setItems((xs) => [...xs, { ...t, id }]);
    setTimeout(() => setItems((xs) => xs.filter((x) => x.id !== id)), 5200);
  }, []);
  const value = useMemo(() => push, [push]);

  return (
    <ToastCtx.Provider value={value}>
      {children}
      <div className="toasts" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className={`toast toast--${t.tone}`}>
            <Icon
              name={t.tone === "allow" ? "check" : t.tone === "deny" ? "ban" : "alert"}
              size={14}
            />
            <div className="grow">
              <strong>{t.title}</strong>
              {t.body && <div className="small muted">{t.body}</div>}
            </div>
            <button
              className="icon-btn close"
              aria-label="Dismiss"
              onClick={() => setItems((xs) => xs.filter((x) => x.id !== t.id))}
            >
              <Icon name="x" size={13} />
            </button>
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  );
}
