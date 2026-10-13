import { useEffect, useState } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router-dom";
import { Lockup } from "./Logo";
import { Icon, type IconName } from "./Icon";
import { Menu } from "./overlays";
import { Badge } from "./ui";
import { useQuery } from "../lib/useQuery";
import { getOrg, getViewer, listNetworkAccessRequests } from "../api/client";
import { currentTheme, toggleTheme, type Theme } from "../lib/theme";

interface NavEntry {
  to: string;
  label: string;
  icon: IconName;
  end?: boolean;
  count?: number;
  attn?: boolean;
}

function NavList({ title, items }: { title: string; items: NavEntry[] }) {
  return (
    <div className="nav-group">
      <span className="eyebrow">{title}</span>
      {items.map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.end}
          className={({ isActive }) => (isActive ? "nav-item nav-item--on" : "nav-item")}
        >
          <Icon name={item.icon} size={15} />
          <span className="truncate">{item.label}</span>
          {item.count ? (
            <span className={item.attn ? "count count--attn" : "count"}>{item.count}</span>
          ) : null}
        </NavLink>
      ))}
    </div>
  );
}

function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>("light");
  useEffect(() => setTheme(currentTheme()), []);
  return (
    <button
      type="button"
      className="icon-btn"
      onClick={() => setTheme(toggleTheme())}
      title="Toggle theme"
      aria-label={`Switch to ${theme === "dark" ? "light" : "dark"} theme`}
    >
      <Icon name={theme === "dark" ? "sun" : "moon"} size={15} />
    </button>
  );
}

export function AppShell() {
  const location = useLocation();
  const [navOpen, setNavOpen] = useState(false);

  const { data: viewer } = useQuery(() => getViewer(), []);
  const { data: org } = useQuery(() => getOrg(), []);
  const { data: myRequests } = useQuery(
    () =>
      viewer
        ? listNetworkAccessRequests({ requesterId: viewer.user.id, status: "open" })
        : Promise.resolve([]),
    [viewer?.user.id],
  );
  const { data: openRequests } = useQuery(
    () => (viewer?.isAdmin ? listNetworkAccessRequests({ status: "open" }) : Promise.resolve([])),
    [viewer?.isAdmin],
  );

  // Route changes close the mobile sheet.
  useEffect(() => setNavOpen(false), [location.pathname]);

  const developer: NavEntry[] = [
    { to: "/", label: "Usage", icon: "chart", end: true },
    { to: "/requests", label: "My requests", icon: "inbox", count: myRequests?.length },
    { to: "/policies", label: "Policies", icon: "shield" },
    { to: "/download", label: "Download John", icon: "download" },
  ];

  const admin: NavEntry[] = [
    { to: "/admin", label: "Usage", icon: "chart", end: true },
    {
      to: "/admin/requests",
      label: "Access requests",
      icon: "inbox",
      count: openRequests?.length,
      attn: (openRequests?.length ?? 0) > 0,
    },
    { to: "/admin/groups", label: "Policy groups", icon: "layers" },
    { to: "/admin/teams", label: "Teams", icon: "users" },
    { to: "/admin/members", label: "Members", icon: "user" },
    { to: "/admin/models", label: "Models", icon: "model" },
    { to: "/admin/traces", label: "Session traces", icon: "book" },
    { to: "/admin/history", label: "Change history", icon: "activity" },
    { to: "/admin/settings", label: "Settings", icon: "settings" },
  ];

  return (
    <div className="app">
      <header className="topbar">
        <button
          className="icon-btn sidebar-toggle"
          onClick={() => setNavOpen((v) => !v)}
          aria-label={navOpen ? "Close navigation" : "Open navigation"}
          aria-expanded={navOpen}
        >
          <Icon name={navOpen ? "x" : "menu"} size={17} />
        </button>

        <Link className="topbar-brand" to="/" aria-label="John console, home">
          <Lockup />
        </Link>
        {org && (
          <span className="org-chip">
            <span className="dot" />
            {org.name}
          </span>
        )}

        <span className="topbar-spacer" />

        <div className="topbar-end">
          <ThemeToggle />

          {org?.standalone ? (
            // Nobody to sign in as or out of: john signs in the local account.
            <span title={`A single-user trial on this machine, signed in as ${viewer?.user.email ?? "you"}.`}>
              <Badge tone="info">Standalone</Badge>
            </span>
          ) : (
            <Menu
              width={230}
              trigger={({ toggle }) => (
                <button className="usermenu-trigger" onClick={toggle}>
                  <span className="avatar">{viewer?.user.initials ?? "  "}</span>
                  <span className="usermenu-name truncate">{viewer?.user.name ?? "…"}</span>
                  <Icon name="chevronDown" size={13} />
                </button>
              )}
            >
              {(close) => (
                <>
                  <div className="menu-label">Signed in as</div>
                  <div style={{ padding: "0 9px 8px" }}>
                    <div style={{ fontSize: 13 }}>{viewer?.user.email}</div>
                    <div className="small muted">
                      {viewer?.isAdmin ? "Administrator" : "Member"}
                      {" · via "}
                      {org?.sso.provider ?? "SSO"}
                    </div>
                  </div>
                  <div className="menu-sep" />
                  <a className="menu-item" href="/auth/logout" onClick={close}>
                    <Icon name="logout" size={14} /> Sign out
                  </a>
                </>
              )}
            </Menu>
          )}
        </div>
      </header>

      <div className="body">
        {navOpen && (
          <button className="scrim" aria-label="Close navigation" onClick={() => setNavOpen(false)} />
        )}
        <nav className={navOpen ? "sidebar sidebar--open" : "sidebar"} aria-label="Console">
          <NavList title="You" items={developer} />
          {viewer?.isAdmin && <NavList title="Administration" items={admin} />}
          <div className="sidebar-foot">John {org?.backend.version}</div>
        </nav>

        <main className="page" id="main">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
