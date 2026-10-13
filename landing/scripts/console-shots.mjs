// console-shots photographs the John web console, full of a believable
// company, in both themes, for the landing page.
//
//	cd landing && npm run screenshots
//	npm run screenshots -- traces models     # just these shots
//
// Nothing on these screens is mocked in the browser. The script starts a real
// backend (scripts/console-backend.go, which is the backend package itself
// behind a small seeding program), signs in through the backend's own OIDC
// flow, and points a real Chromium at the real console. What the console
// shows is what the backend answered.
//
// Step by step:
//
//  1. A temp directory is made; everything below lives in it and it is
//     removed at the end, whether the run succeeded or not. Nothing is written
//     into the repository except the images.
//  2. The console is built from console/src with vite, into that directory,
//     so the shots are of the console as it is in the working tree rather
//     than whatever console/dist last happened to hold. (SHOTS_EMBEDDED=1
//     skips this and photographs the console embedded in the Go build.)
//  3. scripts/console-backend.go is compiled and started. It opens a fresh
//     SQLite database and trace store, mounts the embedded dev identity
//     provider (auth/devidp — what standalone mode and the tests sign in
//     with), and seeds "Lumenfield" through the backend's HTTP API and the
//     store package: people, teams, policy groups, models, sixty days of
//     usage, network access requests, encrypted session traces, and the change
//     history all of that produced. The company is written down at the top of
//     that file; edit it there. When it is ready it prints the console's URL
//     and the organization's trace read key.
//  4. For each theme, a browser context signs in by visiting /auth/login:
//     the dev IdP consents automatically, the backend verifies the ID token
//     and sets its ordinary session cookie. The theme is chosen the way the
//     console chooses it — the `john-theme` key in localStorage, read by the
//     blocking script in index.html before first paint — with
//     prefers-color-scheme emulated to match.
//  5. Each shot navigates, waits until every skeleton is gone, the network is
//     quiet and fonts are loaded, does whatever a person would do to get the
//     view (the traces page is unlocked by pasting the read key into its
//     form, then a session is opened), and takes a 1440×900 viewport
//     screenshot at device scale 2.
//  6. Screenshots are encoded as WebP by sharp and written to
//     src/assets/console/<shot>-<theme>.webp, which Vite imports like any
//     other asset. WebP rather than PNG because a 2880×1800 console
//     screenshot is 1 MB or more as PNG and 200–300 kB as WebP at quality 92,
//     with no visible difference in text or charts.
//  7. The browser and the backend are stopped and the temp directory removed,
//     including on failure and Ctrl-C.
//
// Requirements: Go (on PATH, or GO=/path/to/go), node_modules in landing/ and
// console/, and a Chromium-family browser. The browser is found in this
// order: $CHROME_PATH; Google Chrome, Chromium or Edge in their usual macOS
// and Linux locations; Playwright's own download (`npx playwright-core
// install chromium`).
//
// Fonts are the system's: the console's type stacks are system-ui and
// ui-monospace, so shots taken on a Mac use SF Pro and SF Mono, which is what
// the landing page should show. Shots taken on Linux come out in whatever the
// distribution has and are for checking, not shipping.

