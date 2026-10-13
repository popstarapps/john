/**
 * A real capture of the admin console, in the light or dark version to match
 * the page. Regenerate the images with
 * `npm run screenshots` (see scripts/console-shots.mjs).
 */
export default function ConsoleShot({ light, dark, alt, loading = "lazy" }) {
  return (
    <div className="console-frame">
      <img
        className="shot-light"
        src={light}
        width={1440}
        height={900}
        loading={loading}
        alt={alt}
      />
      <img
        className="shot-dark"
        src={dark}
        width={1440}
        height={900}
        loading={loading}
        alt=""
      />
    </div>
  );
}
