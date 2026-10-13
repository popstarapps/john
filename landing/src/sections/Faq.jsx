import Section from "../components/Section.jsx";

const RISKS =
  "https://github.com/popstarapps/john/blob/main/readme.md#known-risks";

const QA = [
  {
    q: "We already run a coding agent in a Docker container. Why John?",
    a: [
      "A container limits what the agent can damage on the laptop. It does not control what leaves it.",
      <>
        <strong>Network:</strong> by default a container can reach the whole
        internet, and nothing inspects what it sends.{" "}
        <strong>Model access:</strong> the agent calls the provider directly,
        on a developer&apos;s key, with no redaction, routing or central
        record. <strong>Mounts:</strong> the usual conveniences (
        <code>~/.ssh</code>, cloud credentials, the Docker socket) hand the
        boundary back, and a bind mount lets the agent edit real files as it
        goes.
      </>,
      "John also isolates each project in a full virtual machine, a hardware boundary, rather than a container that shares the host's kernel.",
      "All of this can be built by hand: a hardened image, an inspecting proxy, a model gateway, central policy, and a way to keep every laptop up to date. John is that work, done once.",
    ],
  },
  {
    q: "What is John?",
    a: "A coding agent, like Claude Code or Codex, plus the controls around it. The agent runs on a backend the organization hosts; the client on each developer's machine carries out its commands inside a sandbox. Administrators decide which models it uses: internally hosted, external, or both.",
  },
  {
    q: "How is this different from an enterprise plan of Claude Code, Copilot or Codex?",
    a: "Enterprise plans add SSO, seat management, an admin console and retention terms. The agent still runs on the laptop with the developer's files and credentials, its network traffic is not inspected, and the records live with the vendor. John runs the agent on the organization's own backend, sandboxes every command, checks every outbound request against policy, and keeps the records in-house.",
  },
  {
    q: "Does it slow developers down?",
    a: "It is designed not to. Reading, editing and running commands never ask for approval. A developer is interrupted only when a policy says to ask about a specific request. Blocked requests can be turned into an access request on the spot. Each project keeps its sandbox, with tools, dependencies and build caches intact, between sessions.",
  },
  {
    q: "How is the sandbox set up?",
    a: [
      <>
        From a <code>.john/Dockerfile</code> in the project, or a built-in
        default image. Compilers, test runners and internal packages are
        installed before the agent starts, so the agent can work with the
        network off.
      </>,
      "Image builds can reach the network by default, so they can install tooling. Administrators can instead send build traffic through the proxy, to record it or to apply the same policies as the agent.",
      <>
        Each sandbox has default CPU, memory, disk and process limits. A
        developer can raise them for a project with{" "}
        <code>/resources</code>.
      </>,
    ],
  },
  {
    q: "How do the agent's changes reach my files, and can it delete my work?",
    a: [
      <>
        The project is mounted copy-on-write. Every edit and deletion lands in
        the sandbox, not on disk. <code>john apply</code> (or{" "}
        <code>/apply</code>) shows the diff and writes the changes the developer
        picks, all of them or by path.
      </>,
      "So the agent cannot touch uncommitted work. Unapplied changes stay in the sandbox between sessions, and are discarded if the sandbox is reset.",
    ],
  },
  {
    q: "How does the network proxy work, and can it be bypassed?",
    a: [
      "The sandbox has no direct network access. Its only way out is a proxy that runs outside the VM, on the developer's machine. Protocols other than HTTP and HTTPS are blocked.",
      "The proxy terminates TLS so it can check the whole request, not just the hostname. The certificate authority for this is created on the developer's machine for each sandbox and never leaves it.",
      "Every request is checked on its own and gets one of four outcomes: allowed, denied, held for the developer to approve, or, if no policy covers it, blocked. Each decision is recorded in the session trace.",
    ],
  },
  {
    q: "What can a policy express?",
    a: [
      "Policies match the details of a request or a path, so they can be precise. For example:",
      <>
        <strong>&ldquo;Install packages, but never publish them.&rdquo;</strong>{" "}
        Allow <code>GET</code> on the registry. The publish endpoint matches
        nothing, so it is blocked.
      </>,
      <>
        <strong>&ldquo;The payments code stays on internal models.&rdquo;</strong>{" "}
        A routing keyword moves any session that mentions it to internally
        hosted models.
      </>,
      <>
        <strong>&ldquo;Nothing touches .git or CI.&rdquo;</strong> Those paths
        are excluded from the sandbox or mounted read-only.
      </>,
      <>
        <strong>&ldquo;Contractors get less than staff.&rdquo;</strong> Their
        team carries fewer policy groups, on top of a baseline no team can
        weaken.
      </>,
    ],
  },
  {
    q: "Does our source code go to a model provider?",
    a: "Only where policy allows. Every model call is made by the backend, which first applies replacements (redacting secrets, aliasing company names) and then checks routing keywords. A keyword match moves the rest of the session to the models its policy allows, usually internal ones, and it never moves back. Replacements cannot catch every sensitive value, so restricted projects should use internal models only.",
  },
  {
    q: "Can John be prompt-injected? What are the known risks?",
    a: [
      "Yes. John assumes injected instructions will reach the model, from files, dependencies, command output or web content. The defence is that the model cannot decide anything that matters: the sandbox, the proxy and the backend do.",
      <>
        The main remaining risks: <strong>sensitive data reaching a
        provider</strong> (high), since redaction is not perfect;{" "}
        <strong>prompt injection</strong> itself (high), which is contained,
        not prevented; <strong>changes applied without real review</strong>{" "}
        (medium); and <strong>abuse of an allowed destination</strong>{" "}
        (medium), which is why policies should default to read-only methods and
        narrow paths.
      </>,
      <>
        The full list and threat model are in the{" "}
        <a className="ext" href={RISKS} rel="noreferrer" target="_blank">
          readme&apos;s security section
        </a>
        .
      </>,
    ],
  },
  {
    q: "Does John support MCP servers?",
    a: "Not yet. Today the agent can read files, edit files and run commands in the sandbox, and nothing else. MCP would fit the model as long as the agent can't choose its own tools. Administrators would approve which servers are allowed in the console, the same way they control models and network access, and the agent could never add one. Until then, anything a project needs can be installed in the sandbox image.",
  },
  {
    q: "Who can read the session traces?",
    a: [
      "Every session is recorded and encrypted. Administrators read traces with the organization's private key, which is kept outside the backend; decryption happens in the browser. Developers can read their own sessions with a key kept on their machine.",
      "The backend can only append to a session while it is running. To resume a conversation, the client hands back the key, never the conversation itself. Traces are kept until someone deletes them.",
    ],
  },
  {
    q: "Does John help with compliance?",
    a: [
      "John is not a certification, but it turns audit questions into records: who used the agent, what they were allowed to do, what left the machine, where code was sent, and who changed the controls.",
      "Every policy and setting change is versioned in a change history showing who did what and when, readable without unlocking any session. Developers can request access, but only administrators can grant it, and the baseline cannot be weakened by any team.",
    ],
  },
  {
    q: "What happens if the backend goes down?",
    a: "John fails closed. Prompts stop at once, because the agent runs on the backend. Network policies are cached for a few minutes so a running command can finish; after that, anything that needs a policy decision is blocked until the backend is back.",
  },
  {
    q: "What does it take to run?",
    a: [
      "The backend is one Linux service with a SQLite database. No cluster, no managed database, and no vendor cloud in the path: the only outside party is whichever model provider the policies allow.",
      "The client runs on macOS (Apple Silicon and Intel), Windows (x86-64) and Linux (x86-64 and arm64), using the built-in hypervisor on each to run the sandboxes.",
    ],
  },
];

export default function Faq() {
  return (
    <Section id="faq">
      <h2>Before you ask.</h2>

      <div className="faq" style={{ marginTop: 32 }}>
        {QA.map((item, i) => (
          <details key={item.q} open={i === 0}>
            <summary>{item.q}</summary>
            <div className="a">
              {Array.isArray(item.a) ? (
                item.a.map((para, j) => <p key={j}>{para}</p>)
              ) : (
                <p>{item.a}</p>
              )}
            </div>
          </details>
        ))}
      </div>
    </Section>
  );
}
