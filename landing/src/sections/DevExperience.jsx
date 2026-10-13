import Section from "../components/Section.jsx";

/**
 * The developer's half of the pitch, right after the organization's.
 *
 * Each point carries a small drawing of the thing it describes — a dialog, a
 * terminal, a diff — set in the page's own tokens so it follows the theme.
 * They are illustrations, not screenshots, and stay hidden from screen
 * readers; the headline and body carry the claim.
 */

function Dialog() {
  return (
    <div className="dx-dialog">
      <div className="dx-dialog-box">
        <span>Write src/app.go?</span>
        <span className="dx-dialog-buttons">
          <b>Yes</b>
          <b>No</b>
        </span>
      </div>
      <div className="dx-dialog-box">
        <span>Run go test ./...?</span>
        <span className="dx-dialog-buttons">
          <b>Yes</b>
          <b>No</b>
        </span>
      </div>
      <svg className="dx-strike" viewBox="0 0 100 100" preserveAspectRatio="none">
        <line x1="6" y1="92" x2="94" y2="8" />
      </svg>
    </div>
  );
}

function Terminal() {
  return (
    <div className="dx-term">
      <div>
        <span className="dx-dim">$</span> apt-get install -y golang
      </div>
      <div>
        <span className="dx-dim">$</span> go test ./...
      </div>
      <div className="dx-ok">ok  example.com/app  1.42s</div>
    </div>
  );
}

function Diff() {
  return (
    <div className="dx-diff">
      <div className="dx-file">
        <span className="dx-box dx-box--on" /> src/app.go
      </div>
      <div className="dx-del">- return nil</div>
      <div className="dx-add">+ return fmt.Errorf("empty")</div>
      <div className="dx-file">
        <span className="dx-box" /> scratch.txt
      </div>
    </div>
  );
}

function Dockerfile() {
  return (
    <div className="dx-term dx-term--file">
      <div className="dx-tab">.john/Dockerfile</div>
      <div>
        <span className="dx-kw">FROM</span> debian:bookworm
      </div>
      <div>
        <span className="dx-kw">RUN</span> apt-get install -y golang nodejs
      </div>
      <div>
        <span className="dx-kw">RUN</span> npm install -g pnpm
      </div>
    </div>
  );
}

function Paths() {
  return (
    <div className="dx-paths">
      <div className="dx-path">
        <span className="dx-label">your machine</span>
        /Users/you/app
      </div>
      <span className="dx-eq">=</span>
      <div className="dx-path">
        <span className="dx-label">sandbox</span>
        /Users/you/app
      </div>
    </div>
  );
}

function Blocked() {
  return (
    <div className="dx-term">
      <div className="dx-deny">✕ GET registry.example.com blocked</div>
      <div>No policy covers this request.</div>
      <div>
        Request access? <span className="dx-ok">[y]</span>/n
      </div>
    </div>
  );
}

const POINTS = [
  {
    art: Dialog,
    title: "No approval fatigue",
    body: "Commands run in the sandbox without asking, on an overlay filesystem. You choose when changes reach your machine.",
  },
  {
    art: Terminal,
    title: "A real Linux machine",
    body: "Install compilers, packages and system libraries, and run the full test suite. Not a restricted shell.",
  },
  {
    art: Diff,
    title: "Review the diff, apply what you want",
    body: "The agent's changes collect in the sandbox. Pick files or apply everything in one go.",
  },
  {
    art: Dockerfile,
    title: "The project defines its toolchain",
    body: "A .john/Dockerfile gives every developer the same ready-to-go environment.",
  },
  {
    art: Paths,
    title: "Same paths as your machine",
    body: "Your project has the same path inside the sandbox, so stack traces and build caches line up.",
  },
  {
    art: Blocked,
    title: "Blocked isn't a dead end",
    body: "When a request isn't covered by policy, request access right from the prompt.",
  },
];

export default function DevExperience() {
  return (
    <Section id="developers">
      <h2>An awesome developer experience.</h2>
      <p className="lede">
        If you've used Claude Code, Codex or OpenCode, you already know how to
        use John. Type <code>john</code> in a project, sign in once with SSO,
        and start working.
      </p>

      <div className="dx-grid">
        {POINTS.map(({ art: Art, title, body }) => (
          <div className="dx-card" key={title}>
            <div className="dx-art" aria-hidden="true">
              <Art />
            </div>
            <div className="dx-text">
              <h3>{title}</h3>
              <p>{body}</p>
            </div>
          </div>
        ))}
      </div>
    </Section>
  );
}
