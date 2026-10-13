import Section from "../components/Section.jsx";

const ARTICLE = "https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/";

/** A firewall glyph centred on (x, y), the same one the architecture uses. */
function Wall({ x, y }) {
  const l = x - 17;
  const t = y - 14;
  return (
    <g className="n-wall-icon">
      <rect x={l} y={t} width="34" height="28" rx="4" />
      <path
        d={`M ${l} ${t + 9.33} H ${l + 34} M ${l} ${t + 18.67} H ${l + 34} M ${x} ${t} V ${t + 9.33} M ${l + 8.5} ${t + 9.33} V ${t + 18.67} M ${l + 25.5} ${t + 9.33} V ${t + 18.67} M ${x} ${t + 18.67} V ${t + 28}`}
      />
    </g>
  );
}

/**
 * The trifecta as a triangle. The side between the two legs a coding agent
 * cannot avoid is drawn whole; both sides that reach "a way out" are cut,
 * with the firewall standing in the gap.
 */
function Triangle() {
  return (
    <svg
      className="trifecta"
      viewBox="-30 0 500 400"
      role="img"
      aria-label="A triangle of the three legs of the lethal trifecta. Private data and untrusted content are joined: both are your repository. The two sides that lead to a way out are cut by a firewall, because John closes that leg."
    >
      {/* the side that stays: repo to repo */}
      <line className="tri-side" x1="198.9" y1="136.7" x2="91.1" y2="273.3" />

      {/* the two sides John cuts */}
      <line className="tri-side tri-side--cut" x1="241.1" y1="136.7" x2="280.1" y2="186.2" />
      <line className="tri-side tri-side--cut" x1="309.9" y1="223.8" x2="348.9" y2="273.3" />
      <line className="tri-side tri-side--cut" x1="104" y1="300" x2="196" y2="300" />
      <line className="tri-side tri-side--cut" x1="244" y1="300" x2="336" y2="300" />
      <Wall x={295} y={205} />
      <Wall x={220} y={300} />

      {/* private data */}
      <circle className="tri-node" cx="220" cy="110" r="34" />
      <path
        className="tri-glyph"
        d="M 209 96 H 226 L 233 103 V 124 H 209 Z M 226 96 V 103 H 233 M 214 110 H 228 M 214 116 H 228"
      />
      <text className="tri-title" x="220" y="52" textAnchor="middle">
        Private data
      </text>
      <text className="tri-sub" x="220" y="70" textAnchor="middle">
        your repository
      </text>

      {/* untrusted content */}
      <circle className="tri-node" cx="70" cy="300" r="34" />
      <path className="tri-glyph" d="M 70 285 V 304" />
      <circle className="tri-dot" cx="70" cy="314" r="2.2" />
      <text className="tri-title" x="70" y="358" textAnchor="middle">
        Untrusted content
      </text>
      <text className="tri-sub" x="70" y="376" textAnchor="middle">
        also your repository
      </text>

      {/* a way out, closed */}
      <circle className="tri-node tri-node--closed" cx="370" cy="300" r="34" />
      <path
        className="tri-glyph tri-glyph--closed"
        d="M 361 291 L 379 309 M 379 291 L 361 309"
      />
      <text className="tri-title" x="370" y="358" textAnchor="middle">
        A way out
      </text>
      <text className="tri-sub tri-sub--closed" x="370" y="376" textAnchor="middle">
        closed by John
      </text>
    </svg>
  );
}

export default function Trifecta() {
  return (
    <Section id="trifecta">
      <div className="two-col trifecta-cols">
        <div>
          <h2>Break the lethal trifecta.</h2>
          <p className="lede">
            An AI agent with private data, untrusted content and a way to send
            data out can be tricked into leaking it. In a coding agent the first
            two come with the job: your repository is both. John lets you close
            the third.
          </p>
          <p className="small muted">
            Turn network access off, or limit it to an allow-list, and route
            sensitive teams to internally hosted models. Model calls only ever
            leave through your backend, so there is no other way out.
          </p>
          <p className="small muted">
            Injected instructions can still try to change your code. That
            arrives as a diff for you to review, not as a file already changed.
          </p>
          <p className="small">
            <a className="ext" href={ARTICLE} rel="noreferrer" target="_blank">
              Read Simon Willison on the lethal trifecta
            </a>
          </p>
        </div>

        <Triangle />
      </div>
    </Section>
  );
}
