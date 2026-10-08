import { defineConfig } from "vitest/config";

// The bundled pages. Tauri loads dist/ in a build and this dev server
// (port fixed: tauri.conf.json's devUrl) under `tauri dev`. The design
// system's tokens, fonts and logos are imported from ../design-system,
// so the dev server may read it. Nothing is inlined as a data: URL: the
// CSP's font-src is 'self'.
export default defineConfig({
  clearScreen: false,
  server: { port: 1420, strictPort: true, host: "localhost", fs: { allow: [".", "../design-system"] } },
  build: { outDir: "dist", target: "safari16", emptyOutDir: true, assetsInlineLimit: 0 },
  test: { environment: "node", include: ["src/**/*.test.ts"] },
});
