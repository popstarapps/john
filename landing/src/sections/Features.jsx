import Section from "../components/Section.jsx";

/**
 * The full list for developers, security, audit and IT. Deliberately
 * exhaustive: this is the section a careful evaluator reads line by line.
 *
 * Icons are simple line glyphs on a 24-unit grid, stroked in the page's
 * colour; each item names one.
 */
const ICONS = {
  terminal: "M4 17 L10 11 L4 5 M12 19 H20",
  box: "M12 3 L20 7.5 V16.5 L12 21 L4 16.5 V7.5 Z M4 7.5 L12 12 L20 7.5 M12 12 V21",
  package:
    "M16.5 9.4 L7.5 4.2 M21 16 V8 L12 3 L3 8 V16 L12 21 Z M3.3 7 L12 12 L20.7 7 M12 21 V12",
  refresh: "M23 4 V10 H17 M20.5 15 A9 9 0 1 1 18.4 5.6 L23 10",
  link: "M10 13 A5 5 0 0 0 17.5 13.5 L20.5 10.5 A5 5 0 0 0 13.5 3.5 L11.8 5.2 M14 11 A5 5 0 0 0 6.5 10.5 L3.5 13.5 A5 5 0 0 0 10.5 20.5 L12.2 18.8",
  merge:
    "M6 21 V9 A9 9 0 0 0 15 18 M9 6 A3 3 0 1 1 3 6 A3 3 0 1 1 9 6 M21 18 A3 3 0 1 1 15 18 A3 3 0 1 1 21 18",
  bellOff:
    "M13.7 21 A2 2 0 0 1 10.3 21 M18.6 13 A17.9 17.9 0 0 1 18 8 M6.3 6.3 A5.9 5.9 0 0 0 6 8 C6 15 3 17 3 17 H17 M18 8 A6 6 0 0 0 8.7 3 M2 2 L22 22",
  at: "M16 12 A4 4 0 1 1 8 12 A4 4 0 1 1 16 12 M16 8 V13 A3 3 0 0 0 22 13 V12 A10 10 0 1 0 18.1 19.9",
  resume: "M1 4 V10 H7 M3.5 15 A9 9 0 1 0 5.6 5.6 L1 10",
  message:
    "M21 15 A2 2 0 0 1 19 17 H7 L3 21 V5 A2 2 0 0 1 5 3 H19 A2 2 0 0 1 21 5 Z",
  sliders:
    "M4 21 V14 M4 10 V3 M12 21 V12 M12 8 V3 M20 21 V16 M20 12 V3 M1 14 H7 M9 8 H15 M17 16 H23",
  cpu: "M6 4 H18 A2 2 0 0 1 20 6 V18 A2 2 0 0 1 18 20 H6 A2 2 0 0 1 4 18 V6 A2 2 0 0 1 6 4 Z M9 9 H15 V15 H9 Z M9 1 V4 M15 1 V4 M9 20 V23 M15 20 V23 M20 9 H23 M20 14 H23 M1 9 H4 M1 14 H4",
  chart: "M12 20 V10 M18 20 V4 M6 20 V16",
  monitor:
    "M4 3 H20 A2 2 0 0 1 22 5 V15 A2 2 0 0 1 20 17 H4 A2 2 0 0 1 2 15 V5 A2 2 0 0 1 4 3 Z M8 21 H16 M12 17 V21",
  folder:
    "M22 19 A2 2 0 0 1 20 21 H4 A2 2 0 0 1 2 19 V5 A2 2 0 0 1 4 3 H9 L11 6 H20 A2 2 0 0 1 22 8 Z",
  lock: "M5 11 H19 A2 2 0 0 1 21 13 V20 A2 2 0 0 1 19 22 H5 A2 2 0 0 1 3 20 V13 A2 2 0 0 1 5 11 Z M7 11 V7 A5 5 0 0 1 17 7 V11",
  key: "M12 15.5 A4.5 4.5 0 1 1 3 15.5 A4.5 4.5 0 1 1 12 15.5 M10.7 12.3 L21 2 M16 7 L19 10 M19 4 L21 6",
  globe:
    "M22 12 A10 10 0 1 1 2 12 A10 10 0 1 1 22 12 M2 12 H22 M12 2 A15.3 15.3 0 0 1 16 12 A15.3 15.3 0 0 1 12 22 A15.3 15.3 0 0 1 8 12 A15.3 15.3 0 0 1 12 2 Z",
  award:
    "M19 8 A7 7 0 1 1 5 8 A7 7 0 1 1 19 8 M8.2 13.9 L7 23 L12 20 L17 23 L15.8 13.9",
  filter: "M22 3 H2 L10 12.5 V19 L14 21 V12.5 Z",
  ban: "M22 12 A10 10 0 1 1 2 12 A10 10 0 1 1 22 12 M4.9 4.9 L19.1 19.1",
  userCheck:
    "M16 21 V19 A4 4 0 0 0 12 15 H5 A4 4 0 0 0 1 19 V21 M12.5 7 A4 4 0 1 1 4.5 7 A4 4 0 1 1 12.5 7 M17 11 L19 13 L23 9",
  eyeOff:
    "M17.9 17.9 A10 10 0 0 1 12 20 C5 20 1 12 1 12 A18.5 18.5 0 0 1 6.1 6.1 M9.9 4.2 A9.1 9.1 0 0 1 12 4 C19 4 23 12 23 12 A18.5 18.5 0 0 1 20.8 15.2 M14.1 14.1 A3 3 0 1 1 9.9 9.9 M1 1 L23 23",
  shuffle: "M16 3 H21 V8 M4 20 L21 3 M21 16 V21 H16 M15 15 L21 21 M4 4 L9 9",
  layers: "M12 2 L2 7 L12 12 L22 7 Z M2 17 L12 22 L22 17 M2 12 L12 17 L22 12",
  anchor:
    "M15 5 A3 3 0 1 1 9 5 A3 3 0 1 1 15 5 M12 22 V8 M5 12 H2 A10 10 0 0 0 22 12 H19",
  server:
    "M5.5 4 H18.5 A1.5 1.5 0 0 1 20 5.5 V9.5 A1.5 1.5 0 0 1 18.5 11 H5.5 A1.5 1.5 0 0 1 4 9.5 V5.5 A1.5 1.5 0 0 1 5.5 4 Z M5.5 13 H18.5 A1.5 1.5 0 0 1 20 14.5 V18.5 A1.5 1.5 0 0 1 18.5 20 H5.5 A1.5 1.5 0 0 1 4 18.5 V14.5 A1.5 1.5 0 0 1 5.5 13 Z M8 7.5 H8.01 M8 16.5 H8.01",
  tool: "M14.7 6.3 A1 1 0 0 0 14.7 7.7 L16.3 9.3 A1 1 0 0 0 17.7 9.3 L21.5 5.5 A6 6 0 0 1 13.5 13.5 L6.6 20.4 A2.1 2.1 0 0 1 3.6 17.4 L10.5 10.5 A6 6 0 0 1 18.5 2.5 Z",
  paperclip:
    "M21.4 11.1 L12.3 20.2 A6 6 0 0 1 3.8 11.7 L13 2.5 A4 4 0 0 1 18.6 8.2 L9.4 17.4 A2 2 0 0 1 6.6 14.6 L15.1 6.1",
  plugOff:
    "M22 12 A10 10 0 1 1 2 12 A10 10 0 1 1 22 12 M9 7 V10 M15 7 V10 M7.5 10 H16.5 V12 A4.5 4.5 0 0 1 7.5 12 Z M12 16.5 V19",
  power: "M18.4 6.6 A9 9 0 1 1 5.6 6.6 M12 2 V12",
  check: "M22 12 A10 10 0 1 1 2 12 A10 10 0 1 1 22 12 M8 12 L11 15 L16 9",
  file: "M14 2 H6 A2 2 0 0 0 4 4 V20 A2 2 0 0 0 6 22 H18 A2 2 0 0 0 20 20 V8 Z M14 2 V8 H20 M16 13 H8 M16 17 H8",
  clock: "M22 12 A10 10 0 1 1 2 12 A10 10 0 1 1 22 12 M12 6 V12 L16 14",
  checkSquare:
    "M9 11 L12 14 L22 4 M21 12 V19 A2 2 0 0 1 19 21 H5 A2 2 0 0 1 3 19 V5 A2 2 0 0 1 5 3 H16",
  users:
    "M17 21 V19 A4 4 0 0 0 13 15 H5 A4 4 0 0 0 1 19 V21 M13 7 A4 4 0 1 1 5 7 A4 4 0 1 1 13 7 M23 21 V19 A4 4 0 0 0 20 15.1 M16 3.1 A4 4 0 0 1 16 10.9",
  user: "M20 21 V19 A4 4 0 0 0 16 15 H8 A4 4 0 0 0 4 19 V21 M16 7 A4 4 0 1 1 8 7 A4 4 0 1 1 16 7",
  archive: "M21 8 V21 H3 V8 M1 3 H23 V8 H1 Z M10 12 H14",
  window:
    "M5 4 H19 A2 2 0 0 1 21 6 V18 A2 2 0 0 1 19 20 H5 A2 2 0 0 1 3 18 V6 A2 2 0 0 1 5 4 Z M3 9 H21 M7 6.5 H7.01",
  login:
    "M15 3 H19 A2 2 0 0 1 21 5 V19 A2 2 0 0 1 19 21 H15 M10 17 L15 12 L10 7 M15 12 H3",
  download:
    "M21 15 V19 A2 2 0 0 1 19 21 H5 A2 2 0 0 1 3 19 V15 M7 10 L12 15 L17 10 M12 15 V3",
  sync: "M23 4 V10 H17 M1 20 V14 H7 M3.5 9 A9 9 0 0 1 18.4 5.6 L23 10 M1 14 L5.6 18.4 A9 9 0 0 0 20.5 15",
  drive:
    "M22 12 H2 M5.5 5.1 L2 12 V18 A2 2 0 0 0 4 20 H20 A2 2 0 0 0 22 18 V12 L18.5 5.1 A2 2 0 0 0 16.8 4 H7.2 A2 2 0 0 0 5.5 5.1 Z M6 16 H6.01 M10 16 H10.01",
  shield: "M12 22 C12 22 20 18 20 12 V5 L12 2 L4 5 V12 C4 18 12 22 12 22 Z",
  laptop:
    "M5.5 5 H18.5 A1.5 1.5 0 0 1 20 6.5 V16 H4 V6.5 A1.5 1.5 0 0 1 5.5 5 Z M2 19 H22",
};

