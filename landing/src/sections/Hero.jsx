import { Lockup } from "../components/Logo.jsx";
import SessionPlayer from "../components/SessionPlayer.jsx";

export default function Hero() {
  return (
    <section className="hero">
      <div className="wrap">
        <div className="hero-head">
          {/* The drawn wordmark is part of the artwork here, so the hero does
              not also set "John." in type the way the bar and footer do. */}
          <Lockup label="John" className="lockup" />

          <h1>The safe, dependable coding agent for enterprises.</h1>

          <p className="lede">
            Give developers an agent that can get to work. Keep model access,
            network permissions, and changes to your files under your control.
          </p>
          <p className="lede">
            John is the coding agent your security organization has been waiting
            for.
          </p>

          <div className="hero-actions">
            <a className="btn" href="#start">
              Get started
            </a>
            <a className="btn btn--ghost" href="#architecture">
              See the architecture
            </a>
          </div>
        </div>

        {/* A recording of the CLI's own terminal UI driving a scripted
            session, not a mock-up of one. See components/SessionPlayer.jsx
            and scripts/record-demo.go. */}
        <SessionPlayer />
      </div>
    </section>
  );
}
