import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// The dev server proxies API calls to the control plane so the browser only
// ever talks to its own origin (no CORS, works behind preview proxies).
const api = process.env.ASTRION_API_URL ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    host: "0.0.0.0",
    port: 5173,
    allowedHosts: true,
    proxy: {
      "/api": { target: api, changeOrigin: true },
      "/healthz": { target: api, changeOrigin: true },
    },
  },
  test: { environment: "jsdom" },
});
