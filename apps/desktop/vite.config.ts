import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const projectRoot = path.dirname(fileURLToPath(import.meta.url));
const pkg = JSON.parse(fs.readFileSync(path.resolve(projectRoot, "package.json"), "utf8")) as { version: string };

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": path.resolve(projectRoot, "./src") },
  },
  // The desktop client version is read from package.json at build time — the
  // same source the bundler stamps into the app. Never hardcode it in UI.
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
  },
  // Tauri dev server expectations: fixed port, strict host.
  clearScreen: false,
  server: {
    port: 1420,
    strictPort: true,
  },
  envPrefix: ["VITE_", "TAURI_ENV_"],
  build: {
    target: "es2021",
    minify: "esbuild",
    sourcemap: false,
  },
});
