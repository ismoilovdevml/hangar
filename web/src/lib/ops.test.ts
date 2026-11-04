import { describe, expect, it } from 'vitest';
import type { CleanupPolicy, TaskList, TaskRun } from '../api/opsTypes';
import { auditExportUrl, auditQuery, flattenDetails, isoToUtcInput, utcInputToIso, validateAuditSearch } from './audit';
import { allows } from './can';
import {
  draftFromPolicy,
  draftToInput,
  emptyDraft,
  parseGlobs,
  previewHeadline,
  previewKey,
  summarizeCriteria,
  validateDraft,
} from './cleanup';
import { configValue, flattenConfig, formatUptime, hostOf, isSecretKey, maskUrlPassword } from './system';
import {
  applyTaskFinished,
  applyTaskProgress,
  estimateRemaining,
  filterTasks,
  formatDuration,
  formatNextRun,
  formatUtc,
  remainingText,
} from './tasks';

const run = (over: Partial<TaskRun> = {}): TaskRun => ({
  id: 'run_1',
  task_id: 't_1',
  status: 'running',
  progress: 0.2,
  created_at: '2026-11-04T10:00:00Z',
  started_at: '2026-11-04T10:00:00Z',
  ...over,
});

describe('allows', () => {
  it('mirrors the server privilege rules', () => {
    expect(allows(['system:*'], 'system', 'tasks', 'admin')).toBe(true);
    expect(allows(['system:*'], 'repo', 'npm', 'read')).toBe(false);
    expect(allows(['system:tasks:admin'], 'system', 'tasks', 'write')).toBe(true);
    expect(allows(['system:tasks:read'], 'system', 'tasks', 'write')).toBe(false);
    expect(allows(['repo:npm-*:write'], 'repo', 'npm-internal', 'write')).toBe(true);
    expect(allows(['repo:npm-*:write'], 'repo', 'docker-prod', 'write')).toBe(false);
    expect(allows(['repo:npm.x:read'], 'repo', 'npmax', 'read')).toBe(false);
    expect(allows(undefined, 'system', 'audit', 'read')).toBe(false);
  });
});

describe('cleanup draft', () => {
  const policy: CleanupPolicy = {
    id: 'cp_1',
    name: 'docker-lean',
    description: 'keep it lean',
    format: 'docker',
    criteria: { older_than_days: 90, keep_newest: 20, keep_matching: ['release-*', 'latest'] },
    repositories: ['docker-prod', 'docker-staging'],
    schedule: '0 2 * * *',
    enabled: true,
    schedule_text: 'Every day at 02:00 UTC',
    created_at: '2026-01-01T00:00:00Z',
  };

  it('round-trips a saved policy', () => {
    const d = draftFromPolicy(policy);
    expect(d.notDownloaded.on).toBe(false);
    expect(d.schedule.mode).toBe('daily');
    expect(validateDraft(d)).toEqual({});
    expect(draftToInput(d, 'tok')).toEqual({
      name: 'docker-lean',
      description: 'keep it lean',
      format: 'docker',
      criteria: { older_than_days: 90, keep_newest: 20, keep_matching: ['release-*', 'latest'] },
      repositories: ['docker-prod', 'docker-staging'],
      schedule: '0 2 * * *',
      enabled: true,
      preview_token: 'tok',
    });
  });

  it('validates names, numbers, conditions, repositories and the schedule', () => {
    const d = emptyDraft();
    expect(Object.keys(validateDraft(d)).sort()).toEqual(['name', 'repositories']);
    const bad = {
      ...d,
      name: 'Bad Name',
      olderThan: { on: true, value: '0' },
      notDownloaded: { on: true, value: 'x' },
      keepNewest: { on: true, value: '-1' },
      repositories: ['a'],
      schedule: { ...d.schedule, mode: 'custom' as const, cron: 'every day' },
    };
    expect(Object.keys(validateDraft(bad)).sort()).toEqual(['keepNewest', 'name', 'notDownloaded', 'olderThan', 'schedule']);
    const none = { ...d, name: 'ok-name', repositories: ['a'], olderThan: { on: false, value: '1' }, notDownloaded: { on: false, value: '1' }, keepNewest: { on: false, value: '1' } };
    expect(validateDraft(none).criteria).toMatch(/every version is selected/);
  });

  it('marks a preview stale only when what gets deleted changes', () => {
    const d = draftFromPolicy(policy);
    const key = previewKey(d);
    expect(previewKey({ ...d, name: 'other', description: 'x', enabled: false })).toBe(key);
    expect(previewKey({ ...d, repositories: ['docker-staging', 'docker-prod'] })).toBe(key);
    expect(previewKey({ ...d, olderThan: { on: true, value: '91' } })).not.toBe(key);
    expect(previewKey({ ...d, repositories: ['docker-prod'] })).not.toBe(key);
    expect(previewKey({ ...d, format: '' })).not.toBe(key);
  });

  it('parses glob lists and summarizes criteria', () => {
    expect(parseGlobs(' release-*, latest\nv*.*.*  latest ')).toEqual(['release-*', 'latest', 'v*.*.*']);
    expect(summarizeCriteria(policy.criteria)).toBe('older than 90 days · keep newest 20 · keep release-*, latest');
    expect(summarizeCriteria({ prerelease_only: true, not_downloaded_days: 14 })).toBe('pre-releases only · unused 14 days');
    expect(summarizeCriteria({})).toBe('No conditions');
  });

  it('phrases the preview headline', () => {
    expect(previewHeadline(18240, 212e9)).toBe('18,240 versions · 212 GB would be freed');
    expect(previewHeadline(1, 4e6)).toBe('1 version · 4 MB would be freed');
    expect(previewHeadline(0, 0)).toBe('Nothing would be deleted');
  });
});

