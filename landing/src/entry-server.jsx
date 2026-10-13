import { renderToString } from "react-dom/server";
import App from "./App.jsx";

/**
 * Build-time entry. Nothing in App reads the clock, randomness or the DOM
 * during render, so the markup this produces is stable and hydrates cleanly.
 *
 * renderToString, not renderToStaticMarkup: the client hydrates this markup,
 * and only renderToString emits the separators React needs to line text nodes
 * back up.
 */
export function render() {
  return renderToString(<App />);
}
