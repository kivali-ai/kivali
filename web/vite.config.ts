import { mkdirSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import react from '@vitejs/plugin-react';
import type { Plugin } from 'vite';
import { defineConfig } from 'vitest/config';

const backend = 'http://127.0.0.1:8080';
const outDir = '../internal/web/ui/dist';

// changeOrigin stays false on purpose: the API's requireSameOrigin compares Origin with the
// request's Host, and both must stay the dev server's (localhost:5173) for writes to pass.
// SSE streams must not be timed out by the dev proxy (0 disables both timeouts); nothing is a websocket.
const stream = { target: backend, changeOrigin: false, ws: false, timeout: 0, proxyTimeout: 0 };
const plain = { target: backend, changeOrigin: false, ws: false };

// emptyOutDir removes everything in the build output, including the tracked .gitkeep that keeps
// `//go:embed all:dist` compiling on a clean checkout. Put it back after every build.
function keepDist(): Plugin {
  return {
    name: 'kivali-keep-dist',
    apply: 'build',
    closeBundle() {
      const dir = resolve(import.meta.dirname, outDir);
      mkdirSync(dir, { recursive: true });
      writeFileSync(resolve(dir, '.gitkeep'), '');
    },
  };
}

export default defineConfig({
  plugins: [react(), keepDist()],
  // Served at the site root by internal/web/ui; the Go server answers its own paths ahead of the app.
  base: '/',
  build: {
    outDir,
    emptyOutDir: true,
    sourcemap: false,
  },
  // Everything the Go server answers itself goes to it (keep in step with wireRoutes in internal/web/web.go and
  // SERVER_PREFIXES in src/app/links.ts); every other path is the app, served by Vite with index.html as fallback.
  server: {
    proxy: {
      '/api': plain,
      '/org/stream': stream,
      '^/agents/[^/]+/stream$': stream,
      '^/agents/[^/]+/subagents/[^/]+/stream$': stream,
      '^/agents/[^/]+/(messages|stop)$': plain,
      '/messages/': plain,
      '/attachments/': plain,
      '/branding/': plain,
      '/favicon.ico': plain,
      '/auth/': plain,
      '/healthz': plain,
      '/readyz': plain,
      '/admin/': plain,
      '/v1/': plain,
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    exclude: ['**/node_modules/**', '**/dist/**', 'e2e/**'],
  },
});
