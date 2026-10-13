import Section from "../components/Section.jsx";

const REPO = "https://github.com/popstarapps/john";
const DOCS = `${REPO}/blob/main/readme.md`;
const RELEASES = `${REPO}/releases`;

/**
 * The page's ending: the two real ways in. A developer can try John alone in
 * a couple of minutes; a team needs one server first. Each card labels where
 * its commands run, because the team path spans two kinds of machine.
 */
export default function GetStarted() {
  return (
    <Section id="start">
      <div className="start-head">
        <h2>Get started.</h2>
        <p className="lede">
          Free and open source under MIT. One binary, no cluster.
        </p>
      </div>

      <div className="start-cards">
        <div className="start-card">
          <h3>Try it on your laptop</h3>
          <p className="small muted">
            Standalone mode runs the whole thing on your machine, with your own
            Anthropic, OpenAI or Gemini API key. No server needed.
          </p>
          <span className="start-where">On your machine</span>
          <div className="cmd-stack">
            <span className="cmd">
              <span className="p">$ </span>john login standalone
            </span>
            <span className="cmd">
              <span className="p">$ </span>cd my-project &amp;&amp; john
            </span>
          </div>
          <a className="btn btn--lg" href={RELEASES} rel="noreferrer" target="_blank">
            Download the client
          </a>
        </div>

        <div className="start-card">
          <h3>Roll it out to your team</h3>
          <p className="small muted">
            One backend on a Linux host, signed in with your SSO. Policies,
            models and teams are set up in its web console.
          </p>
          <span className="start-where">On your server</span>
          <div className="cmd-stack">
            <span className="cmd">
              <span className="p">$ </span>john backend --setup
            </span>
          </div>
          <span className="start-where">On each developer&apos;s machine</span>
          <div className="cmd-stack">
            <span className="cmd">
              <span className="p">$ </span>john login https://john.corp.internal
            </span>
          </div>
          <a
            className="btn btn--lg"
            href={`${DOCS}#installation`}
            rel="noreferrer"
            target="_blank"
          >
            Read the setup guide
          </a>
        </div>
      </div>
    </Section>
  );
}
