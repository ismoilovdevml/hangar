/**
 * Mock API for the operations screens: cleanup policies (with preview and
 * runs), scheduled tasks (runs that progress over the event stream, logs),
 * the audit log with export, cluster nodes and the effective configuration.
 *
 * Endpoints not in api/openapi.yaml yet are marked "not in the contract yet".
 */
import { delay, http, HttpResponse } from 'msw';
import type {
  AuditEvent,
  CleanupPolicy,
  CleanupPolicyInput,
  CleanupPreview,
  CleanupRun,
  ClusterStatus,
  SystemConfig,
  Task,
  TaskLogLine,
  TaskRun,
  TaskType,
} from '../api/opsTypes';
import type { ApiError, Format, PublishedComponent } from '../api/types';
import { describeCron, nextRuns, parseCron } from '../lib/cron';
import * as data from './data';

const api = (path: string) => `*/api/v1${path}`;
type Guard = (request: Request, mutating?: boolean) => Response | null;

const GB = 1e9;
const MB = 1e6;
const DAY = 86_400_000;

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });

/* ---------------------------------------------------------------- live events */

type MockEmit = (type: string, data: unknown) => void;
const liveSubscribers = new Set<MockEmit>();

/** The mocked /events stream subscribes here to forward task events. */
export function onMockLiveEvent(fn: MockEmit): () => void {
  liveSubscribers.add(fn);
  return () => {
    liveSubscribers.delete(fn);
  };
}

function emit(type: string, payload: unknown) {
  for (const fn of liveSubscribers) fn(type, payload);
}

/* ---------------------------------------------------------------- seed data */

/** A version a cleanup policy may select. Size and last download drive the preview. */
interface Candidate {
  component: PublishedComponent & { size_bytes: number };
  prerelease: boolean;
}

const PACKAGES: [repo: string, format: Format, name: string, versions: [v: string, ageDays: number, unusedDays: number | null, mb: number][]][] = [
  ['docker-prod', 'docker', 'payments-api', [['2.14.0', 1, 0, 172], ['2.13.2', 40, 2, 171], ['2.3.0', 235, 154, 172], ['2.2.9', 247, null, 171], ['2.2.8', 260, 200, 170]]],
  ['docker-prod', 'docker', 'billing-worker', [['1.9.4', 177, 145, 138], ['1.9.3', 190, 160, 137], ['1.4.2', 3, 0, 140]]],
  ['docker-staging', 'docker', 'web-frontend', [['pr-1182', 137, 136, 64], ['pr-1175', 140, 139, 63], ['pr-1203', 6, 1, 66], ['release-2026.10', 25, 3, 65]]],
  ['dockerhub-proxy', 'docker', 'node', [['18-alpine', 268, 126, 48], ['20-alpine', 30, 0, 51], ['latest', 2, 0, 52]]],
  ['docker-dev', 'docker', 'auth-gateway', [['feat-sso-3', 95, 92, 210], ['feat-sso-2', 110, 101, 208], ['3.2.0', 22, 1, 212]]],
  ['maven-snapshots', 'maven', 'ledger-core', [['7.0.0-SNAPSHOT', 20, 19, 18], ['6.9.0-SNAPSHOT', 64, 60, 18], ['7.0.0-rc2', 4, 0, 18]]],
  ['npm-internal', 'npm', '@acme/ui-kit', [['5.3.1', 1, 0, 2], ['5.0.0-beta.3', 120, 118, 2], ['4.8.0', 200, 150, 2]]],
  ['pypi-internal', 'pypi', 'acme-ml-utils', [['0.9.4', 9, 0, 4], ['0.9.0rc1', 75, 70, 4], ['0.7.2', 320, 300, 4]]],
];

const PRERELEASE = /(-|\.|^)(snapshot|rc|beta|alpha|pr-|feat-)|rc\d|SNAPSHOT/i;

function candidates(now: number): Candidate[] {
  let n = 0;
  return PACKAGES.flatMap(([repo, format, name, versions]) =>
    versions.map(([version, age, unused, mb]) => ({
      prerelease: PRERELEASE.test(version),
      component: {
        id: `pv_${++n}`,
        format,
        name,
        version,
        repository: repo,
        publisher: { kind: 'ci' as const, display_name: `CI · build ${8000 + n}` },
        published_at: new Date(now - age * DAY).toISOString(),
        last_downloaded_at: unused === null ? null : new Date(now - unused * DAY).toISOString(),
        download_count: unused === null ? 0 : 10 + n * 7,
        scan: { state: 'clean' as const, critical: 0, high: 0, medium: 0, low: 0 },
        size_bytes: mb * MB,
      },
    })),
  );
}

