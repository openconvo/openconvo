import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build output goes into internal/web/dist, where the Go binary
// embeds it (internal/web/web.go). emptyOutDir is false because the
// directory holds a committed .gitkeep; `make web` cleans stale files.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/web/dist",
    emptyOutDir: false,
    // Vite's default, stated so the browser floor is visible in the repo:
    // Chrome/Edge 107, Firefox 104, Safari 16 at the time of writing.
    // Nothing in src/ may rely on a feature newer than that.
    target: "baseline-widely-available",
  },
  // Dev only: `vite build` ignores this, so no localhost backend can reach
  // a production bundle. In production the Go binary serves the API and
  // these assets from the same origin, which is why base stays "/".
  server: {
    port: 5173,
    proxy: {
      "/api": "http://localhost:8080",
      "/health": "http://localhost:8080",
    },
  },
});