const GROUPS = [
  {
    title: "For developers",
    items: [
      [
        "terminal",
        "Reads, edits, runs and tests",
        "Change files, run the build and the tests, and iterate, all inside a real Linux machine.",
      ],
      [
        "box",
        "A full Linux machine per project",
        "Not a restricted shell: install compilers, package managers and system libraries the way the project needs them.",
      ],
      [
        "package",
        "Toolchain from the project's Dockerfile",
        <>
          <code>.john/Dockerfile</code> defines the sandbox, so everything is
          installed before the agent starts.
        </>,
      ],
      [
        "refresh",
        "Warm sandboxes",
        "Tools, dependencies and build caches survive between sessions, until the sandbox is reset.",
      ],
      [
        "link",
        "Same paths as the host",
        "The project has the same path inside the sandbox, so stack traces and build caches line up.",
      ],
      [
        "merge",
        "Review the diff, apply what's picked",
        <>
          <code>john apply</code> writes all of the changes, or just the paths
          selected.
        </>,
      ],
      [
        "bellOff",
        "No approval prompts",
        "Reading, editing and running commands never stop to ask.",
      ],
      [
        "at",
        "Mention files, images and PDFs",
        <>
          Point the agent at a file with <code>@</code>, or attach a screenshot
          or document to the prompt.
        </>,
      ],
      [
        "resume",
        "Resume earlier conversations",
        <>
          <code>/resume</code> lists earlier sessions in the project and picks
          one back up.
        </>,
      ],
      [
        "message",
        "Ask for access on the spot",
        "When a request is blocked, file an access request from the prompt and follow it in the console.",
      ],
      [
        "sliders",
        "More CPU or memory when needed",
        <>
          <code>/resources cpu=8 memory=16</code> for the project that needs it,
          with no administrator involved.
        </>,
      ],
      [
        "cpu",
        "Choice of approved models",
        <>
          Switch between the internal and external models the team is allowed,
          with <code>/model</code>.
        </>,
      ],
      [
        "chart",
        "A console for developers too",
        "Personal usage and spend, team memberships, and exactly which policies apply.",
      ],
      [
        "monitor",
        "One CLI on every platform",
        <>
          macOS, Windows and Linux, signed in once with <code>john login</code>.
        </>,
      ],
    ],
  },
  {
    title: "For security teams",
    items: [
      [
        "box",
        "A virtual machine per project",
        "Hardware-level isolation, one VM per working directory, with no sharing between them.",
      ],
      [
        "folder",
        "Only the working directory is mounted",
        "No home folder, no other repositories, no keychain.",
      ],
      [
        "lock",
        "Protected paths",
        <>
          Exclude paths, mount them read-only, or block deletion:{" "}
          <code>.env</code>, <code>.git</code>, CI workflows.
        </>,
      ],
      [
        "key",
        "No host credentials",
        "No SSH agent, environment variables, Docker socket or devices inside the sandbox.",
      ],
      [
        "globe",
        "Proxy-only network",
        "HTTP and HTTPS through the proxy. Every other protocol and direct connection is blocked.",
      ],
      [
        "award",
        "Checks the request, not the hostname",
        "TLS is terminated on the developer's machine, with a per-sandbox certificate authority that never leaves it.",
      ],
      [
        "filter",
        "Request-level rules",
        "Match method, host, port, path, selected headers and query parameters, and cap body sizes.",
      ],
      [
        "ban",
        "Unknown means blocked",
        "Allow, deny, or ask the developer. Anything no policy covers is blocked.",
      ],
      [
        "userCheck",
        "Approve one request, not a host",
        "An ask-developer match shows the whole buffered request, and approves only that one.",
      ],
      [
        "eyeOff",
        "Redaction and aliasing",
        "Regex replacements run on everything bound for a model, before routing.",
      ],
      [
        "shuffle",
        "Keyword routing",
        "A listed keyword in a prompt, file or command output moves the session to internal models.",
      ],
      [
        "layers",
        "Policy groups",
        "Controls bundled once and attached to teams, instead of copied per team.",
      ],
      [
        "anchor",
        "A baseline no team can weaken",
        "Attached to every team. Where policies conflict, the most restrictive one wins.",
      ],
      [
        "server",
        "The agent runs on the backend",
        "Clients hold no model credentials, so there is no second path to a model.",
      ],
      [
        "tool",
        "Governed image builds",
        "Optionally send build traffic through the proxy, to record it or apply the same policies.",
      ],
      [
        "paperclip",
        "Attachment controls",
        "By default, images and PDFs are refused in sessions under redaction or routing rules, since text rules can't read them.",
      ],
      [
        "plugOff",
        "The agent can't add its own tools",
        "Read, edit and run is the whole surface. MCP isn't supported yet; when it is, only servers an administrator approves will load.",
      ],
      [
        "check",
        "Signed policy snapshots",
        <>
          Clients enforce a signed copy of their policies, visible with{" "}
          <code>/policies</code>.
        </>,
      ],
      [
        "power",
        "Fails closed",
        "No policy decision, no operation. A short cache rides out brief outages.",
      ],
    ],
  },
  {
    title: "For audit and compliance",
    items: [
      [
        "file",
        "Encrypted session traces",
        "A complete record of every session, each encrypted with its own key.",
      ],
      [
        "key",
        "A read key the backend never holds",
        "Traces decrypt in the browser. A stolen database or backup reveals nothing.",
      ],
      [
        "clock",
        "Change history",
        "Who changed which policy, group, request or setting, and when, readable without unlocking any trace.",
      ],
      [
        "checkSquare",
        "Every network decision recorded",
        "Including the requests a developer approved by hand.",
      ],
      [
        "users",
        "Separation of duties",
        "Developers request access, administrators decide. Nobody approves their own exception.",
      ],
      [
        "paperclip",
        "Attachments on the record",
        "Name, type, size and SHA-256 of every file sent to a model.",
      ],
      [
        "user",
        "Developers can read their own sessions",
        "With a key kept on their own machine.",
      ],
      [
        "archive",
        "Kept until deliberately deleted",
        "Traces never expire on their own.",
      ],
    ],
  },
  {
    title: "For IT operations",
    items: [
      [
        "server",
        "One binary, one SQLite database",
        "No cluster, no queue, no second service. Scale it vertically.",
      ],
      [
        "window",
        "Browser-based setup",
        "First run prints a one-time setup URL. Everything else lives in the database, with no config files.",
      ],
      [
        "login",
        "SSO with any OIDC provider",
        "Sign-in, teams and roles through the organization's own identity provider.",
      ],
      [
        "globe",
        "Behind the existing reverse proxy",
        "Listens on localhost; put any HTTPS reverse proxy in front of it.",
      ],
      [
        "download",
        "Clients served by the backend",
        "Developers download the right build for macOS, Windows or Linux from the console.",
      ],
      [
        "sync",
        "Clients stay in step",
        "A client that doesn't match the backend's version is offered the matching build before a session starts.",
      ],
      [
        "shield",
        "Credentials in the OS keychain",
        "Login tokens and registry passwords go to Keychain, Credential Manager or the desktop keyring.",
      ],
      [
        "package",
        "Private registries",
        <>
          Uses existing Docker credential helpers, or{" "}
          <code>john registry login</code>. Credentials stay on the host.
        </>,
      ],
      [
        "chart",
        "Usage and spend",
        "Per team, model and person, over 7, 30 or 60 days.",
      ],
      [
        "drive",
        "Simple backups",
        "The database and the trace directory are the whole state.",
      ],
      [
        "cpu",
        "Built-in hypervisors",
        "Virtualization.framework on macOS, Hyper-V on Windows, KVM with libkrun on Linux.",
      ],
      [
        "laptop",
        "Standalone mode",
        "Try it on one laptop with an Anthropic, OpenAI or Gemini API key.",
      ],
    ],
  },
];

export default function Features() {
  return (
    <Section id="features">
      <div className="details-head">
        <h2>All the little details.</h2>
        <p className="lede">
          The full list for developers, security, audit and IT.
        </p>
      </div>

      <div className="details">
        {GROUPS.map((group) => (
          <div className="details-group" key={group.title}>
            <div className="details-title">
              <h3>{group.title}</h3>
              <span className="count">{group.items.length}</span>
            </div>
            <ul>
              {group.items.map(([icon, name, body]) => (
                <li key={name}>
                  <svg viewBox="0 0 24 24" aria-hidden="true">
                    <path d={ICONS[icon]} />
                  </svg>
                  <div>
                    <strong>{name}</strong>
                    <span>{body}</span>
                  </div>
                </li>
              ))}
            </ul>
          </div>
        ))}
      </div>
    </Section>
  );
}