import { spawn, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import { chromium } from "playwright-core";
import sharp from "sharp";

const here = path.dirname(fileURLToPath(import.meta.url));
const landing = path.resolve(here, "..");
const root = path.resolve(landing, "..");
const outDir = path.join(landing, "src", "assets", "console");

const VIEWPORT = { width: 1440, height: 900 };
const SCALE = 2;
const THEMES = ["light", "dark"];

/* ---------------------------------------------------------------- the shots
 *
 * Each shot is a name (the file name) and what to do to get there. `go` is a
 * console path; `then` runs after the page has settled. Order is the order
 * they are taken in, which does not matter to anything. */

const SHOTS = [
  {
    name: "usage",
    // The administrator's home: org spend by model, by team, heaviest users.
    go: "/admin",
  },
  {
    name: "requests",
    // The review queue of network access requests.
    go: "/admin/requests",
  },
  {
    name: "request",
    // One request as the reviewer sees it: the blocked traffic that prompted
    // it, and the policy it would become, editable before approval.
    go: async (page) => ref(page, "crates.io for the ledger rewrite"),
  },
  {
    name: "baseline",
    // The baseline group: workspace protections, redaction, denied hosts and
    // the codename routing rule, in one place.
    go: async (page) => groupPath(page, "Lumenfield baseline"),
  },
  {
    name: "teams",
    // Teams and the policy groups each one carries. (A single team's page,
    // /admin/teams/payments, has its effective-policy table cut off at this
    // width, which is why it is not the shot.)
    go: "/admin/teams",
  },
  {
    name: "models",
    // External and internally hosted models, and the routing rules using them.
    go: "/admin/models",
  },
  {
    name: "traces",
    // Session traces, unlocked in the browser, one session open.
    go: "/admin/traces",
    then: async (page, { traceKey }) => {
      await page.getByPlaceholder("AGE-SECRET-KEY-1…").fill(traceKey);
      await page.getByRole("button", { name: "Unlock" }).click();
      await page.getByText("ledger-service").first().waitFor();
      await dismissToasts(page);
      await page.locator("tr.clickable").first().click();
      await page.locator(".drawer").getByText("session.open").first().waitFor();
    },
  },
  {
    name: "history",
    // The append-only change history.
    go: "/admin/history",
  },
];

/* ---------------------------------------------------------------- the run */

const only = process.argv.slice(2);
const shots = only.length ? SHOTS.filter((s) => only.includes(s.name)) : SHOTS;
if (only.length && shots.length !== only.length) {
  fail(`unknown shot; choose from: ${SHOTS.map((s) => s.name).join(", ")}`);
}

const tmp = mkdtempSync(path.join(tmpdir(), "john-console-shots-"));
let backend = null;
let browser = null;
let cleaned = false;

async function cleanup() {
  if (cleaned) return;
  cleaned = true;
  await browser?.close().catch(() => {});
  if (backend && backend.exitCode === null) {
    backend.stdin.end(); // its signal to shut down cleanly
    const exited = new Promise((r) => backend.once("exit", r));
    const timer = setTimeout(() => backend.kill("SIGKILL"), 5000);
    await exited;
    clearTimeout(timer);
  }
  rmSync(tmp, { recursive: true, force: true });
}

for (const sig of ["SIGINT", "SIGTERM"]) {
  process.on(sig, async () => {
    await cleanup();
    process.exit(130);
  });
}

try {
  await main();
} catch (err) {
  console.error(`\nconsole-shots: ${err?.stack || err}`);
  process.exitCode = 1;
} finally {
  await cleanup();
}

async function main() {
  const dist = process.env.SHOTS_EMBEDDED ? null : buildConsole();
  const ready = await startBackend(dist);
  console.log(`backend ready at ${ready.url}`);

  browser = await launchBrowser();
  mkdirSync(outDir, { recursive: true });

  for (const theme of THEMES) {
    const context = await browser.newContext({
      viewport: VIEWPORT,
      deviceScaleFactor: SCALE,
      colorScheme: theme,
      locale: "en-US",
      timezoneId: "UTC",
      reducedMotion: "reduce",
    });
    await context.addInitScript((t) => {
      try {
        localStorage.setItem("john-theme", t);
      } catch {}
    }, theme);
    const page = await context.newPage();
    page.on("pageerror", (err) => console.warn(`  page error: ${err.message}`));

    // Sign in: the dev IdP consents on its own and the backend sets a cookie.
    await page.goto(`${ready.url}/auth/login?next=/admin`);
    await page.waitForURL(`${ready.url}/admin`);
    await settle(page);

    for (const shot of shots) {
      const target = typeof shot.go === "function" ? await shot.go(page) : shot.go;
      await page.goto(ready.url + target);
      await settle(page);
      if (shot.then) {
        await shot.then(page, ready);
        await settle(page);
      }
      await dismissToasts(page);
      const png = await page.screenshot({ type: "png", animations: "disabled", caret: "hide" });
      const file = path.join(outDir, `${shot.name}-${theme}.webp`);
      const info = await sharp(png).webp({ quality: 92, effort: 6, smartSubsample: true }).toFile(file);
      console.log(`  ${path.relative(landing, file)}  ${Math.round(info.size / 1024)} kB`);
    }
    await context.close();
  }
}

/* ---------------------------------------------------------------- helpers */

function buildConsole() {
  const consoleDir = path.join(root, "console");
  if (!existsSync(path.join(consoleDir, "node_modules"))) {
    fail("console/node_modules is missing: run `npm install` in console/ (or set SHOTS_EMBEDDED=1)");
  }
  const dist = path.join(tmp, "console-dist");
  console.log("building the console…");
  const res = spawnSync(
    process.platform === "win32" ? "npx.cmd" : "npx",
    ["vite", "build", "--outDir", dist, "--emptyOutDir", "--logLevel", "warn"],
    { cwd: consoleDir, stdio: "inherit" },
  );
  if (res.status !== 0) fail("the console build failed");
  return dist;
}

async function startBackend(dist) {
  const bin = path.join(tmp, process.platform === "win32" ? "console-backend.exe" : "console-backend");
  const go = process.env.GO || "go";
  console.log("building the backend…");
  const res = spawnSync(go, ["build", "-o", bin, "./landing/scripts/console-backend.go"], {
    cwd: root,
    stdio: "inherit",
  });
  if (res.error) fail(`could not run ${go}: ${res.error.message} (set GO=/path/to/go)`);
  if (res.status !== 0) fail("the backend build failed");

  const args = ["-data", path.join(tmp, "data")];
  if (dist) args.push("-dist", dist);
  backend = spawn(bin, args, { cwd: tmp, stdio: ["pipe", "pipe", "inherit"] });

  return await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("the backend did not become ready in 60s")), 60_000);
    backend.once("exit", (code) => reject(new Error(`the backend exited with ${code} before it was ready`)));
    createInterface({ input: backend.stdout }).once("line", (line) => {
      clearTimeout(timer);
      try {
        resolve(JSON.parse(line));
      } catch {
        reject(new Error(`unexpected output from the backend: ${line}`));
      }
    });
  });
}