function globMatch(glob: string, s: string): boolean {
  const re = new RegExp(`^${glob.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.')}$`);
  return re.test(s);
}

/** Applies criteria the way the server describes them: all conditions AND, keep rules win. */
export function selectForCleanup(input: CleanupPolicyInput, now = Date.now()) {
  const c = input.criteria ?? {};
  const byPackage = new Map<string, Candidate[]>();
  for (const cand of candidates(now)) {
    if (!input.repositories.includes(cand.component.repository)) continue;
    if (input.format && cand.component.format !== input.format) continue;
    const key = `${cand.component.repository}/${cand.component.name}`;
    byPackage.set(key, [...(byPackage.get(key) ?? []), cand]);
  }
  const picked: Candidate[] = [];
  for (const list of byPackage.values()) {
    const newest = [...list].sort((a, b) => b.component.published_at.localeCompare(a.component.published_at));
    newest.forEach((cand, i) => {
      const comp = cand.component;
      const age = (now - Date.parse(comp.published_at)) / DAY;
      const unused = comp.last_downloaded_at ? (now - Date.parse(comp.last_downloaded_at)) / DAY : Infinity;
      if (c.older_than_days && age < c.older_than_days) return;
      if (c.not_downloaded_days && unused < c.not_downloaded_days) return;
      if (c.prerelease_only && !cand.prerelease) return;
      if (c.keep_newest !== undefined && i < c.keep_newest) return;
      if (c.keep_matching?.some((g) => globMatch(g, comp.version))) return;
      picked.push(cand);
    });
  }
  return picked.sort((a, b) => b.component.size_bytes - a.component.size_bytes);
}

const SEED_POLICIES: CleanupPolicyInput[] = [
  ...data.cleanupPolicies.map(({ id: _id, schedule_text: _t, created_at: _c, next_run_at: _n, ...p }) => p),
  {
    name: 'docker-lean', description: 'Docker images · keep it lean', format: 'docker',
    criteria: { older_than_days: 90, not_downloaded_days: 60, keep_newest: 2, keep_matching: ['release-*', 'latest', 'v*.*.*'] },
    repositories: ['docker-prod', 'docker-staging', 'dockerhub-proxy'], schedule: '0 2 * * *', enabled: true,
  },
  {
    name: 'npm-proxy-cache', description: 'npm proxy cache · unused 120 days', format: 'npm',
    criteria: { not_downloaded_days: 120 }, repositories: ['npmjs-proxy'], schedule: '30 2 * * *', enabled: true,
  },
  {
    name: 'pypi-prereleases', description: 'PyPI pre-releases · dev, rc builds', format: 'pypi',
    criteria: { older_than_days: 30, prerelease_only: true }, repositories: ['pypi-internal'], schedule: '0 4 * * 0', enabled: true,
  },
  {
    name: 'raw-build-outputs', description: 'Raw build outputs', format: 'raw',
    criteria: { older_than_days: 180, keep_newest: 5 }, repositories: ['raw-artifacts'], schedule: null, enabled: false,
  },
];

function policyFrom(input: CleanupPolicyInput, id: string, createdAt: string, now: number): CleanupPolicy {
  const rest = { ...input };
  delete rest.preview_token;
  const schedule = input.schedule ?? null;
  const enabled = input.enabled ?? true;
  return {
    ...rest,
    schedule,
    enabled,
    id,
    created_at: createdAt,
    schedule_text: schedule ? (describeCron(schedule) ?? schedule) : 'Manual only',
    next_run_at: schedule && enabled ? (nextRuns(schedule, new Date(now), 1)[0]?.toISOString() ?? null) : null,
  };
}

interface TaskSeed {
  id: string;
  name: string;
  type: TaskType;
  description: string;
  schedule: string | null;
  last?: { status: TaskRun['status']; message: string; minutes: number; agoMin: number };
}

