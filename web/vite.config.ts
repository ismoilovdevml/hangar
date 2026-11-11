/// <reference types="vitest/config" />
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { extname, join, resolve } from 'node:path';
import { brotliCompressSync, constants as zlib, gzipSync } from 'node:zlib';
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

/** Text files worth compressing; fonts and images are compressed already. */
const COMPRESSIBLE = new Set(['.js', '.mjs', '.css', '.html', '.svg', '.json', '.txt', '.map', '.webmanifest']);

/**
 * Writes `<file>.br` and `<file>.gz` next to every compressible build file, so
 * the server sends them to browsers that accept the encoding without
 * compressing on each request (internal/webui). Variants that would not save
 * at least a tenth are skipped. Both encoders are deterministic, so builds
 * stay reproducible.
 */
function precompress(): Plugin {
  let outDir = 'dist';
  const walk = (dir: string): string[] =>
    readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
      e.isDirectory() ? walk(join(dir, e.name)) : [join(dir, e.name)],
    );
  return {
    name: 'hangar-precompress',
    apply: 'build',
    configResolved(config) {
      outDir = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      for (const file of walk(outDir)) {
        if (!COMPRESSIBLE.has(extname(file))) continue;
        const source = readFileSync(file);
        if (source.length < 1024) continue;
        const variants: [string, Buffer][] = [
          ['.br', brotliCompressSync(source, {
            params: {
              [zlib.BROTLI_PARAM_QUALITY]: zlib.BROTLI_MAX_QUALITY,
              [zlib.BROTLI_PARAM_SIZE_HINT]: source.length,
            },
          })],
          ['.gz', gzipSync(source, { level: zlib.Z_BEST_COMPRESSION })],
        ];
        for (const [ext, data] of variants) {
          if (data.length <= source.length * 0.9) writeFileSync(file + ext, data);
        }
      }
    },
  };
}

/**
 * Moves the built style sheet and entry script from <head> to the end of
 * <body>, after the boot screen in index.html. In <head> the style sheet
 * blocks the first paint, so on a slow link the boot screen would only show
 * once the whole sheet has arrived. The script stays after the sheet, so the
 * app still never renders unstyled.
 */
function bootScreenFirst(): Plugin {
  return {
    name: 'hangar-boot-screen-first',
    apply: 'build',
    transformIndexHtml: {
      order: 'post',
      handler(html) {
        const tags: string[] = [];
        const head = html.replace(
          /\n?\s*<(?:link rel="(?:stylesheet|modulepreload)"|script type="module")[^>]*>(?:<\/script>)?/g,
          (tag) => {
            tags.push(tag.trim());
            return '';
          },
        );
        // Style sheets first: a module script waits for the sheets before it.
        tags.sort((a, b) => Number(a.startsWith('<script')) - Number(b.startsWith('<script')));
        return head.replace(/\s*<\/body>/, `\n${tags.map((t) => `    ${t}\n`).join('')}  </body>`);
      },
    },
  };
}

export default defineConfig(({ mode }) => {
  const env = { ...loadEnv(mode, process.cwd(), ''), ...process.env };
  const mocks = env.VITE_API_MOCKS === 'true';
  const backend = env.HANGAR_BACKEND_URL ?? 'http://localhost:8080';
  // The Host header is kept (no changeOrigin), so the server's same-origin check
  // sees the browser's Origin and Host agree. In mock mode nothing is proxied.
  const proxy = mocks
    ? undefined
    : {
        '/api': { target: backend },
        '/v2': { target: backend },
        '/repository': { target: backend },
      };

  return {
    plugins: [react(), mockServiceWorker(mocks), bootScreenFirst(), precompress()],
    build: {
      rolldownOptions: {
        output: {
          codeSplitting: {
            // Everything the first paint needs (React, the router, the shell)
            // in one file; pages and their dependencies load on demand.
            groups: [{ name: 'app', tags: ['$initial'] }],
          },
        },
      },
    },
    server: {
      port: 5173,
      proxy,
    },
    preview: {
      port: 4173,
      strictPort: true,
      proxy,
    },
    test: {
      environment: 'jsdom',
      globals: true,
      setupFiles: ['./src/test/setup.ts'],
      include: ['src/**/*.test.{ts,tsx}'],
      css: false,
      // Tests render whole pages against the mock server; leave room for a
      // loaded machine (parallel workers, CI runners). See src/test/setup.ts.
      testTimeout: 20_000,
    },
  };
});
