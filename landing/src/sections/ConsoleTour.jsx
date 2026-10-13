import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import Section from "../components/Section.jsx";
import ConsoleShot from "../components/ConsoleShot.jsx";
import baselineLight from "../assets/console/baseline-light.webp";
import baselineDark from "../assets/console/baseline-dark.webp";
import modelsLight from "../assets/console/models-light.webp";
import modelsDark from "../assets/console/models-dark.webp";
import requestsLight from "../assets/console/requests-light.webp";
import requestsDark from "../assets/console/requests-dark.webp";
import usageLight from "../assets/console/usage-light.webp";
import usageDark from "../assets/console/usage-dark.webp";

// Four screens of the admin console, alternating sides down the page.
const ROWS = [
  {
    title: "Set the rules once",
    body: "Bundle policies into groups, attach them to teams, and mark one as the baseline that no team can weaken.",
    bullets: [
      <>
        Protect paths like <code>.env</code> and <code>.git</code>
      </>,
      "Redact secrets and internal names",
      "Route sensitive work to internal models",
    ],
    shot: {
      light: baselineLight,
      dark: baselineDark,
      alt: "The baseline policy group in the John console: protected paths, redaction rules, blocked sites and model routing.",
    },
  },
  {
    title: "Models and routing",
    body: "Add the external and self-hosted models the backend can use. A model is only available to the teams a routing policy assigns it to.",
    bullets: [
      "External and self-hosted models side by side",
      "Cost per million tokens, in and out",
      "See which routing policies use each one",
    ],
    shot: {
      light: modelsLight,
      dark: modelsDark,
      alt: "The models page in the John console: external and internally hosted models with their endpoints, context sizes, costs and the routing policies that use them.",
    },
  },
  {
    title: "Exceptions without the Slack thread",
    body: "When the agent hits something blocked, a developer can ask for access right from the prompt. Administrators see exactly what was blocked, and approving it creates the policy.",
    bullets: [
      "The real blocked request, not a description",
      "The decision applies to the next attempt",
    ],
    shot: {
      light: requestsLight,
      dark: requestsDark,
      alt: "Pending network access requests in the John console, each with the requested URL, the developer and their team.",
    },
  },
  {
    title: "Know what AI costs",
    body: "Tokens, sessions and spend across the organization, by day, by model, by team and by person.",
    bullets: ["Spend per model, per team and per person", "7, 30 or 60 days"],
    shot: {
      light: usageLight,
      dark: usageDark,
      alt: "The usage page in the John console: tokens, sessions, spend per day by model, and spend by team.",
    },
  },
];

const Chevron = ({ d }) => (
  <svg width="18" height="18" viewBox="0 0 12 12" aria-hidden="true">
    <path d={d} fill="none" stroke="currentColor" strokeWidth="1.6" />
  </svg>
);

/**
 * The screenshots at full size, one at a time, with their text. Rendered into
 * <body> so no transformed ancestor can trap the fixed positioning.
 */
function Lightbox({ index, onClose, onGo }) {
  const row = ROWS[index];
  const closeRef = useRef(null);

  useEffect(() => {
    const opener = document.activeElement;
    closeRef.current?.focus();
    const { overflow } = document.body.style;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = overflow;
      opener?.focus?.();
    };
  }, []);

  useEffect(() => {
    const onKey = (e) => {
      if (e.key === "Escape") onClose();
      else if (e.key === "ArrowLeft") onGo(index - 1);
      else if (e.key === "ArrowRight") onGo(index + 1);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [index, onClose, onGo]);

  return createPortal(
    <div
      className="lightbox"
      role="dialog"
      aria-modal="true"
      aria-label={row.title}
      onClick={onClose}
    >
      <button
        type="button"
        className="lb-btn lb-close"
        onClick={onClose}
        ref={closeRef}
        aria-label="Close"
      >
        <svg width="16" height="16" viewBox="0 0 12 12" aria-hidden="true">
          <path d="M2 2 L10 10 M10 2 L2 10" stroke="currentColor" strokeWidth="1.6" />
        </svg>
      </button>

      <button
        type="button"
        className="lb-btn lb-prev"
        onClick={(e) => {
          e.stopPropagation();
          onGo(index - 1);
        }}
        aria-label="Previous screenshot"
      >
        <Chevron d="M8 1.5 L3.5 6 L8 10.5" />
      </button>

      <figure className="lb-body" onClick={(e) => e.stopPropagation()}>
        <ConsoleShot {...row.shot} loading="eager" key={index} />
        <figcaption>
          <span className="lb-count">
            {index + 1} / {ROWS.length}
          </span>
          <h3>{row.title}</h3>
          <p>{row.body}</p>
        </figcaption>
      </figure>

      <button
        type="button"
        className="lb-btn lb-next"
        onClick={(e) => {
          e.stopPropagation();
          onGo(index + 1);
        }}
        aria-label="Next screenshot"
      >
        <Chevron d="M4 1.5 L8.5 6 L4 10.5" />
      </button>
    </div>,
    document.body,
  );
}

export default function ConsoleTour() {
  const [open, setOpen] = useState(null);
  const go = (n) => setOpen((n + ROWS.length) % ROWS.length);
  const close = () => setOpen(null);

  return (
    <Section id="console">
      <div className="tour-head">
        <h2>Central control over everything.</h2>
        <p className="lede">
          The web console is built into the backend. Administrators set the rules,
          choose the models, answer access requests and track spend, all in one
          place.
        </p>
      </div>

      <div className="tour">
        {ROWS.map((row, i) => (
          <div className={`tour-row${i % 2 ? " tour-row--flip" : ""}`} key={row.title}>
            <button
              type="button"
              className="tour-shot"
              onClick={() => setOpen(i)}
              aria-label={`Enlarge screenshot: ${row.title}`}
            >
              <ConsoleShot {...row.shot} />
            </button>
            <div className="tour-text">
              <h3>{row.title}</h3>
              <p>{row.body}</p>
              <ul>
                {row.bullets.map((b, j) => (
                  <li key={j}>{b}</li>
                ))}
              </ul>
            </div>
          </div>
        ))}
      </div>

      {open !== null ? (
        <Lightbox index={open} onClose={close} onGo={go} />
      ) : null}
    </Section>
  );
}