async function launchBrowser() {
  const candidates = [
    process.env.CHROME_PATH,
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/Applications/Chromium.app/Contents/MacOS/Chromium",
    "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
    "/usr/bin/google-chrome",
    "/usr/bin/google-chrome-stable",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/snap/bin/chromium",
  ].filter(Boolean);
  const executablePath = candidates.find((p) => existsSync(p));
  try {
    // With no executablePath, playwright-core uses its own download, if any.
    return await chromium.launch({ executablePath, headless: true });
  } catch (err) {
    fail(
      `could not start a browser${executablePath ? ` at ${executablePath}` : ""}: ${err.message}\n` +
        "Set CHROME_PATH to a Chrome or Chromium binary, or run `npx playwright-core install chromium`.",
    );
  }
}

/** Wait until the page is done: no skeletons, a quiet network, fonts in. */
async function settle(page) {
  await page.waitForLoadState("networkidle");
  await page.waitForFunction(() => !document.querySelector(".skeleton, [aria-busy='true']"), null, {
    timeout: 15_000,
  });
  await page.evaluate(() => document.fonts.ready);
  await page.waitForLoadState("networkidle");
  await page.waitForTimeout(250); // one more frame for layout after data
}

/** Toasts are transient confirmations of what the script just did. Dismissed
 *  with their own close button, so React stays in charge of the DOM. */
async function dismissToasts(page) {
  const close = page.locator(".toast button[aria-label='Dismiss']");
  while ((await close.count()) > 0) await close.first().click();
}

/** The console path for a request, found by its title through the API. */
async function ref(page, title) {
  const all = await page.evaluate(async () => (await fetch("/api/v1/access-requests")).json());
  const hit = all.find((r) => r.title === title);
  if (!hit) fail(`no request titled ${title}`);
  return `/admin/requests/${hit.id}`;
}

/** The console path for a policy group, by name. */
async function groupPath(page, name) {
  const all = await page.evaluate(async () => (await fetch("/api/v1/groups")).json());
  const hit = all.find((g) => g.name === name);
  if (!hit) fail(`no policy group named ${name}`);
  return `/admin/groups/${hit.id}`;
}

function fail(msg) {
  throw new Error(msg);
}
