/**
 * Bakes the rendered page into dist/index.html.
 *
 *   npm run build:static   →  vite build, then this
 *
 * Why: the page is a security argument that gets opened in locked-down
 * browsers, forwarded to people who will skim it, and read by crawlers. None
 * of those should see an empty <div id="root">. The client bundle still ships
 * and hydrates on top (see src/main.jsx), so behaviour is unchanged — the
 * reveal-on-scroll animation simply never runs for a visitor without JS, which
 * leaves the content plainly visible rather than plainly missing.
 *
 * Run AFTER `vite build`. It reads and rewrites that build's index.html.
 */
import { readFile, writeFile, rm } from "node:fs/promises";
import { build } from "vite";

const TMP = "node_modules/.ssr-prerender";
const ENTRY = new URL(`../${TMP}/entry-server.js`, import.meta.url);
const INDEX = new URL("../dist/index.html", import.meta.url);
const MARKER = '<div id="root"></div>';

// A throwaway SSR bundle. It never ships; only its render() output does.
await build({
  logLevel: "warn",
  build: {
    ssr: "src/entry-server.jsx",
    outDir: TMP,
    emptyOutDir: true,
  },
});

const { render } = await import(ENTRY.href);

/**
 * Vite resolves asset URLs in an SSR build against "/" no matter what `base`
 * says, so render() emits /assets/logo-light.svg while the client build ships
 * ./assets/logo-light.svg. Left alone, every logo in the prerendered markup
 * 404s the moment the site is served from a subdirectory or opened off the
 * filesystem — and React's automatic <link rel="preload"> for those images
 * 404s with it. Rewrite them to match the base the rest of the build uses.
 */
const rebase = (markup) => markup.replace(/(\s(?:src|href)=")\/assets\//g, "$1./assets/");

let html;
try {
  html = await readFile(INDEX, "utf8");
} catch {
  console.error(
    "prerender: dist/index.html not found. Run `vite build` first — " +
      "`npm run build:static` does both.",
  );
  process.exit(1);
}

if (!html.includes(MARKER)) {
  console.error(`prerender: could not find ${MARKER} in dist/index.html.`);
  process.exit(1);
}

await writeFile(
  INDEX,
  html.replace(MARKER, `<div id="root">${rebase(render())}</div>`),
  "utf8",
);
await rm(new URL(`../${TMP}`, import.meta.url), {
  recursive: true,
  force: true,
});

console.log("prerender: dist/index.html now ships the rendered page.");
