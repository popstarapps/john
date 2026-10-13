import { useEffect, useRef, useState } from "react";

/**
 * The whole system, plus the path a single prompt takes through it.
 *
 * The argument the picture has to make is the one thing about John that is
 * genuinely unusual: the agent loop runs on the backend. The client is a thin
 * executor. That is why every model call is governed by construction rather
 * than by a client that could be patched, and it is why the flow crosses the
 * client/backend line four times instead of once.
 *
 * The second argument is the border. Everything above the firewall line is
 * yours; below it is the open internet. Exactly two arrows cross, and each has
 * a guard immediately before it — the model router, and the proxy.
 *
 * The nodes carry names only. What each one is doing at a given moment is the
 * job of the caption box, which parks next to whatever is lit. Geometry is hand-placed; the viewBox starts at VX/VY rather than 0 so
 * the layout could be tightened without renumbering every node. Node ids are
 * shared between STEPS and the markup, so adding a beat means naming nodes,
 * not moving anything.
 */

/* ---- edges, written in the direction the packet travels ---- */
const EDGES = {
  "dev-cli": "M 230 126 L 230 190",
  "cli-dev": "M 230 190 L 230 126",
  "cli-up": "M 396 208 L 492 208",
  "be-down": "M 492 226 L 396 226",
  "cli-sandbox": "M 92 242 L 92 274",
  // The host tree is read upward into the VM. The only path back down is the
  // apply, and it stays inert until the developer pulls it.
  "workdir-sandbox": "M 180 416 L 180 378",
  "overlay-workdir": "M 106 356 L 106 416",
  "sandbox-proxy": "M 317 378 L 317 416",
  "proxy-internet": "M 317 468 L 317 680",
  "loop-policy": "M 532 242 L 532 300",
  "policy-router": "M 532 352 L 532 416",
  "router-internal": "M 532 468 L 532 522",
  "router-external": "M 720 468 L 720 680",
};

// Drawn dashed, because it is the one edge that carries nothing until a person
// decides it should.
const DASHED_EDGES = ["overlay-workdir"];

const STEPS = [
  {
    n: "01",
    title: "You write a prompt",
    body: "Run john in a project and describe the task. Nothing has left your machine yet.",
    nodes: ["dev", "cli"],
    edges: ["dev-cli"],
    box: [440, 150],
    ms: 3200,
  },
  {
    n: "02",
    title: "The prompt goes to your backend",
    body: "The CLI sends it up, signed in with SSO. The agent itself doesn't run on the laptop.",
    nodes: ["cli", "loop"],
    edges: ["cli-up"],
    box: [300, 262],
    ms: 3400,
  },
  {
    n: "03",
    title: "Your policies apply",
    body: "Your team's policies are looked up, and secrets and internal names are redacted.",
    nodes: ["loop", "policy"],
    edges: ["loop-policy"],
    box: [170, 282],
    ms: 3800,
  },
  {
    n: "04",
    title: "The backend picks the model",
    body: "It chooses from the models your team is allowed to use. Sensitive work is routed to an internal one.",
    nodes: ["policy", "router"],
    edges: ["policy-router"],
    box: [170, 370],
    ms: 3600,
  },
  {
    n: "05",
    title: "Internal model: nothing leaves",
    body: "A model you host yourself. The request never leaves your network.",
    nodes: ["router", "internal"],
    edges: ["router-internal"],
    box: [170, 500],
    ms: 3200,
  },
  {
    n: "06",
    title: "External model, only if allowed",
    body: "A provider on the internet, reached only when your policies allow it.",
    nodes: ["router", "external"],
    edges: ["router-external"],
    warn: ["external"],
    warnEdges: ["router-external"],
    box: [320, 520],
    ms: 3800,
  },
  {
    n: "07",
    title: "The model asks for a tool call",
    body: "An edit to a file, say. The backend sends it down to the CLI.",
    nodes: ["loop", "cli"],
    edges: ["be-down"],
    box: [300, 262],
    ms: 3600,
  },
  {
    n: "08",
    title: "The sandbox runs it",
    body: "Commands are run inside the Linux VM.",
    nodes: ["cli", "sandbox"],
    edges: ["cli-sandbox"],
    box: [440, 262],
    ms: 3400,
  },
  {
    n: "09",
    title: "Changes go to an overlay",
    body: "The sandbox reads your real files, but every change lands in a layer inside the VM.",
    nodes: ["sandbox", "overlay", "workdir"],
    edges: ["workdir-sandbox"],
    box: [440, 290],
    ms: 3800,
  },
  {
    n: "10",
    title: "Network access goes through the proxy",
    body: "Every outbound request is checked against policy. Anything not allowed is blocked.",
    nodes: ["sandbox", "proxy", "internet"],
    edges: ["sandbox-proxy", "proxy-internet"],
    warn: ["internet"],
    warnEdges: ["proxy-internet"],
    box: [420, 380],
    ms: 4000,
  },
  {
    n: "11",
    title: "The result goes back up",
    body: "The output returns to the backend, and the loop continues.",
    nodes: ["sandbox", "cli", "loop"],
    edges: ["cli-up"],
    box: [490, 290],
    ms: 3600,
  },
  {
    n: "12",
    title: "You review and apply",
    body: "john apply shows the changes and writes the ones you pick to your project. Nothing else touches your files.",
    nodes: ["dev", "cli", "sandbox", "overlay", "workdir"],
    edges: ["cli-dev", "overlay-workdir"],
    box: [440, 150],
    ms: 4400,
  },
];

