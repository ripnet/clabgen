import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` proxies API calls to a running clabgen-ui (default :8090) or
// agent; set CLABGEN_UI to point elsewhere.
const target = process.env.CLABGEN_UI ?? "http://127.0.0.1:8090";

export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "../internal/webui/dist",
    // Keep the committed .gitkeep; `make web` removes stale assets first.
    emptyOutDir: false,
    chunkSizeWarningLimit: 1000,
  },
  server: {
    proxy: {
      "/api": target,
      "/agents": target,
    },
  },
});