describe('task helpers', () => {
  const list: TaskList = {
    items: [
      { id: 't_1', name: 'Rebuild index', type: 'reindex', schedule_text: 'Manual', enabled: true, last_run: null },
      { id: 't_2', name: 'Compact', description: 'soft-deleted blobs', type: 'blob_compaction', schedule_text: 'x', enabled: true, last_run: run({ id: 'r2', task_id: 't_2', status: 'failed' }) },
      { id: 't_3', name: 'Backup', type: 'db_backup', schedule_text: 'x', enabled: true, last_run: run({ id: 'r3', task_id: 't_3', status: 'succeeded' }) },
    ],
    running: [run()],
  };

  it('filters by type, status and text', () => {
    expect(filterTasks(list, { status: 'running' }).map((t) => t.id)).toEqual(['t_1']);
    expect(filterTasks(list, { status: 'failed' }).map((t) => t.id)).toEqual(['t_2']);
    expect(filterTasks(list, { status: 'never' }).map((t) => t.id)).toEqual(['t_1']);
    expect(filterTasks(list, { type: 'db_backup' }).map((t) => t.id)).toEqual(['t_3']);
    expect(filterTasks(list, { q: 'SOFT-deleted' }).map((t) => t.id)).toEqual(['t_2']);
  });

  it('applies progress without going backwards and ignores other runs', () => {
    const a = applyTaskProgress(list, { task_id: 't_1', run_id: 'run_1', progress: 0.64, message: 'indexing maven-all' });
    expect(a.running[0]).toMatchObject({ progress: 0.64, message: 'indexing maven-all' });
    const b = applyTaskProgress(a, { task_id: 't_1', run_id: 'run_1', progress: 0.5 });
    expect(b.running[0]!.progress).toBe(0.64);
    expect(applyTaskProgress(list, { task_id: 't_9', progress: 0.5 })).toBe(list);
  });

  it('moves a finished run into last_run', () => {
    const done = run({ status: 'succeeded', progress: 1, finished_at: '2026-11-04T10:05:00Z' });
    const next = applyTaskFinished(list, done);
    expect(next.running).toEqual([]);
    expect(next.items[0]!.last_run).toEqual(done);
  });

  it('formats durations and estimates time left', () => {
    expect(formatDuration(4_000)).toBe('4 s');
    expect(formatDuration(75_000)).toBe('1 min 15 s');
    expect(formatDuration(41 * 60_000)).toBe('41 min');
    expect(formatDuration(3_900_000)).toBe('1 h 5 min');
    const now = new Date('2026-11-04T10:04:00Z');
    expect(estimateRemaining(run({ progress: 0.5 }), now)).toBe(240_000);
    expect(remainingText(estimateRemaining(run({ progress: 0.5 }), now))).toBe('about 4 min left');
    expect(estimateRemaining(run({ progress: 0.01 }), now)).toBeNull();
    expect(remainingText(30_000)).toBe('less than a minute left');
  });

  it('describes next runs in UTC relative to now', () => {
    const now = new Date('2026-11-04T10:30:00Z'); // Wednesday
    expect(formatNextRun('2026-11-04T10:39:00Z', now)).toBe('In 9 min');
    expect(formatNextRun('2026-11-04T10:30:30Z', now)).toBe('Due now');
    expect(formatNextRun('2026-11-04T22:00:00Z', now)).toBe('Today 22:00');
    expect(formatNextRun('2026-11-05T02:00:00Z', now)).toBe('Tomorrow 02:00');
    expect(formatNextRun('2026-11-08T03:00:00Z', now)).toBe('Sun 03:00');
    expect(formatNextRun('2026-11-20T03:00:00Z', now)).toBe('20 Nov 03:00');
    expect(formatUtc('2026-03-14T09:52:07Z', { seconds: true })).toBe('14 Mar 2026, 09:52:07 UTC');
  });
});