const TASK_SEEDS: TaskSeed[] = [
  { id: 't_cleanup_docker', name: 'Cleanup · docker-lean', type: 'cleanup', description: 'Applies policy docker-lean to 3 repositories', schedule: '0 2 * * *', last: { status: 'succeeded', message: 'Freed 31 GB', minutes: 4, agoMin: 420 } },
  { id: 't_compact', name: 'Compact blob store · s3-eu-primary', type: 'blob_compaction', description: 'Removes soft-deleted blobs', schedule: '0 3 * * 0', last: { status: 'succeeded', message: 'Freed 180 GB', minutes: 41, agoMin: 3 * 1440 } },
  { id: 't_rescan', name: 'Rescan all packages', type: 'rescan', description: 'Against latest advisories', schedule: '0 */6 * * *', last: { status: 'succeeded', message: '7 new findings', minutes: 18, agoMin: 260 } },
  { id: 't_docker_gc', name: 'Docker garbage collection', type: 'docker_gc', description: 'Unreferenced layers older than 7 days', schedule: '0 4 * * *', last: { status: 'succeeded', message: '2.1k layers', minutes: 9, agoMin: 360 } },
  { id: 't_reindex', name: 'Rebuild search index · maven-all', type: 'reindex', description: 'Manual run', schedule: null },
  { id: 't_dirsync', name: 'Sync users from Active Directory', type: 'directory_sync', description: 'Groups → roles mapping', schedule: '*/15 * * * *', last: { status: 'succeeded', message: '3 changes', minutes: 0, agoMin: 6 } },
  { id: 't_backup', name: 'Backup database', type: 'db_backup', description: 'PostgreSQL → minio-onprem/backups', schedule: '0 1 * * *', last: { status: 'succeeded', message: '2.4 GB', minutes: 6, agoMin: 540 } },
  { id: 't_replicate', name: 'Replicate to us-east', type: 'replication', description: 'docker-prod, helm-stable', schedule: '*/30 * * * *', last: { status: 'failed', message: 'Retrying · 2 failed: connection refused', minutes: 1, agoMin: 12 } },
];

function auditSeed(now: number): AuditEvent[] {
  const templates: Omit<AuditEvent, 'id' | 'at'>[] = [
    { actor: 'd.ismoilov', action: 'cleanup.policy.update', target: 'cleanup:docker-lean', outcome: 'success', ip: '10.4.2.18', details: { changed: ['criteria.older_than_days'], from: 60, to: 90 } },
    { actor: 'gitea-runner', action: 'component.publish', target: 'repo:docker-prod', outcome: 'success', ip: '10.8.0.4', details: { component: 'payments-api:2.14.0', size_bytes: 172_000_000 } },
    { actor: 'system', action: 'component.quarantine', target: 'repo:maven-releases', outcome: 'success', ip: null, details: { component: 'legacy-reports:1.4.0', reason: 'critical CVE-2026-1182' } },
    { actor: 'j.karimov', action: 'token.create', target: 'token:laptop-cli', outcome: 'success', ip: '10.4.7.33', details: { expires_in_days: 30, scopes: ['repo:npm-*:write'] } },
    { actor: 'n.tursunova', action: 'role.bind', target: 'repo:npm-internal', outcome: 'success', ip: '10.4.2.40', details: { user: 'm.nazarova', role: 'Publisher' } },
    { actor: 'unknown', action: 'auth.login.failed', target: 'user:admin', outcome: 'denied', ip: '185.22.10.9', details: { reason: 'invalid password', attempts: 5 } },
    { actor: 'argo-cd', action: 'component.delete', target: 'repo:helm-charts', outcome: 'denied', ip: '10.8.3.12', details: { reason: 'missing repo:helm-charts:delete' } },
    { actor: 'system', action: 'task.run', target: 'task:t_replicate', outcome: 'error', ip: null, details: { error: 'connection refused', node: 'hangar-1' } },
    { actor: 'd.ismoilov', action: 'repository.create', target: 'repo:npm-sandbox', outcome: 'success', ip: '10.4.2.18', details: { format: 'npm', type: 'hosted' } },
    { actor: 'd.ismoilov', action: 'auth.login', target: 'user:d.ismoilov', outcome: 'success', ip: '10.4.2.18', details: { method: 'oidc' } },
  ];
  const out: AuditEvent[] = [];
  for (let i = 0; i < 140; i++) {
    const t = templates[i % templates.length]!;
    // Spread over ~30 days, denser recently.
    const minutesAgo = Math.round(((i * i) / 140) * 310 + i * 3);
    out.push({ ...t, id: 5000 - i, at: new Date(now - minutesAgo * 60_000 - 8_000).toISOString() });
  }
  return out;
}

/* ---------------------------------------------------------------- state */

interface ActiveRun {
  run: TaskRun;
  timer?: ReturnType<typeof setInterval>;
}

