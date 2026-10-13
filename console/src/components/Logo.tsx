import logoLight from "../assets/logo-light.svg";
import logoDark from "../assets/logo-dark.svg";

const W = 920;
const H = 323;

/**
 * The brand lockup, as the same two SVGs the landing site ships. Both variants
 * are in the markup and CSS hides one, so the correct mark is painted on the
 * first frame — no flash when the pre-paint theme script has chosen dark.
 */
export function Lockup({ label, className }: { label?: string; className?: string }) {
  return (
    <span
      className={className ? `logo ${className}` : "logo"}
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    >
      <img className="logo-light" src={logoLight} width={W} height={H} alt="" />
      <img className="logo-dark" src={logoDark} width={W} height={H} alt="" />
    </span>
  );
}
