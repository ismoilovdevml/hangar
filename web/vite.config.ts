/// <reference types="vitest/config" />
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import type { Connect, Plugin } from 'vite';
import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';

const require = createRequire(import.meta.url);

/**
 * Serves (and, on build, emits) MSW's service worker only when mock mode is on,
 * so production bundles never contain it.
 */
function mockServiceWorker(enabled: boolean): Plugin {
  const file = 'mockServiceWorker.js';
  const source = () => readFileSync(require.resolve(`msw/${file}`), 'utf8');
  const serve: Connect.NextHandleFunction = (req, res, next) => {
    if (req.url !== `/${file}`) return next();
    res.setHeader('Content-Type', 'text/javascript');
    res.setHeader('Service-Worker-Allowed', '/');
    res.end(source());
  };
  return {
    name: 'hangar-mock-service-worker',
    apply: () => enabled,
    configureServer(server) {
      server.middlewares.use(serve);
    },
    configurePreviewServer(server) {
      server.middlewares.use(serve);
    },
    generateBundle() {
      this.emitFile({ type: 'asset', fileName: file, source: source() });
    },
  };
}

export default defineConfig(({ mode }) => {
  const env = { ...loadEnv(mode, process.cwd(), ''), ...process.env };
  const mocks = env.VITE_API_MOCKS === 'true';
  const backend = env.HANGAR_BACKEND_URL ?? 'http://localhost:8080';

  return {
    plugins: [react(), mockServiceWorker(mocks)],
    server: {
      port: 5173,
      proxy: {
        '/api': { target: backend },
        '/v2': { target: backend },
        '/repository': { target: backend },
      },
    },
    preview: {
      port: 4173,
      strictPort: true,
    },
    test: {
      environment: 'jsdom',
      globals: true,
      setupFiles: ['./src/test/setup.ts'],
      include: ['src/**/*.test.{ts,tsx}'],
      css: false,
    },
  };
});