function initialState(now = Date.now()) {
  const policies = SEED_POLICIES.map((p, i) => policyFrom(p, `cp_${String(i + 1).padStart(2, '0')}`, '2026-02-01T10:00:00Z', now));
  const runs = new Map<string, TaskRun[]>();
  const logs = new Map<string, TaskLogLine[]>();
  const tasks: Task[] = TASK_SEEDS.map((s) => {
    const history: TaskRun[] = [];
    if (s.last) {
      // A history long enough to page through (newest first).
      const count = s.id === 't_rescan' ? 26 : 6;
      for (let k = 0; k < count; k++) {
        const end = now - (s.last.agoMin + k * 360) * 60_000;
        const started = end - Math.max(2, s.last.minutes * 60) * 1000;
        const status = k === 0 ? s.last.status : k % 5 === 3 ? 'failed' : 'succeeded';
        const run: TaskRun = {
          id: `${s.id}_r${count - k}`,
          task_id: s.id,
          task_name: s.name,
          status,
          progress: status === 'succeeded' ? 1 : 0.62,
          message: k === 0 ? s.last.message : status === 'failed' ? 'Upstream timed out after 30 s' : s.last.message,
          triggered_by: k % 4 === 1 ? 'd.ismoilov' : 'schedule',
          node: k % 2 ? 'hangar-2' : 'hangar-1',
          created_at: new Date(started - 1000).toISOString(),
          started_at: new Date(started).toISOString(),
          finished_at: new Date(end).toISOString(),
        };
        history.push(run);
        logs.set(run.id, [
          { seq: 1, at: run.started_at!, level: 'info', message: `started on ${run.node}` },
          { seq: 2, at: run.started_at!, level: 'info', message: s.description },
          status === 'failed'
            ? { seq: 3, at: run.finished_at!, level: 'error', message: run.message ?? 'failed' }
            : { seq: 3, at: run.finished_at!, level: 'info', message: `done: ${run.message}` },
        ]);
      }
    }
    runs.set(s.id, history);
    return {
      id: s.id,
      name: s.name,
      type: s.type,
      description: s.description,
      schedule: s.schedule,
      enabled: true,
      config: {},
      schedule_text: s.schedule ? (describeCron(s.schedule) ?? s.schedule) : 'Manual',
      next_run_at: s.schedule ? (nextRuns(s.schedule, new Date(now), 1)[0]?.toISOString() ?? null) : null,
      last_run: history[0] ?? null,
    };
  });

  // A manual reindex in progress, as on a busy afternoon. It does not advance on its own.
  const reindex: TaskRun = {
    id: 't_reindex_r1',
    task_id: 't_reindex',
    task_name: 'Rebuild search index · maven-all',
    status: 'running',
    progress: 0.64,
    message: 'Indexed 412,880 of 645,000 components',
    triggered_by: 'd.ismoilov',
    node: 'hangar-1',
    created_at: new Date(now - 6 * 60_000).toISOString(),
    started_at: new Date(now - 6 * 60_000).toISOString(),
    finished_at: null,
  };
  runs.set('t_reindex', [reindex]);
  logs.set(reindex.id, [
    { seq: 1, at: reindex.started_at!, level: 'info', message: 'started on hangar-1 by d.ismoilov' },
    { seq: 2, at: new Date(now - 5 * 60_000).toISOString(), level: 'info', message: 'reading components of maven-all (3 members)' },
    { seq: 3, at: new Date(now - 2 * 60_000).toISOString(), level: 'warn', message: 'maven-central: 12 components without metadata, skipped' },
    { seq: 4, at: new Date(now - 60_000).toISOString(), level: 'info', message: reindex.message! },
  ]);
  const active = new Map<string, ActiveRun>([['t_reindex', { run: reindex }]]);

  const cleanupRuns = new Map<string, CleanupRun[]>();
  cleanupRuns.set('docker-lean', [
    { id: 'cr_3', task_id: 't_cleanup_docker', task_name: 'Cleanup · docker-lean', status: 'succeeded', progress: 1, message: 'Deleted 1,204 versions', triggered_by: 'schedule', node: 'hangar-1', created_at: new Date(now - 7 * 3_600_000).toISOString(), started_at: new Date(now - 7 * 3_600_000).toISOString(), finished_at: new Date(now - 7 * 3_600_000 + 240_000).toISOString(), components_deleted: 1204, bytes_freed: 31 * GB },
    { id: 'cr_2', task_id: 't_cleanup_docker', task_name: 'Cleanup · docker-lean', status: 'failed', progress: 0.4, message: 'Blob store s3-eu-primary unavailable', triggered_by: 'schedule', node: 'hangar-2', created_at: new Date(now - 31 * 3_600_000).toISOString(), started_at: new Date(now - 31 * 3_600_000).toISOString(), finished_at: new Date(now - 31 * 3_600_000 + 95_000).toISOString(), components_deleted: 410, bytes_freed: 9 * GB },
    { id: 'cr_1', task_id: 't_cleanup_docker', task_name: 'Cleanup · docker-lean', status: 'succeeded', progress: 1, message: 'Deleted 980 versions', triggered_by: 'd.ismoilov', node: 'hangar-1', created_at: new Date(now - 55 * 3_600_000).toISOString(), started_at: new Date(now - 55 * 3_600_000).toISOString(), finished_at: new Date(now - 55 * 3_600_000 + 200_000).toISOString(), components_deleted: 980, bytes_freed: 26 * GB },
  ]);

  return {
    policies,
    nextPolicy: policies.length + 1,
    previews: new Map<string, string>(),
    tasks,
    runs,
    logs,
    active,
    cleanupRuns,
    audit: auditSeed(now),
    /** Interval between progress steps of a started run. */
    stepMs: 1_200,
    configAvailable: true,
  };
}