const VX = 20;
const VY = 72;
const VW = 850;
const VH = 688;

/** Fires once, the first time the diagram is properly on screen. */
function useInView(ref) {
  const [seen, setSeen] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!el || typeof IntersectionObserver === "undefined") return;
    const io = new IntersectionObserver(
      (entries) => {
        entries.forEach((entry) => {
          if (entry.isIntersecting) {
            setSeen(true);
            io.disconnect();
          }
        });
      },
      { threshold: 0.3 },
    );
    io.observe(el);
    return () => io.disconnect();
  }, [ref]);
  return seen;
}

export default function SystemFlow() {
  const ref = useRef(null);
  const inView = useInView(ref);
  const [i, setI] = useState(0);
  const [playing, setPlaying] = useState(false);
  // Until the sequence starts the diagram is shown whole, with no card over
  // it. That is also the state a prerendered or scripting-free visitor gets,
  // which is why the step text is repeated in the list below the stage.
  const [started, setStarted] = useState(false);

  useEffect(() => {
    if (!inView) return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
    setStarted(true);
    setPlaying(true);
  }, [inView]);

  useEffect(() => {
    if (!playing) return;
    const t = setTimeout(
      () => setI((n) => (n + 1) % STEPS.length),
      STEPS[i].ms,
    );
    return () => clearTimeout(t);
  }, [playing, i]);

  const step = STEPS[i];


  const go = (n) => {
    setStarted(true);
    setPlaying(false);
    setI((n + STEPS.length) % STEPS.length);
  };
  const toggle = () => {
    setStarted(true);
    setPlaying((p) => !p);
  };

  const isOn = (id) => started && step.nodes.includes(id);
  const isWarn = (id) => (step.warn || []).includes(id);
  // The agent loop is what writes the trace, so the trace lights with the loop
  // rather than appearing in the script as a step of its own.
  const traceOn = started && step.nodes.includes("loop");

  const nodeCls = (id) => {
    const on = id === "trace" ? traceOn : isOn(id);
    if (!started) return "node";
    if (!on) return "node is-dim";
    return isWarn(id) ? "node is-on is-on--warn" : "node is-on";
  };

  const edgeCls = (id) => {
    const dash = DASHED_EDGES.includes(id) ? " dash" : "";
    if (!started || !step.edges.includes(id)) return `f-edge${dash}`;
    const tone = (step.warnEdges || []).includes(id) ? " warn" : " on";
    return `f-edge${tone}${dash}`;
  };

  // An arrow's label shows only while that arrow is carrying something, and
  // lights with it. Before the walkthrough starts the diagram is shown whole,
  // labels included.
  const edgeLabel = (id) => {
    if (!started) return "t-edge";
    if (!step.edges.includes(id)) return "t-edge is-hidden";
    return (step.warnEdges || []).includes(id)
      ? "t-edge is-on is-on--warn"
      : "t-edge is-on";
  };


  return (
    <div className="flow" ref={ref}>
      <div className="flow-bar">
        <div className="flow-controls">
          <div className="flow-btns">
            <button
              type="button"
              className="icon-btn"
              onClick={() => go(i - 1)}
              aria-label="Previous step"
            >
              <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
                <path
                  d="M8.5 1 L3.5 6 L8.5 11"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.6"
                />
              </svg>
            </button>
            <button
              type="button"
              className="icon-btn"
              onClick={toggle}
              aria-label={playing ? "Pause the walkthrough" : "Play the walkthrough"}
            >
              {playing ? (
                <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
                  <rect x="2.5" y="1.5" width="2.6" height="9" fill="currentColor" />
                  <rect x="6.9" y="1.5" width="2.6" height="9" fill="currentColor" />
                </svg>
              ) : (
                <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
                  <path d="M3 1.5 L10.5 6 L3 10.5 z" fill="currentColor" />
                </svg>
              )}
            </button>
            <button
              type="button"
              className="icon-btn"
              onClick={() => go(i + 1)}
              aria-label="Next step"
            >
              <svg width="12" height="12" viewBox="0 0 12 12" aria-hidden="true">
                <path
                  d="M3.5 1 L8.5 6 L3.5 11"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.6"
                />
              </svg>
            </button>
          </div>

          <div className="flow-ticks">
            {STEPS.map((s, n) => (
              <button
                type="button"
                key={s.n}
                className={`flow-tick${n === i && started ? " is-here" : ""}${
                  (s.warn || []).length ? " is-warn" : ""
                }`}
                onClick={() => go(n)}
                aria-label={`Step ${s.n}: ${s.title}`}
                aria-current={n === i && started ? "step" : undefined}
              />
            ))}
          </div>
        </div>
      </div>

      <div className="flow-stage">
        <div className="flow-canvas">
          {/* The step's text, in a box parked next to where it happens.
              Positions are viewBox coordinates, turned into percentages so
              the box tracks the drawing as it scales. */}
          <div
            className="flow-caption"
            style={{
              left: `${((step.box[0] - VX) / VW) * 100}%`,
              top: `${((step.box[1] - VY) / VH) * 100}%`,
            }}
            aria-live="polite"
          >
            <strong>{step.title}</strong>
            <p>{step.body}</p>
          </div>

          <svg
            viewBox={`${VX} ${VY} ${VW} ${VH}`}
            role="img"
            aria-label="System architecture. Inside your internal network sit the developer machine and the self-hosted John backend. The John CLI executes tool calls inside a Linux sandbox, which reads the working directory but writes only to an overlay inside the virtual machine, and which can reach the network only through a local proxy. In the backend, the agent loop feeds the policy engine and the model router, and writes an encrypted session trace. The router reaches an internal model that stays inside the network. Below a firewall line is the open internet: external model providers and internet destinations. Exactly two arrows cross the border — the router's, and the proxy's."
          >
            <defs>
              <marker
                id="fa-n"
                viewBox="0 0 10 10"
                refX="9"
                refY="5"
                markerWidth="6.5"
                markerHeight="6.5"
                orient="auto-start-reverse"
              >
                <path d="M0 0 L10 5 L0 10 z" fill="var(--fg-muted)" />
              </marker>
              <marker
                id="fa-on"
                viewBox="0 0 10 10"
                refX="9"
                refY="5"
                markerWidth="6.5"
                markerHeight="6.5"
                orient="auto-start-reverse"
              >
                <path d="M0 0 L10 5 L0 10 z" fill="var(--allow)" />
              </marker>
              <marker
                id="fa-warn"
                viewBox="0 0 10 10"
                refX="9"
                refY="5"
                markerWidth="6.5"
                markerHeight="6.5"
                orient="auto-start-reverse"
              >
                <path d="M0 0 L10 5 L0 10 z" fill="var(--warn)" />
              </marker>
            </defs>

            <rect className="zone" x="40" y="140" width="380" height="352" rx="10" />
            <text className="t-sub" x="60" y="164">
              Developer machine
            </text>

            <rect className="zone" x="470" y="140" width="380" height="352" rx="10" />
            <text className="t-sub" x="490" y="164">
              John backend (self-hosted)
            </text>

            {/* ---------- edges, under the boxes ---------- */}
            {Object.entries(EDGES).map(([id, d]) => {
              const cls = edgeCls(id);
              const live = cls.includes(" on") || cls.includes(" warn");
              const marker = cls.includes(" warn")
                ? "url(#fa-warn)"
                : cls.includes(" on")
                  ? "url(#fa-on)"
                  : "url(#fa-n)";
              return (
                <g className={cls} key={id}>
                  <path className="f-line" d={d} markerEnd={marker} />
                  {live ? (
                    <path
                      className="f-pulse"
                      d={d}
                      pathLength="100"
                      key={`${id}-${i}`}
                    />
                  ) : null}
                </g>
              );
            })}

            {/* ---------- the border ----------
                One line between what is yours and what is not, with a
                firewall on it. The two arrows that cross it are the only
                traffic that leaves. */}
            <line className="n-wall" x1="74" y1="644" x2="850" y2="644" />
            <g className="n-wall-icon">
              <rect x="40" y="630" width="34" height="28" rx="4" />
              <path d="M 40 639.33 H 74 M 40 648.67 H 74 M 57 630 V 639.33 M 48.5 639.33 V 648.67 M 65.5 639.33 V 648.67 M 57 648.67 V 658" />
            </g>
            <text className="t-sub" x="88" y="636">
              Your network
            </text>
            <text className="t-sub t-sub--out" x="88" y="660">
              Public internet
            </text>

            {/* what each crossing has to get past */}
            <text className={edgeLabel("proxy-internet")} x="309" y="572" textAnchor="end">
              checked by the proxy
            </text>
            <text className={edgeLabel("router-external")} x="728" y="572">
              checked by policy
            </text>

            {/* ---------- developer machine ---------- */}
            {/* The developer stands outside the machine: a person, not a
                component of it. */}
            <g className={nodeCls("dev")}>
              <circle className="n-avatar" cx="230" cy="100" r="22" />
              <circle className="n-person" cx="230" cy="93" r="7" />
              <path className="n-person" d="M 216 115 A 14 12 0 0 1 244 115 Z" />
              <text className="t-title" x="262" y="105">
                Developer
              </text>
            </g>

            <g className={nodeCls("cli")}>
              <rect className="n-box" x="64" y="190" width="332" height="52" rx="5" />
              <text className="t-title" x="86" y="222">
                John CLI
              </text>
            </g>

            <text className={edgeLabel("cli-up")} x="444" y="200" textAnchor="middle">
              prompt
            </text>
            <text className={edgeLabel("be-down")} x="444" y="246" textAnchor="middle">
              tool call
            </text>

            <g className={nodeCls("sandbox")}>
              <rect className="n-box" x="64" y="274" width="332" height="104" rx="5" />
              <text className="t-title" x="86" y="306">
                Sandbox · Linux virtual machine
              </text>
            </g>

            {/* Nested inside the sandbox rect on purpose: the overlay is part
                of the VM, not part of the developer's tree. */}
            <g className={nodeCls("overlay")}>
              <rect className="n-strip" x="82" y="320" width="296" height="36" rx="3" />
              <text className="t-strip" x="98" y="343">
                overlay · every write goes here
              </text>
            </g>

            <text className={edgeLabel("overlay-workdir")} x="112" y="401">
              apply
            </text>
            <text className={edgeLabel("workdir-sandbox")} x="186" y="401">
              reads
            </text>

            <g className={nodeCls("workdir")}>
              <rect className="n-box" x="64" y="416" width="158" height="52" rx="5" />
              <text className="t-title" x="86" y="448">
                Working dir
              </text>
            </g>

            <g className={nodeCls("proxy")}>
              <rect className="n-box" x="238" y="416" width="158" height="52" rx="5" />
              <text className="t-title" x="258" y="448">
                Network proxy
              </text>
            </g>

            {/* ---------- backend ---------- */}
            <g className={nodeCls("loop")}>
              <rect className="n-box" x="492" y="190" width="290" height="52" rx="5" />
              <text className="t-title" x="514" y="222">
                Agent loop
              </text>
              {/* A 300° arc with its own head, so the loop reads as a loop
                  even when it is standing still. Centre (752, 218), r 12: the
                  ends sit at -60° and 240°, and the head points along the
                  direction of travel into the gap. Keep these in step with the
                  transform-origin in the stylesheet — a couple of units out
                  and the spin visibly wobbles. */}
              <g className={traceOn ? "n-cycle is-spin" : "n-cycle"}>
                <path d="M 758 207.61 A 12 12 0 1 1 746 207.61" />
                <path
                  className="head"
                  d="M 749.9 205.36 L 746.3 211.13 L 743.1 205.59 z"
                />
              </g>
            </g>

            <g className={nodeCls("policy")}>
              <rect className="n-box" x="492" y="300" width="290" height="52" rx="5" />
              <text className="t-title" x="514" y="332">
                Policy engine
              </text>
            </g>

            <g className={nodeCls("router")}>
              <rect className="n-box" x="492" y="416" width="290" height="52" rx="5" />
              <text className="t-title" x="514" y="448">
                Model router
              </text>
            </g>

            <g className={nodeCls("trace")}>
              <rect className="n-spine" x="794" y="190" width="36" height="278" rx="4" />
              <text
                className="t-sub"
                x="812"
                y="329"
                textAnchor="middle"
                transform="rotate(-90 812 329)"
              >
                Session traces · encrypted
              </text>
              <path className="n-tick" d="M 782 216 L 794 216" />
            </g>

            {/* The internal model is a place, like the machine and the
                backend, so its panel stays solid while its label dims. */}
            <rect className="zone" x="470" y="522" width="200" height="58" rx="10" />
            <g className={nodeCls("internal")}>
              <rect className="n-box n-box--zone" x="470" y="522" width="200" height="58" rx="10" />
              <text className="t-title" x="492" y="550">
                Internal model
              </text>
              <text className="t-item" x="492" y="570">
                Inside your network
              </text>
            </g>

            {/* ---------- the open internet ---------- */}
            <g className={nodeCls("external")}>
              <rect className="n-box" x="620" y="680" width="200" height="56" rx="5" />
              <text className="t-title" x="640" y="706">
                External model
              </text>
              <text className="t-item" x="640" y="726">
                Claude, OpenAI, …
              </text>
            </g>

            <g className={nodeCls("internet")}>
              <rect className="n-box" x="217" y="680" width="200" height="56" rx="5" />
              <text className="t-title" x="237" y="706">
                The web
              </text>
              <text className="t-item" x="237" y="726">
                Registries and APIs
              </text>
            </g>

          </svg>
        </div>
      </div>


      {/* The same script as plain text, for a visitor who never sees the
          animation run. */}
      <ol className="flow-list">
        {STEPS.map((s) => (
          <li key={s.n}>
            <strong>{s.title}.</strong> {s.body}
          </li>
        ))}
      </ol>
    </div>
  );
}
