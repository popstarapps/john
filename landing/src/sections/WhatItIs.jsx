import Section from "../components/Section.jsx";

// Small line icons, drawn on a 24-unit grid in the page's own stroke style.
const ICONS = {
  sandbox: (
    <path d="M12 3 L20 7.5 V16.5 L12 21 L4 16.5 V7.5 Z M4 7.5 L12 12 L20 7.5 M12 12 V21" />
  ),
  lock: (
    <>
      <rect x="5" y="11" width="14" height="10" rx="2" />
      <path d="M8 11 V7.5 a4 4 0 0 1 8 0 V11 M12 15 V17" />
    </>
  ),
  globe: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M3 12 H21 M12 3 a13 13 0 0 1 0 18 M12 3 a13 13 0 0 0 0 18" />
    </>
  ),
  server: (
    <>
      <rect x="4" y="4" width="16" height="7" rx="1.5" />
      <rect x="4" y="13" width="16" height="7" rx="1.5" />
      <path d="M8 7.5 H8.01 M8 16.5 H8.01" />
    </>
  ),
  building: (
    <path d="M4 21 V5 a1 1 0 0 1 1 -1 H14 a1 1 0 0 1 1 1 V21 M15 10 H19 a1 1 0 0 1 1 1 V21 M3 21 H21 M8 8 H11 M8 12 H11 M8 16 H11" />
  ),
};

const POINTS = [
  {
    icon: "sandbox",
    title: "Agent works in a sandbox",
    body: "Each project gets its own Linux VM, keeping the developer's machine fully out of the agent's reach.",
  },
  {
    icon: "lock",
    title: "Protect your IP",
    body: "Centrally route sensitive work to internally hosted models, and redact secrets and internal names from anything sent to external ones.",
  },
  {
    icon: "globe",
    title: "Centrally control what the agent can reach",
    body: "No network access by default. Managed policies decide which outbound requests are allowed.",
  },
  {
    icon: "server",
    title: "The agent loop runs on your backend",
    body: "Every model call is made by your server. Developer machines hold no model credentials, so there's no way around your controls.",
  },
  {
    icon: "building",
    title: "Fits the enterprise stack",
    body: "Self-hosted, SSO via your identity provider, centrally stored encrypted session logs, and open source under MIT.",
  },
];

export default function WhatItIs() {
  return (
    <Section id="what">
      <div className="benefits-head">
        <h2>Developers get the agent. Security keeps control.</h2>
        <p className="lede">
          John gives engineering teams a capable coding agent, and gives the
          organization the controls to decide what it can see, where it can
          reach and which models it uses.
        </p>
      </div>

      <ul className="benefits">
        {POINTS.map((item) => (
          <li key={item.title}>
            <span className="benefit-icon" aria-hidden="true">
              <svg viewBox="0 0 24 24">{ICONS[item.icon]}</svg>
            </span>
            <h3>{item.title}</h3>
            <p>{item.body}</p>
          </li>
        ))}
      </ul>
    </Section>
  );
}
