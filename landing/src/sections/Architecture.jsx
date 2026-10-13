import Section from "../components/Section.jsx";
import SystemFlow from "../components/SystemFlow.jsx";

export default function Architecture() {
  return (
    <Section id="architecture">
      <h2>The agent runs in your network, under your rules.</h2>
      <p className="lede" style={{ marginBottom: 4 }}>
        Developers get a client that carries out commands in a sandbox on their
        machine. The agent itself runs on a backend you host, and every AI
        request passes through it, checked against your policies and routed to
        an internal or external model.
      </p>

      <SystemFlow />
    </Section>
  );
}