describe('audit helpers', () => {
  const now = new Date('2026-11-04T12:00:00Z');

  it('reads the URL defensively', () => {
    expect(validateAuditSearch({ actor: ' gitea-runner ', outcome: 'maybe', range: 'forever', from: 'nope', cursor: '' })).toEqual({
      actor: 'gitea-runner',
      action: undefined,
      target: undefined,
      outcome: undefined,
      range: undefined,
      from: undefined,
      to: undefined,
      cursor: undefined,
    });
  });

  it('turns presets and custom ranges into from/to', () => {
    expect(auditQuery({}, now)).toMatchObject({ from: '2026-10-28T12:00:00.000Z', to: undefined });
    expect(auditQuery({ range: '1h', outcome: 'denied' }, now)).toMatchObject({ from: '2026-11-04T11:00:00.000Z', outcome: 'denied' });
    expect(auditQuery({ range: 'custom', from: '2026-11-01T00:00:00.000Z', to: '2026-11-02T00:00:00.000Z' }, now)).toMatchObject({
      from: '2026-11-01T00:00:00.000Z',
      to: '2026-11-02T00:00:00.000Z',
    });
  });

  it('builds export links with the same filters and no cursor', () => {
    const url = auditExportUrl({ action: 'repository.*', range: '24h', cursor: 'c1' }, 'csv', now);
    const u = new URL(url, 'http://x');
    expect(u.pathname).toBe('/api/v1/audit/export');
    expect(Object.fromEntries(u.searchParams)).toEqual({ format: 'csv', action: 'repository.*', from: '2026-11-03T12:00:00.000Z' });
  });

  it('converts datetime-local values as UTC', () => {
    expect(utcInputToIso('2026-11-04T09:30')).toBe('2026-11-04T09:30:00.000Z');
    expect(utcInputToIso('')).toBeUndefined();
    expect(isoToUtcInput('2026-11-04T09:30:00.000Z')).toBe('2026-11-04T09:30');
  });

  it('flattens nested details', () => {
    expect(flattenDetails({ repo: 'npm', change: { from: 'allow', to: 'deny' }, tags: ['a', 'b'], note: null })).toEqual([
      ['repo', 'npm'],
      ['change.from', 'allow'],
      ['change.to', 'deny'],
      ['tags', 'a, b'],
      ['note', '—'],
    ]);
  });
});

describe('system helpers', () => {
  it('formats uptime', () => {
    expect(formatUptime(45)).toBe('45 s');
    expect(formatUptime(3_720)).toBe('1 h 2 min');
    expect(formatUptime(93_784)).toBe('1 d 2 h');
  });

  it('masks secrets even when the server did not', () => {
    expect(isSecretKey('secret_access_key')).toBe(true);
    expect(isSecretKey('token_key')).toBe(true);
    expect(isSecretKey('session_token_ttl')).toBe(false);
    expect(isSecretKey('max_token_lifetime')).toBe(false);
    expect(maskUrlPassword('postgres://hangar:s3cret@db:5432/hangar')).toBe('postgres://hangar:••••••@db:5432/hangar');
    expect(maskUrlPassword('https://example.com/x')).toBe('https://example.com/x');

    const sections = flattenConfig({
      config: {
        server: { base_url: 'https://hangar.acme.example', trusted_proxies: ['10.0.0.0/8'] },
        database: { url: 'postgres://hangar:REDACTED@db/hangar', max_conns: 20 },
        storage: { s3: { bucket: 'b', secret_access_key: 'leaked' } },
        docker: { token_key: 'REDACTED' },
      },
      sources: { 'server.base_url': 'env' },
    });
    const all = Object.fromEntries(sections.flatMap((s) => s.entries.map((e) => [e.key, e])));
    expect(sections.map((s) => s.name)).toEqual(['server', 'database', 'storage', 'docker']);
    expect(all['server.base_url']).toMatchObject({ value: 'https://hangar.acme.example', secret: false, source: 'env' });
    expect(all['server.trusted_proxies']!.value).toBe('10.0.0.0/8');
    expect(all['database.url']).toMatchObject({ value: 'postgres://hangar:••••••@db/hangar', secret: true });
    expect(all['storage.s3.secret_access_key']).toMatchObject({ value: '••••••', secret: true });
    expect(all['docker.token_key']).toMatchObject({ value: '••••••', secret: true });
  });

  it('reads hosts and dotted config values', () => {
    expect(hostOf('https://hangar.acme.example:8443/')).toBe('hangar.acme.example:8443');
    expect(hostOf('not a url')).toBeUndefined();
    expect(configValue({ config: { server: { base_url: 'x' } } }, 'server.base_url')).toBe('x');
    expect(configValue(undefined, 'server.base_url')).toBeUndefined();
  });
});