let state = initialState();

export function resetOpsMockState(): void {
  for (const a of state.active.values()) clearInterval(a.timer);
  state = initialState();
}

/** Appends an audit event recorded by another mock area (newest first), so GET /audit sees it. */
export function recordMockAudit(e: Omit<AuditEvent, 'id'>): void {
  const id = state.audit.reduce((m, x) => Math.max(m, x.id), 0) + 1;
  state.audit.unshift({ ...e, id });
}

/** Test helpers. */
export const opsMockState = {
  get policies(): readonly CleanupPolicy[] {
    return state.policies;
  },
  get tasks(): readonly Task[] {
    return state.tasks;
  },
  set stepMs(ms: number) {
    state.stepMs = ms;
  },
  set configAvailable(v: boolean) {
    state.configAvailable = v;
  },
  emit,
};

/* ---------------------------------------------------------------- helpers */

function previewKeyOf(input: CleanupPolicyInput): string {
  return JSON.stringify({ c: input.criteria ?? {}, r: [...(input.repositories ?? [])].sort(), f: input.format ?? null });
}

const NAME_RE = /^[a-z0-9][a-z0-9-]{1,62}$/;

function validatePolicy(input: CleanupPolicyInput): string | null {
  if (!NAME_RE.test(input.name ?? '')) return `policy name ${JSON.stringify(input.name)} is invalid`;
  if (!input.repositories?.length) return 'a policy needs at least one repository';
  if (input.schedule && !parseCron(input.schedule).ok) return `schedule ${JSON.stringify(input.schedule)} is not a 5-field cron expression`;
  const c = input.criteria ?? {};
  if (!c.older_than_days && !c.not_downloaded_days && !c.prerelease_only && c.keep_newest === undefined) {
    return 'criteria select every version; add an age, download, pre-release or keep-newest condition';
  }
  return null;
}

