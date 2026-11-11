import { describe, expect, it } from 'vitest';
import type { Task } from '../api/opsTypes';
import { overview } from '../mocks/data';
import type { SystemStatus, UpstreamHealth } from '../api/types';
import { systemHealth } from '../api/health';
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
    expect(
      successRatio({ total: 1000, server_errors: 4, denied: 10, auth_challenges: 0, trend_pct: null, peak_total: null, p95_ms: null, coverage_start: null }),
    ).toBeCloseTo(0.986);
  });
  it('leaves authentication challenges out of both sides', () => {
    // A NuGet-heavy day: every anonymous 401 is retried with credentials and succeeds.
    const base = { trend_pct: null, peak_total: null, p95_ms: null, coverage_start: null };
    expect(successRatio({ ...base, total: 27200, server_errors: 0, denied: 0, auth_challenges: 13600 })).toBe(1);
    expect(successRatio({ ...base, total: 1100, server_errors: 4, denied: 6, auth_challenges: 100 })).toBeCloseTo(0.99);
    expect(successRatio({ ...base, total: 40, server_errors: 0, denied: 0, auth_challenges: 40 })).toBeNull();
    // Servers that predate the field.
    expect(successRatio({ ...base, total: 1000, server_errors: 4, denied: 10, auth_challenges: null })).toBeCloseTo(0.986);
  });
  it('is null without requests or outcome counts', () => {
    const base = { trend_pct: null, peak_total: null, p95_ms: null, coverage_start: null };
    expect(successRatio({ ...base, total: 0, server_errors: 0, denied: 0, auth_challenges: 0 })).toBeNull();
    expect(successRatio({ ...base, total: null, server_errors: null, denied: null, auth_challenges: null })).toBeNull();
    expect(successRatio({ ...base, total: 5, server_errors: null, denied: null, auth_challenges: null })).toBeNull();
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

  it('names upstream credential failures instead of calling the remote unreachable', () => {
    const up = (name: string, state: UpstreamHealth['state'], reason?: UpstreamHealth['reason']): UpstreamHealth => ({
      name,
      url: `https://${name}.example.com/`,
      state,
      reason,
      latency_ms: null,
      requests: 0,
    });
    const items = todoItems({
      metrics: calm(),
      upstreams: [
        up('nuget-gitlab', 'blocked', 'auth_failed'),
        up('npm-gitlab', 'blocked', 'access_denied'),
        up('raw-lan', 'blocked', 'address_blocked'),
        up('pypi', 'offline'),
      ],
      now: NOW,
    });
    expect(items.map((t) => [t.title, t.detail])).toEqual([
      ['nuget-gitlab: authentication failed (401)', 'nuget-gitlab.example.com refuses the credentials: check the proxy credentials; uncached packages fail'],
      ['npm-gitlab: access denied (403)', 'npm-gitlab.example.com refuses the credentials: check the proxy credentials; uncached packages fail'],
      ['raw-lan is blocked', 'raw-lan.example.com cannot be reached; uncached packages fail'],
      ['pypi is offline', 'pypi.example.com cannot be reached; uncached packages fail'],
    ]);
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
    // No backup task on the server: backups run outside it, so the reminder links the guide and can be dismissed.
    expect(todoItems({ metrics: m, tasks: [], now: NOW })).toMatchObject([
      { id: 'backup-outside', tone: 'info', title: 'Back up the database', dismissible: true, link: { kind: 'docs', href: expect.stringContaining('backup-restore.md') } },
    ]);
    expect(todoItems({ metrics: m, tasks: [task({ enabled: false })], now: NOW })).toMatchObject([{ id: 'backup', title: 'Schedule a database backup' }]);
    expect(todoItems({ metrics: m, tasks: [task({ enabled: false, last_run: null })], now: NOW })[0]!.dismissible).toBeUndefined();
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

describe('systemHealth', () => {
  const status = (state: SystemStatus['state'], message: string, problems?: SystemStatus['problems']) =>
    ({ state, message, problems, version: '1', instance_name: 'h', nodes_up: 1, nodes_total: 1 }) as SystemStatus;

  it('is grey while loading and when the status cannot be read, never green', () => {
    expect(systemHealth({ isError: false })).toMatchObject({ tone: 'neutral', state: 'unknown', text: 'Checking status…', loading: true });
    expect(systemHealth({ isError: true })).toMatchObject({ tone: 'neutral', state: 'unknown', text: 'Status unavailable', error: true });
    // A refresh that fails does not keep showing the old green.
    expect(systemHealth({ isError: true, data: status('ok', 'All systems normal') }).tone).toBe('neutral');
  });

  it('says what the server says', () => {
    expect(systemHealth({ isError: false, data: status('ok', 'All systems normal') })).toMatchObject({ tone: 'ok', text: 'All systems normal' });
    const down = systemHealth({ isError: false, data: status('down', 'Blob storage down', [{ name: 'blobstores', label: 'Blob storage', status: 'down' }]) });
    expect(down).toMatchObject({ tone: 'bad', text: 'Blob storage down' });
    expect(down.problems).toHaveLength(1);
    expect(systemHealth({ isError: false, data: status('degraded', '2 checks need attention') }).tone).toBe('warn');
  });
});

describe('todoItems from health', () => {
  const offline: UpstreamHealth = { name: 'broken', url: 'https://does-not-exist.invalid', state: 'offline', latency_ms: null, requests: 0 };

  it('lists each failing check, and leaves upstreams to the per-remote items', () => {
    const items = todoItems({
      metrics: calm(),
      system: {
        state: 'down',
        message: '1 check is down',
        nodes_up: 1,
        nodes_total: 1,
        problems: [
          { name: 'blobstores', label: 'Blob storage', status: 'down', detail: 's3-bad is unreachable: no such host' },
          { name: 'upstreams', label: 'Upstream proxies', status: 'degraded' },
        ],
      },
      upstreams: [offline],
      now: NOW,
    });
    expect(items.map((t) => `${t.tag}: ${t.title}: ${t.detail}`)).toEqual([
      'OUTAGE: Blob storage down: s3-bad is unreachable: no such host',
      'UPSTREAM: broken is offline: does-not-exist.invalid cannot be reached; uncached packages fail',
    ]);
  });

  it('keeps the upstream check when no remote can be named', () => {
    const items = todoItems({
      metrics: calm(),
      system: { state: 'degraded', message: 'Upstream proxies degraded', nodes_up: 1, nodes_total: 1, problems: [{ name: 'upstreams', label: 'Upstream proxies', status: 'degraded' }] },
      now: NOW,
    });
    expect(items.map((t) => t.title)).toEqual(['Upstream proxies degraded']);
  });
});
