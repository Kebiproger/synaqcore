import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// SPA отдаётся шлюзом под /dashboard/ (см. nginx.conf контейнера).
export default defineConfig({
  base: "/dashboard/",
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "dist",
    sourcemap: false,
    // Без inline-скриптов/стилей, чтобы CSP оставался строгим.
    assetsInlineLimit: 0,
  },
  server: {
    port: 5173,
    // Для локальной разработки: API и oidc.json через шлюз.
    proxy: {
      "/api": "http://localhost:8080",
      "/dashboard/oidc.json": "http://localhost:8080",
    },
  },
});