function csvCell(v: string | number | null | undefined): string {
  const s = v === null || v === undefined ? '' : String(v);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

function pageOf<T>(items: T[], cursor: string | null, limit: number) {
  const offset = cursor && /^o\d+$/.test(cursor) ? Number(cursor.slice(1)) : 0;
  const page = items.slice(offset, offset + limit);
  return { items: page, next_cursor: offset + limit < items.length ? `o${offset + limit}` : null };
}

function log(runId: string, level: TaskLogLine['level'], message: string) {
  const lines = state.logs.get(runId) ?? [];
  lines.push({ seq: (lines[lines.length - 1]?.seq ?? 0) + 1, at: new Date().toISOString(), level, message });
  state.logs.set(runId, lines);
}

function finish(taskId: string, status: TaskRun['status'], message: string) {
  const a = state.active.get(taskId);
  if (!a) return;
  clearInterval(a.timer);
  state.active.delete(taskId);
  const run: TaskRun = { ...a.run, status, message, finished_at: new Date().toISOString(), progress: status === 'succeeded' ? 1 : a.run.progress };
  state.runs.set(taskId, (state.runs.get(taskId) ?? []).map((r) => (r.id === run.id ? run : r)));
  state.tasks = state.tasks.map((t) => (t.id === taskId ? { ...t, last_run: run } : t));
  log(run.id, status === 'succeeded' ? 'info' : 'warn', status === 'succeeded' ? `done: ${message}` : message);
  emit('task.finished', run);
}

function startRun(task: Task, triggeredBy: string): TaskRun {
  const now = new Date().toISOString();
  const history = state.runs.get(task.id) ?? [];
  const run: TaskRun = {
    id: `${task.id}_r${history.length + 1}_${Date.now().toString(36)}`,
    task_id: task.id,
    task_name: task.name,
    status: 'running',
    progress: 0,
    message: 'Starting',
    triggered_by: triggeredBy,
    node: 'hangar-1',
    created_at: now,
    started_at: now,
    finished_at: null,
  };
  state.runs.set(task.id, [run, ...history]);
  log(run.id, 'info', `started on hangar-1 by ${triggeredBy}`);
  const active: ActiveRun = { run };
  active.timer = setInterval(() => {
    const progress = Math.min(1, Math.round((active.run.progress + 0.2) * 100) / 100);
    if (progress >= 1) {
      finish(task.id, 'succeeded', 'Completed');
      return;
    }
    const message = `Processed ${Math.round(progress * 100)} %`;
    active.run = { ...active.run, progress, message };
    state.runs.set(task.id, (state.runs.get(task.id) ?? []).map((r) => (r.id === run.id ? active.run : r)));
    log(run.id, 'info', message);
    emit('task.progress', { task_id: task.id, run_id: run.id, progress, message });
  }, state.stepMs);
  state.active.set(task.id, active);
  return run;
}

/* ---------------------------------------------------------------- handlers */

export function opsHandlers(guard: Guard) {
  return [
    /* ---------- cleanup policies ---------- */
    http.get(api('/cleanup-policies'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = new URL(request.url).searchParams;
      return HttpResponse.json(pageOf(state.policies, p.get('cursor'), Math.min(1000, Number(p.get('limit') ?? 50))));
    }),

    http.post(api('/cleanup-policies/preview'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      await delay(120);
      const input = (await request.json()) as CleanupPolicyInput;
      if (!input.repositories?.length) return error(400, { code: 'invalid_argument', message: 'choose at least one repository to preview' });
      const picked = selectForCleanup(input);
      const token = `pv_${Math.random().toString(36).slice(2, 10)}`;
      state.previews.set(token, previewKeyOf(input));
      if ((request.headers.get('Accept') ?? '').includes('text/csv')) {
        const rows = [
          'repository,format,name,version,published_at,last_downloaded_at,size_bytes',
          ...picked.map(({ component: c }) =>
            [c.repository, c.format, c.name, c.version, c.published_at, c.last_downloaded_at ?? '', c.size_bytes].map(csvCell).join(','),
          ),
        ];
        return new HttpResponse(`${rows.join('\n')}\n`, { headers: { 'Content-Type': 'text/csv; charset=utf-8' } });
      }
      const body: CleanupPreview & { sample: (PublishedComponent & { size_bytes?: number })[] } = {
        component_count: picked.length,
        bytes: picked.reduce((s, c) => s + c.component.size_bytes, 0),
        sample: picked.slice(0, 20).map((c) => c.component),
        preview_token: token,
      };
      return HttpResponse.json(body);
    }),

    http.post(api('/cleanup-policies'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const input = (await request.json()) as CleanupPolicyInput;
      const invalid = validatePolicy(input);
      if (invalid) return error(400, { code: 'invalid_argument', message: invalid });
      if (!input.preview_token || state.previews.get(input.preview_token) !== previewKeyOf(input)) {
        return error(400, { code: 'preview_required', message: 'preview these exact criteria before saving' });
      }
      if (state.policies.some((p) => p.name === input.name)) {
        return error(409, { code: 'conflict', message: `cleanup policy "${input.name}" already exists` });
      }
      const policy = policyFrom(input, `cp_${String(state.nextPolicy++).padStart(2, '0')}`, new Date().toISOString(), Date.now());
      state.policies = [...state.policies, policy];
      return HttpResponse.json(policy, { status: 201 });
    }),

    http.get(api('/cleanup-policies/:name'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = state.policies.find((x) => x.name === params.name);
      return p ? HttpResponse.json(p) : error(404, { code: 'not_found', message: `cleanup policy "${String(params.name)}" not found` });
    }),

    http.put(api('/cleanup-policies/:name'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const existing = state.policies.find((x) => x.name === params.name);
      if (!existing) return error(404, { code: 'not_found', message: `cleanup policy "${String(params.name)}" not found` });
      const input = (await request.json()) as CleanupPolicyInput;
      if (input.name !== existing.name) return error(400, { code: 'invalid_argument', message: 'a policy cannot be renamed' });
      const invalid = validatePolicy(input);
      if (invalid) return error(400, { code: 'invalid_argument', message: invalid });
      if (!input.preview_token || state.previews.get(input.preview_token) !== previewKeyOf(input)) {
        return error(400, { code: 'preview_required', message: 'preview these exact criteria before saving' });
      }
      const policy = policyFrom(input, existing.id, existing.created_at, Date.now());
      state.policies = state.policies.map((p) => (p.id === existing.id ? policy : p));
      return HttpResponse.json(policy);
    }),

    http.delete(api('/cleanup-policies/:name'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!state.policies.some((x) => x.name === params.name)) {
        return error(404, { code: 'not_found', message: `cleanup policy "${String(params.name)}" not found` });
      }
      state.policies = state.policies.filter((p) => p.name !== params.name);
      return new HttpResponse(null, { status: 204 });
    }),

    // Not in the contract yet.
    http.post(api('/cleanup-policies/:name/run'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const p = state.policies.find((x) => x.name === params.name);
      if (!p) return error(404, { code: 'not_found', message: `cleanup policy "${String(params.name)}" not found` });
      const history = state.cleanupRuns.get(p.name) ?? [];
      if (history.some((r) => r.status === 'queued' || r.status === 'running')) {
        return error(409, { code: 'conflict', message: `cleanup policy "${p.name}" is already running` });
      }
      const picked = selectForCleanup(p);
      const now = new Date().toISOString();
      const run: CleanupRun = {
        id: `cr_${Date.now().toString(36)}`,
        task_id: `cleanup:${p.name}`,
        task_name: `Cleanup · ${p.name}`,
        status: 'queued',
        progress: 0,
        message: 'Queued',
        triggered_by: 'd.ismoilov',
        created_at: now,
        started_at: null,
        finished_at: null,
      };
      state.cleanupRuns.set(p.name, [run, ...history]);
      // Completes shortly after, like a small real run.
      setTimeout(() => {
        const done: CleanupRun = {
          ...run,
          status: 'succeeded',
          progress: 1,
          started_at: now,
          finished_at: new Date().toISOString(),
          message: `Deleted ${picked.length} versions`,
          node: 'hangar-1',
          components_deleted: picked.length,
          bytes_freed: picked.reduce((s, c) => s + c.component.size_bytes, 0),
        };
        state.cleanupRuns.set(p.name, (state.cleanupRuns.get(p.name) ?? []).map((r) => (r.id === run.id ? done : r)));
      }, state.stepMs);
      return HttpResponse.json(run, { status: 202 });
    }),

    // Not in the contract yet.
    http.get(api('/cleanup-policies/:name/runs'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = new URL(request.url).searchParams;
      return HttpResponse.json(pageOf(state.cleanupRuns.get(String(params.name)) ?? [], p.get('cursor'), Number(p.get('limit') ?? 20)));
    }),

    /* ---------- tasks ---------- */
    http.get(api('/tasks'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      return HttpResponse.json({ items: state.tasks, running: [...state.active.values()].map((a) => a.run) });
    }),

    http.get(api('/tasks/:id'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      const t = state.tasks.find((x) => x.id === params.id);
      return t ? HttpResponse.json(t) : error(404, { code: 'not_found', message: `task "${String(params.id)}" not found` });
    }),

    http.post(api('/tasks/:id/run'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const t = state.tasks.find((x) => x.id === params.id);
      if (!t) return error(404, { code: 'not_found', message: `task "${String(params.id)}" not found` });
      if (state.active.has(t.id)) return error(409, { code: 'conflict', message: `task "${t.name}" is already running` });
      return HttpResponse.json(startRun(t, 'd.ismoilov'), { status: 202 });
    }),

    http.post(api('/tasks/:id/stop'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const a = state.active.get(String(params.id));
      if (!a) return error(409, { code: 'conflict', message: 'task is not running' });
      const run = a.run;
      finish(String(params.id), 'cancelled', 'Stopped by d.ismoilov');
      return HttpResponse.json({ ...run, status: 'cancelled' }, { status: 202 });
    }),

    http.get(api('/tasks/:id/runs'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = new URL(request.url).searchParams;
      return HttpResponse.json(pageOf(state.runs.get(String(params.id)) ?? [], p.get('cursor'), Number(p.get('limit') ?? 50)));
    }),

    // Not in the contract yet.
    http.get(api('/tasks/:id/runs/:runId/log'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      const lines = state.logs.get(String(params.runId));
      if (!lines) return error(404, { code: 'not_found', message: 'no log for this run' });
      const after = Number(new URL(request.url).searchParams.get('after') ?? 0);
      return HttpResponse.json({ items: lines.filter((l) => l.seq > after), truncated: false });
    }),

    /* ---------- audit ---------- */
    http.get(api('/audit'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = new URL(request.url).searchParams;
      return HttpResponse.json(pageOf(filterAudit(p), p.get('cursor'), Math.min(1000, Number(p.get('limit') ?? 50))));
    }),

    http.get(api('/audit/export'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = new URL(request.url).searchParams;
      const format = p.get('format');
      const rows = filterAudit(p).reverse(); // export is oldest first
      if (format === 'csv') {
        const lines = [
          'id,at,actor,action,target,outcome,ip,details',
          ...rows.map((e) => [e.id, e.at, e.actor, e.action, e.target, e.outcome, e.ip ?? '', JSON.stringify(e.details ?? {})].map(csvCell).join(',')),
        ];
        return new HttpResponse(`${lines.join('\n')}\n`, {
          headers: { 'Content-Type': 'text/csv; charset=utf-8', 'Content-Disposition': 'attachment; filename="hangar-audit.csv"' },
        });
      }
      if (format === 'json') {
        return new HttpResponse(rows.map((e) => JSON.stringify(e)).join('\n') + '\n', {
          headers: { 'Content-Type': 'application/x-ndjson', 'Content-Disposition': 'attachment; filename="hangar-audit.ndjson"' },
        });
      }
      return error(400, { code: 'invalid_argument', message: `format "${format}": want csv or json` });
    }),

    /* ---------- system ---------- */
    http.get(api('/nodes'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      return HttpResponse.json(clusterStatus());
    }),

    // Not in the contract yet.
    http.get(api('/system/config'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      if (!state.configAvailable) return error(501, { code: 'not_implemented', message: 'effective configuration is not available' });
      return HttpResponse.json(systemConfig);
    }),
  ];
}

