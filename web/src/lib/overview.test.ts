import { describe, expect, it } from 'vitest';
import type { Task } from '../api/opsTypes';
import { overview } from '../mocks/data';
import { headlineUpstream, ratioPercent, storageNote, successRatio, todoItems, urlHost } from './overview';

const NOW = new Date('2026-11-04T12:00:00Z');

function task(over: Partial<Task>): Task {
  return {
    id: 't1',
    name: 'Backup database',
    type: 'db_backup',
    description: '',
    schedule: '0 1 * * *',
    schedule_text: 'Every day at 01:00',
    config: {},
    enabled: true,
    builtin: false,
    next_run_at: null,
    last_run: null,
    created_at: NOW.toISOString(),
    updated_at: NOW.toISOString(),
    ...over,
  };
}

/** Metrics with nothing to act on. */
function calm() {
  const m = overview('today', NOW.getTime());
  return { ...m, activity: { ...m.activity, pushes_rejected: 0 }, issues: { ...m.issues, critical: 0 } };
}

describe('successRatio', () => {
  it('counts server errors and refusals as failures', () => {
    expect(successRatio({ total: 1000, server_errors: 4, denied: 10, trend_pct: null, peak_total: null, p95_ms: null, coverage_start: null })).toBeCloseTo(0.986);
  });
  it('is null without requests or outcome counts', () => {
    const base = { trend_pct: null, peak_total: null, p95_ms: null, coverage_start: null };
    expect(successRatio({ ...base, total: 0, server_errors: 0, denied: 0 })).toBeNull();
    expect(successRatio({ ...base, total: null, server_errors: null, denied: null })).toBeNull();
    expect(successRatio({ ...base, total: 5, server_errors: null, denied: null })).toBeNull();
  });
  it('never rounds a failure up to 100%', () => {
    expect(ratioPercent(0.99996)).toBe('99.9');
    expect(ratioPercent(1)).toBe('100.0');
    expect(ratioPercent(0.9885)).toBe('98.8');
  });
});

describe('upstream helpers', () => {
  it('puts the worst state first, then the busiest', () => {
    const u = (name: string, state: 'healthy' | 'slow' | 'offline', requests: number) => ({ name, url: `https://${name}.example.com`, state, latency_ms: 10, requests });
    expect(headlineUpstream([u('a', 'healthy', 9), u('b', 'slow', 1), u('c', 'slow', 5)])?.name).toBe('c');
    expect(headlineUpstream([u('a', 'healthy', 9), u('b', 'offline', 0)])?.name).toBe('b');
    expect(headlineUpstream([])).toBeNull();
  });
  it('shows a host, or the raw value when it is not a URL', () => {
    expect(urlHost('https://api.nuget.org/v3/index.json')).toBe('api.nuget.org');
    expect(urlHost('proxy.example.com:3128')).toBe('proxy.example.com:3128');
  });
});

describe('storageNote', () => {
  it('names a single format and says when there is no quota', () => {
    const m = overview('today');
    const note = storageNote(
      { ...m, storage: { ...m.storage, capacity_bytes: null }, storage_by_format: [{ format: 'nuget', bytes: 421e6 }] },
      () => 'NuGet',
    );
    expect(note).toBe('All NuGet · no quota');
  });
  it('gives the biggest format share and the quota use', () => {
    expect(storageNote(overview('today'), (f) => f.toUpperCase())).toBe('DOCKER 60% · 7 formats · 40% of quota');
  });
});

describe('todoItems', () => {
  it('is empty when nothing needs attention', () => {
    expect(todoItems({ metrics: calm(), now: NOW })).toEqual([]);
  });

  it('orders urgent items before reminders and setup', () => {
    const m = overview('today', NOW.getTime());
    const items = todoItems({
      metrics: { ...m, storage: { ...m.storage, capacity_bytes: null } },
      canAdminBlobStores: true,
      tokenWarnings: [
        { id: 'k', name: 'ci-publisher', owner: 'ci', owner_kind: 'service_account', prefix: 'hgr_k', scopes: [], created_at: NOW.toISOString(), expires_at: '2026-11-05T00:00:00Z', allowed_cidrs: [], read_only: false },
      ],
      username: 'd.ismoilov',
      now: NOW,
    });
    expect(items.map((t) => [t.tone, t.id])).toEqual([
      ['bad', 'pushes'],
      ['bad', 'critical'],
      ['warn', 'token-k'],
      ['info', 'quota'],
    ]);
    // Someone else's token is renewed under Access, not on My account.
    expect(items[2]).toMatchObject({ detail: 'Expires in 12 h · ci', link: { kind: 'access-tokens' } });
  });

  it('only suggests a quota to people who can set one', () => {
    const m = calm();
    expect(todoItems({ metrics: { ...m, storage: { ...m.storage, capacity_bytes: null } }, now: NOW })).toEqual([]);
  });

  it('follows the backup tasks the user can see', () => {
    const m = calm();
    expect(todoItems({ metrics: m, tasks: [], now: NOW })).toMatchObject([{ id: 'backup', detail: 'No backup has ever run' }]);
    expect(todoItems({ metrics: m, tasks: [task({ enabled: false })], now: NOW })).toMatchObject([{ id: 'backup' }]);
    expect(todoItems({ metrics: m, tasks: [task({})], now: NOW })).toEqual([]);
    const failed = task({
      last_run: { status: 'failed', message: 'disk full' } as Task['last_run'],
    });
    expect(todoItems({ metrics: m, tasks: [failed], now: NOW })).toMatchObject([
      { id: 'backup-failed', tone: 'bad', detail: 'disk full', link: { kind: 'task', id: 't1' } },
    ]);
    // Without tasks (no permission to read them) there is no guess either way.
    expect(todoItems({ metrics: m, now: NOW })).toEqual([]);
  });

  it('reports a system that is not ok and unreachable upstreams', () => {
    const items = todoItems({
      metrics: calm(),
      system: { state: 'degraded', message: 'Search index lagging', nodes_up: 3, nodes_total: 3 },
      upstreams: [
        { name: 'nuget-proxy', url: 'https://api.nuget.org/v3/index.json', state: 'offline', latency_ms: null, requests: 0 },
        { name: 'npm-proxy', url: 'https://registry.npmjs.org', state: 'slow', latency_ms: 900, requests: 3 },
      ],
      now: NOW,
    });
    expect(items.map((t) => `${t.tag}: ${t.title}`)).toEqual(['UPSTREAM: nuget-proxy is offline', 'DEGRADED: Search index lagging']);
  });
});
