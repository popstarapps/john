# Console

The John web console — the surface behind `john console` and the backend's own
web interface. Developers use it to see what John cost them, the policies that
apply to them and the teams those policies come from, and the network access
they have asked for. Administrators use it to decide those requests, manage
teams, groups, members, models and settings, and read the encrypted traces.

The developer half is deliberately small: usage, requests, policies, download.
There is no activity view — traces are an administrator's surface, not something
a developer browses about themselves; sandboxes, their resource limits and
per-request approvals are the CLI's job, where the developer actually is when
the decision matters. And the only thing a member can ask for is network
access: replacements, routing and workspace protection are an administrator's
to write directly.

React 19 + Vite + TypeScript, hand-written CSS. No UI framework, no component
library, no CSS framework — the same design language as `/landing`, with tokens
copied verbatim so the two are visibly one product.

```
npm install
npm run dev        # http://localhost:5174
npm run build      # → dist/, a static bundle
npm run preview    # serve dist/ locally
npm run typecheck
```

## Backend connection

Every screen uses `src/api/client.ts` to call the Go backend's `/api/v1`
endpoints. The backend stores data and enforces authentication and permissions.

```
src/api/types.ts      request and response types
src/api/client.ts     API calls and change subscriptions
```

Successful mutations increment a version counter that `useQuery` subscribes to,
refreshing views that display the changed data. The backend records
configuration changes in the change history. Session traces are fetched as
encrypted files and decrypted in the browser; the private key stays in memory
in the browser tab.

For local development, build the embedded console with `make console` from the
repository root, then run `go run . backend --setup -db console-dev.db`.
Complete setup using the printed URL (see [Installation](../readme.md#installation)),
stop the setup process, and run `go run . backend -db console-dev.db`.

In another terminal, run the commands above from `console/` and open
`http://localhost:5174`. Vite proxies `/api`, `/auth`, and `/devidp` to the
backend at `http://127.0.0.1:8080`. The development server uses real backend data;
the example commands keep it in a separate development database.

## Layout

```
src/components/     the kit: shell, tables, badges, charts, overlays
src/components/domain.tsx   John's vocabulary rendered consistently
src/routes/         one file per screen; src/routes/admin/ for admin screens
src/lib/            formatting, the query hook, the id→name directory, theme
src/styles/         tokens → base → shell → components → views
```

## Routes

Developer: `/` usage (the home page) · `/requests`, `/requests/new`,
`/requests/:id` — network access requests · `/policies` (which includes your
teams) · `/download`

Administration: `/admin` organization usage · `/admin/requests` and
`/admin/requests/:id` · `/admin/groups` and `/admin/groups/:id` ·
`/admin/teams` and `/admin/teams/:slug` · `/admin/members` and
`/admin/members/:id` · `/admin/models` · `/admin/traces` · `/admin/history` ·
`/admin/settings/:tab`

Outside the shell: `/signin` and `/setup`, the first-run wizard the backend's
one-time `--setup` URL opens.

A network access request has two faces, and which one you get follows the
**route**, not the role: `/requests/:id` is the requester's view (what I asked
for, what came back), `/admin/requests/:id` is the reviewer's — the same policy as an editable
form, because approving a request unchanged is the exception rather
than the rule. An administrator opening their own request from `/requests` is
the person who asked, and sees it that way.

There are two roles, member and administrator, held organization-wide rather
than delegated per team. Admin routes are hidden from navigation when the viewer
is not an administrator. The backend also checks permissions on API requests;
hiding navigation does not grant or restrict access.

## Development notes

- **`base` is `/`, not `./`.** The console has real nested URLs, and relative
  asset paths break on a hard refresh at `/admin/teams/x`. Whatever serves
  `dist/` must fall back to `index.html` for unknown paths.
- **Themes.** Light is the ground; three token blocks (`:root`, the
  `prefers-color-scheme` media query, `[data-theme]`) must stay in step, and a
  blocking script in `index.html` applies the saved theme before first paint.
  Same `john-theme` storage key as the landing site.
- **One categorical chart, and it is validated.** Spend-per-day-by-model is the
  only place several hues appear at once; the slots (`--series-1…5`) are the
  validated order, assigned by series index and never cycled, and the chart
  ships a legend with values plus the per-model table beside it — three of the
  light steps sit under 3:1 on the surface and that is the required relief.
  Every other chart is single-hue, and status is always a word before it is a
  colour: green/amber/red as adjacent fills fails colourblind separation, so
  allowed / ask-developer / blocked never becomes one stacked bar.
- **The model dialog is shared.** `components/ModelDialog.tsx` is used by both
  first-run setup and the admin models screen; its fields are driven by a
  provider preset rather than a union of every credential field in existence.
- **So is the policy editor.** `components/PolicyEditor.tsx` renders a policy's
  matcher for all three callers — the developer asking for network access, the
  administrator adding a policy to a group, and the administrator amending a
  request before approving it — so a rule looks the same wherever it is written.
  A network policy's decision is one of **always allow**, **ask developer** or
  **deny**; a request nothing matches at all is unknown, and the proxy blocks it
  the moment it is made. Unknown is the only outcome a member can act on, which
  is why it is the only one that leads here.
- **And the effective-policy tables.** `components/policies.tsx` is what both
  `/policies` and a team's page render. Every section is four columns and the
  last two hold one width, so the tables line up down the page. A reference to
  something defined elsewhere — a group, a policy, a model, a keyword — is
  always in a tag; prose is prose.
- **And the usage view.** `components/usage.tsx` is `/` and `/admin` at two
  scales: same tiles, same chart, same table, so a number means the same thing
  on both. Dollars in a column pass `money(x, 2)` so the decimal points line up.
- **Traces and change history.** `/admin/traces` shows encrypted session
  records, unlocked in the browser with the organization's private read key.
  `/admin/history` shows configuration changes, such as policy edits and request
  decisions. The backend records these changes without session content, so
  administrators can read the history without a trace key. Entries cannot be
  edited or deleted through the API.