function prefixMatch(pattern: string | null, value: string): boolean {
  if (!pattern) return true;
  return pattern.endsWith('*') ? value.startsWith(pattern.slice(0, -1)) : value === pattern;
}

function filterAudit(p: URLSearchParams): AuditEvent[] {
  const from = p.get('from') ? Date.parse(p.get('from')!) : -Infinity;
  const to = p.get('to') ? Date.parse(p.get('to')!) : Infinity;
  return state.audit.filter((e) => {
    const at = Date.parse(e.at);
    return (
      (!p.get('actor') || e.actor === p.get('actor')) &&
      prefixMatch(p.get('action'), e.action) &&
      prefixMatch(p.get('target'), e.target) &&
      (!p.get('outcome') || e.outcome === p.get('outcome')) &&
      at >= from &&
      at < to
    );
  });
}


function clusterStatus(): ClusterStatus {
  const now = new Date().toISOString();
  return {
    state: 'ok',
    quorum: true,
    nodes: [
      { id: 'n1', name: 'hangar-1', role: 'leader', zone: 'eu-central-1a', address: '10.0.1.11:8080', version: '1.0.0', healthy: true, cpu_pct: 34, memory_pct: 58, cache_disk_pct: 41, requests_per_second: 412, uptime_seconds: 1_209_600, last_heartbeat: now },
      { id: 'n2', name: 'hangar-2', role: 'follower', zone: 'eu-central-1b', address: '10.0.2.11:8080', version: '1.0.0', healthy: true, cpu_pct: 29, memory_pct: 61, cache_disk_pct: 78, requests_per_second: 388, uptime_seconds: 1_209_000, last_heartbeat: now },
      { id: 'n3', name: 'hangar-3', role: 'follower', zone: 'eu-central-1c', address: '10.0.3.11:8080', version: '1.0.0', healthy: true, cpu_pct: 22, memory_pct: 49, cache_disk_pct: 37, requests_per_second: 301, uptime_seconds: 86_400, last_heartbeat: now },
    ],
    dependencies: data.systemStatus.dependencies ?? [],
  };
}

