import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { viteSingleFile } from "vite-plugin-singlefile";

// `npm run build`        → dist/          normal static site (separate js/css)
// `npm run build:single` → dist-single/   one self-contained index.html
export default defineConfig(({ mode }) => ({
  plugins: [react(), ...(mode === "single" ? [viteSingleFile()] : [])],
  // Relative asset paths so the build works from any directory, including
  // opening index.html straight off the filesystem.
  base: "./",
  build:
    mode === "single"
      ? {
          outDir: "dist-single",
          // Nothing may be emitted alongside the HTML in this mode, so the
          // logo SVGs have to inline as data URIs rather than as files.
          assetsInlineLimit: 1024 * 1024,
        }
      : {},
}));
