import { Lockup } from "../components/Logo.jsx";

export default function Footer() {
  return (
    <footer>
      <div className="wrap">
        <a className="brand" href="#top" aria-label="John, home">
          <Lockup />
        </a>
        <span className="fine">
          The safe, dependable coding agent for enterprises.
          <span className="sep" aria-hidden="true"> · </span>
          <a href="https://github.com/popstarapps/john" rel="noreferrer" target="_blank">
            GitHub
          </a>
        </span>
      </div>
    </footer>
  );
}
