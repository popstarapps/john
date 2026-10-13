import logoLight from "../assets/logo-light.svg";
import logoDark from "../assets/logo-dark.svg";

// Declared so the browser reserves the right box before the images land. CSS
// sets the width; these keep the height in proportion.
const W = 920;
const H = 323;

/**
 * The brand artwork — hexagon and wordmark — as the SVGs in src/assets. The
 * console ships copies of the same files.
 *
 * Both themes' variants are in the markup and CSS hides one, which is what
 * makes this work with the pre-paint theme script in index.html: the right
 * logo is already painted on the first frame, in prerendered HTML, with no JS
 * and no flash when the visitor toggles the theme.
 */
export function Lockup({ label, className }) {
  return (
    <span
      className={className ? `logo ${className}` : "logo"}
      // One accessible name on the wrapper, not on each variant — otherwise
      // which name a screen reader gets would depend on the theme.
      role={label ? "img" : undefined}
      aria-label={label}
      aria-hidden={label ? undefined : true}
    >
      <img className="logo-light" src={logoLight} width={W} height={H} alt="" />
      <img className="logo-dark" src={logoDark} width={W} height={H} alt="" />
    </span>
  );
}
