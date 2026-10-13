/**
 * Turns the single-file build into a fragment suitable for publishing as a
 * Claude Artifact, which supplies its own <!doctype>/<html>/<head>/<body>.
 *
 *   npm run build:artifact   →  dist-single/artifact.html
 */
import { readFile, writeFile } from "node:fs/promises";

const SRC = new URL("../dist-single/index.html", import.meta.url);
const OUT = new URL("../dist-single/artifact.html", import.meta.url);

const html = await readFile(SRC, "utf8");

const head = html.match(/<head[^>]*>([\s\S]*?)<\/head>/i)?.[1];
const body = html.match(/<body[^>]*>([\s\S]*?)<\/body>/i)?.[1];

if (!head || !body) {
  console.error("make-artifact: could not find <head> and <body> in the build.");
  process.exit(1);
}

await writeFile(OUT, `${head.trim()}\n${body.trim()}\n`, "utf8");
console.log(`make-artifact: wrote ${OUT.pathname}`);
