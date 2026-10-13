import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev`   → dev server on :5174 (the landing site uses :5173)
// `npm run build` → dist/, a static bundle the Go backend can go:embed and
//                   serve from the root of the console host.
//
// base is "/" rather than the landing site's "./": the console has real nested
// URLs (/admin/teams/eng-platform), and relative asset paths would resolve
// against the current route on a hard refresh. Whatever serves dist/ must fall
// back to index.html for unknown paths — the dev server already does.
// In dev, API and auth traffic proxies to a locally running backend
// (`go run . backend`, which listens on 127.0.0.1:8080 by default).
const backend = "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  base: "/",
  server: {
    port: 5174,
    proxy: {
      "/api": backend,
      "/auth": backend,
      "/devidp": backend,
    },
  },
  build: {
    outDir: "dist",
    sourcemap: true,
  },
});