const systemConfig: SystemConfig = {
  config: {
    server: { addr: ':8080', base_url: 'https://pkg.acme.io', instance_name: 'acme-platform', trusted_proxies: ['10.0.0.0/8'], shutdown_timeout: '30s' },
    log: { level: 'info', format: 'json' },
    database: { url: 'postgres://hangar:REDACTED@db.acme.internal:5432/hangar', max_conns: 40, migrate_on_start: true },
    storage: { kind: 's3', s3: { endpoint: 'https://minio.acme.internal', bucket: 'hangar', region: 'eu-central-1', access_key_id: 'REDACTED', secret_access_key: 'REDACTED', use_path_style: true } },
    nats: { embedded: true, data_dir: '/var/lib/hangar/nats' },
    http: { proxy: 'http://proxy.acme.internal:3128', no_proxy: ['.acme.internal', '10.0.0.0/8'], connect_timeout: '10s' },
    auth: { session: { ttl: '12h0m0s', remember_ttl: '720h0m0s' }, lockout: { max_attempts: 5, window: '15m0s' }, max_token_lifetime: '8760h0m0s' },
    docker: { token_key: 'REDACTED' },
    scanner: { trivy_url: 'http://trivy.acme.internal:4954' },
    metrics: { enabled: true, path: '/metrics' },
  },
  sources: { 'server.base_url': 'env', 'database.url': 'env', 'storage.s3.secret_access_key': 'env', 'docker.token_key': 'env', 'log.level': 'default' },
};

