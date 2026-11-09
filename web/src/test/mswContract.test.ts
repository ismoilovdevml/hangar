/**
 * The MSW mock server stands in for the real API in unit tests, Playwright
 * and offline UI work, so it must not invent endpoints. Every mocked
 * /api/v1 route must be an operation of api/openapi.yaml with the same
 * method; otherwise a page can pass every test against the mocks and fail
 * against the server.
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { HttpHandler } from 'msw';
import { describe, expect, it } from 'vitest';
import { handlers } from '../mocks/handlers';

const PREFIX = '/api/v1';

/**
 * Mocked routes the server does not serve. Each entry needs a reason; the
 * test fails when an entry is no longer mocked, so the list cannot go stale.
 */
const KNOWN_DRIFT: Record<string, string> = {
  'GET /system/config':
    'not in the contract and not served (404); the System and Settings pages should stop calling it',
};

type Operation = { method: string; path: string };

/** Operations of api/openapi.yaml: path keys at two spaces under `paths:`, methods at four. */
function specOperations(): Operation[] {
  // Tests run from web/ (npm test, vitest).
  const text = readFileSync(resolve(process.cwd(), '../api/openapi.yaml'), 'utf8');
  const ops: Operation[] = [];
  let inPaths = false;
  let path = '';
  for (const line of text.split('\n')) {
    if (/^\S/.test(line)) {
      inPaths = line.startsWith('paths:');
      continue;
    }
    if (!inPaths) continue;
    const p = /^ {2}(\/\S*):\s*$/.exec(line);
    if (p) {
      path = p[1]!;
      continue;
    }
    const m = /^ {4}(get|put|post|delete|patch|head|options):\s*$/.exec(line);
    if (m && path) ops.push({ method: m[1]!.toUpperCase(), path });
  }
  return ops;
}

/** A concrete URL path for a spec template: every {param} becomes "p". */
const sample = (template: string) => template.replace(/\{[^}]+\}/g, 'p');

/** A matcher for an MSW path ("/users/:id", "/x/:name\\:test"). */
function mswMatcher(path: string): RegExp {
  const parts = path.split(/(\\:|:[A-Za-z_]\w*|\*)/).filter(Boolean);
  const re = parts
    .map((part) => {
      if (part === '\\:') return ':';
      if (part === '*') return '.*';
      if (part.startsWith(':')) return '[^/]+';
      return part.replace(/[.+?^${}()|[\]\\]/g, '\\$&');
    })
    .join('');
  return new RegExp(`^${re}$`);
}

type Mocked = { key: string; method: string; matches: (path: string) => boolean };

function mockedRoutes(): Mocked[] {
  const out: Mocked[] = [];
  for (const h of handlers) {
    if (!(h instanceof HttpHandler)) continue;
    const { method, path } = h.info;
    if (typeof method !== 'string') throw new Error(`mock handler with a non-literal method: ${String(method)} ${String(path)}`);
    if (path instanceof RegExp) {
      // Regular-expression routes match the full URL path.
      if (!path.source.includes('api\\/v1')) continue;
      out.push({ key: `${method} ${path.source}`, method, matches: (rel) => path.test(PREFIX + rel) });
      continue;
    }
    const i = path.indexOf(PREFIX);
    if (i < 0) continue; // package endpoints (/repository/..., /v2/...) are not part of this contract
    const rel = path.slice(i + PREFIX.length);
    const re = mswMatcher(rel);
    out.push({ key: `${method} ${rel.replace(/\\:/g, ':')}`, method, matches: (p) => re.test(p) });
  }
  return out;
}

describe('MSW mocks follow api/openapi.yaml', () => {
  const ops = specOperations();
  const mocked = mockedRoutes();

  it('reads the contract and the mocks', () => {
    expect(ops.length).toBeGreaterThan(100);
    expect(mocked.length).toBeGreaterThan(50);
  });

  it('mocks only documented operations', () => {
    const undocumented = mocked
      .filter((m) => !KNOWN_DRIFT[m.key])
      .filter((m) => !ops.some((o) => o.method === m.method && m.matches(sample(o.path))))
      .map((m) => m.key);
    expect(undocumented, 'mocked but not in api/openapi.yaml (fix the mock or the contract)').toEqual([]);
  });

  it('lists only drift that is still mocked', () => {
    const keys = new Set(mocked.map((m) => m.key));
    const stale = Object.keys(KNOWN_DRIFT).filter((k) => !keys.has(k));
    expect(stale, 'no longer mocked: remove from KNOWN_DRIFT').toEqual([]);
  });
});
