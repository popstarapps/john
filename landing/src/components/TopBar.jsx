import { useEffect, useRef, useState } from "react";
import { Lockup } from "./Logo.jsx";

const REPO = "https://github.com/popstarapps/john";

// The nav is deliberately short. Everything else on the page is reachable by
// scrolling; these are the six a visitor asks for by name, plus the source.
const LINKS = [
  ["#console", "Console"],
  ["#developers", "Developers"],
  ["#architecture", "Architecture"],
  ["#trifecta", "Security"],
  ["#features", "Details"],
  ["#faq", "FAQ"],
];

/** Reads the theme the pre-paint script in index.html already resolved. */
function currentTheme() {
  if (typeof document === "undefined") return "light";
  const set = document.documentElement.getAttribute("data-theme");
  if (set) return set;
  return window.matchMedia("(prefers-color-scheme: dark)").matches
    ? "dark"
    : "light";
}

function ThemeToggle() {
  // Rendered as light on the server; corrected in the effect below so
  // prerendered markup and first client render always agree.
  const [theme, setTheme] = useState("light");
  useEffect(() => setTheme(currentTheme()), []);

  function toggle() {
    const next = theme === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    try {
      localStorage.setItem("john-theme", next);
    } catch {
      // Private browsing, or storage disabled by policy. The choice simply
      // won't survive a reload.
    }
    setTheme(next);
  }

  return (
    <button
      type="button"
      className="icon-btn"
      onClick={toggle}
      aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}
      title="Toggle theme"
    >
      <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true">
        {theme === "dark" ? (
          <path
            d="M12 4V2m0 20v-2m8-8h2M2 12h2m13.66-5.66 1.41-1.41M4.93 19.07l1.41-1.41m11.32 0 1.41 1.41M4.93 4.93l1.41 1.41M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0Z"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.8"
            strokeLinecap="round"
          />
        ) : (
          <path
            d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5Z"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.8"
            strokeLinejoin="round"
          />
        )}
      </svg>
    </button>
  );
}

export default function TopBar() {
  const [open, setOpen] = useState(false);
  const [stuck, setStuck] = useState(false);
  const navRef = useRef(null);

  // A hairline appears under the bar once the hero has scrolled beneath it.
  useEffect(() => {
    const onScroll = () => setStuck(window.scrollY > 8);
    onScroll();
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);

  // Escape closes the mobile sheet; so does following any link inside it.
  useEffect(() => {
    if (!open) return;
    const onKey = (e) => e.key === "Escape" && setOpen(false);
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open]);

  return (
    <>
      <a className="skip" href="#main">
        Skip to content
      </a>

      <header className={stuck ? "topbar topbar--stuck" : "topbar"}>
        <div className="topbar-in">
          <a className="topbar-brand" href="#top" aria-label="John, home">
            <Lockup />
          </a>

          <nav
            ref={navRef}
            className={open ? "topbar-nav topbar-nav--open" : "topbar-nav"}
            aria-label="Sections"
            onClick={() => setOpen(false)}
          >
            {LINKS.map(([href, label]) => (
              <a key={href} href={href}>
                {label}
              </a>
            ))}
            <a href={REPO} rel="noreferrer" target="_blank">
              GitHub
            </a>
          </nav>

          <div className="topbar-end">
            <ThemeToggle />
            <a className="btn btn--sm" href="#start">
              Get started
            </a>
            <button
              type="button"
              className="icon-btn topbar-burger"
              aria-expanded={open}
              aria-label={open ? "Close menu" : "Open menu"}
              onClick={() => setOpen((v) => !v)}
            >
              <svg viewBox="0 0 24 24" width="18" height="18" aria-hidden="true">
                <path
                  d={open ? "M6 6l12 12M18 6L6 18" : "M4 7h16M4 12h16M4 17h16"}
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.8"
                  strokeLinecap="round"
                />
              </svg>
            </button>
          </div>
        </div>
      </header>
    </>
  );
}
