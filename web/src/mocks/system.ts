/**
 * Mock API for the System status and System logs screens: three nodes (one
 * degraded, one silent) and an in-memory log buffer per node that grows by a
 * record on every follow request, like a live server.
 */
import { http, HttpResponse } from 'msw';
import type { components } from '../api/schema';
import type { ApiError, SystemStatus } from '../api/types';
import { summarize, type LogLevel, type LogPage, type LogRecord, type Subsystem, type SystemNode, type SystemNodeList } from '../pages/system/model';

const api = (path: string) => `*/api/v1${path}`;
type Guard = (request: Request, mutating?: boolean) => Response | null;

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });

const CAPACITY = 5000;
const LEVEL_RANK: Record<LogLevel, number> = { debug: 0, info: 1, warn: 2, error: 3 };

interface NodeLogs {
  boot: string;
  records: LogRecord[];
  next: number;
}

interface SubExtra {
  latency?: number;
  metric?: string;
  target?: string;
  /** How long the subsystem has had its status, in ms. */
  sinceMs?: number;
  incident?: Subsystem['last_incident'];
}

function sub(name: string, label: string, status: Subsystem['status'], detail: string, facts: [string, string][] = [], extra: SubExtra = {}): Subsystem {
  const now = Date.now();
  return {
    name,
    label,
    status,
    detail,
    checked_at: new Date(now - 4_000).toISOString(),
    latency_ms: extra.latency,
    metric: extra.metric,
    target: extra.target,
    since: new Date(now - (extra.sinceMs ?? 86_400_000)).toISOString(),
    last_incident: extra.incident,
    facts: facts.map(([l, value]) => ({ label: l, value })),
  };
}

const HOUR = 3_600_000;

function subsystems(degraded: boolean, uptimeMs: number): Subsystem[] {
  const iso = (agoMs: number) => new Date(Date.now() - agoMs).toISOString();
  const since = uptimeMs;
  return [
    sub('database', 'Database', 'healthy', 'PostgreSQL 17.2 · 6 of 40 connections in use', [
      ['Server', 'PostgreSQL 17.2'],
      ['Connections in use', '6 of 40'],
      ['Idle connections', '4'],
      ['Waited for a connection', '0 times, 0s in total'],
    ], { latency: 2, metric: '2 ms', target: 'db.hangar.example.com:5432/hangar', sinceMs: since }),
    sub('blobstores', 'Blob storage', 'healthy', '2 of 2 reachable', [
      ['default (fs)', 'Reachable; 412.6 GiB free of 1.8 TiB (22%)'],
      ['s3-archive (s3)', 'Reachable (38 ms)'],
    ], { metric: '22% free', sinceMs: since }),
    sub('eventbus', 'Event bus', 'healthy', 'JetStream · external cluster · 1 stream, 4 consumers', [
      ['Mode', 'External cluster'],
      ['Server', 'nats://nats-1.hangar.example.com:4222'],
      ['Reconnects', '0'],
      ['Streams', '1'],
      ['Consumers', '4'],
      ['Stored', '1.2 MiB'],
    ], { latency: 1, metric: '1 ms', target: 'nats://nats-1.hangar.example.com:4222', sinceMs: since }),
    sub('scheduler', 'Task scheduler', 'healthy', 'Another node is the leader; this one runs tasks · 0 active, 0 failed in 24 h', [
      ['Role since', iso(since)],
      ['Active runs', '0'],
      ['Failed runs (24 h)', '0'],
      ['Overdue tasks', '0'],
    ], { metric: 'follower', sinceMs: since }),
    sub('search', 'Search index', 'healthy', 'Index valid · about 18342 components', [
      ['Components (estimate)', '18342'],
      ['Probe query', '3ms'],
    ], { latency: 3, metric: '3 ms', target: 'components_document_idx', sinceMs: since }),
    sub('auth', 'Sign-in (OIDC)', 'healthy', 'OIDC enabled · worked at last use', [['OIDC', 'Working']], { metric: 'ok', sinceMs: since }),
    degraded
      ? sub('upstreams', 'Upstream proxies', 'degraded', '2 of 3 healthy · 1 offline', [
          ['Healthy', '2'],
          ['Slow', '0'],
          ['Blocked', '0'],
          ['Offline', '1'],
          ['Offline remotes', 'maven-central'],
        ], { metric: '180 ms', sinceMs: 25 * 60_000 })
      : sub('upstreams', 'Upstream proxies', 'healthy', '3 of 3 healthy', [
          ['Healthy', '3'],
          ['Slow', '0'],
          ['Blocked', '0'],
          ['Offline', '0'],
        ], {
          metric: '140 ms',
          sinceMs: 3 * HOUR,
          incident: { status: 'degraded', detail: '2 of 3 healthy · 1 slow', started_at: iso(5 * HOUR), ended_at: iso(3 * HOUR) },
        }),
  ];
}

function initialNodes(): SystemNode[] {
  const now = Date.now();
  const iso = (ms: number) => new Date(ms).toISOString();
  const leader = subsystems(false, 1_209_600_000).map((s) =>
    s.name === 'scheduler' ? { ...s, detail: 'This node is the leader · 0 active, 0 failed in 24 h', metric: 'leader' } : s,
  );
  return [
    {
      id: 'hangar-1', hostname: 'hangar-1', url: 'http://10.0.1.11:8080', version: '1.0.0', commit: '237ab95c1d2e',
      started_at: iso(now - 1_209_600_000), uptime_seconds: 1_209_600, last_heartbeat: iso(now - 2_000),
      role: 'leader', status: 'healthy', stale: false, current: true, subsystems: leader,
    },
    {
      id: 'hangar-2', hostname: 'hangar-2', url: 'http://10.0.2.11:8080', version: '1.0.0', commit: '237ab95c1d2e',
      started_at: iso(now - 86_400_000), uptime_seconds: 86_400, last_heartbeat: iso(now - 9_000),
      role: 'follower', status: 'degraded', stale: false, current: false, subsystems: subsystems(true, 86_400_000),
    },
    {
      id: 'hangar-3', hostname: 'hangar-3', url: 'http://10.0.3.11:8080', version: '0.9.4', commit: 'a91f00b7e6c3',
      started_at: iso(now - 7_200_000), uptime_seconds: 5_400, last_heartbeat: iso(now - 1_800_000),
      role: 'follower', status: 'down', stale: true, current: false, subsystems: subsystems(false, 5_400_000),
    },
  ];
}

const MESSAGES: [LogLevel, string, Record<string, unknown>][] = [
  ['info', 'http request', { method: 'GET', path: '/repository/nuget-group/v3/index.json', route: '/repository/{repo}/*', status: 200, duration_ms: 4, principal: 'ci-runner', repository: 'nuget-group' }],
  ['info', 'http request', { method: 'PUT', path: '/repository/nuget-hosted/', route: '/repository/{repo}/*', status: 201, duration_ms: 182, principal: 'release-bot', repository: 'nuget-hosted' }],
  ['debug', 'proxy cache hit', { repository: 'nuget-proxy', path: 'newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg' }],
  ['warn', 'upstream slow', { repository: 'nuget-proxy', remote: 'https://api.nuget.org/v3/index.json', latency_ms: 2140 }],
  ['info', 'task run finished', { task: 'blob-compaction', run_id: 'r_0192', freed_bytes: 104857600 }],
  ['error', 'upstream request failed', { repository: 'maven-central', error: 'dial tcp proxy.example.com:3128: connect: connection refused', authorization: 'REDACTED' }],
  ['info', 'auth.oidc: provider discovered', { issuer: 'https://sso.hangar.example.com/realms/main' }],
  ['warn', 'login failed', { username: 'm.nazarova', remote_ip: '10.4.2.18', reason: 'bad password' }],
];

function makeRecord(logs: NodeLogs, at: number, i: number): LogRecord {
  const [level, msg, attrs] = MESSAGES[i % MESSAGES.length]!;
  const seq = logs.next++;
  return { seq, time: new Date(at).toISOString(), level, msg, attrs: { ...attrs, request_id: `req-${seq.toString(16).padStart(6, '0')}` } };
}

function initialLogs(node: string, count: number): NodeLogs {
  const logs: NodeLogs = { boot: `${node.replace(/\W/g, '')}b00t`, records: [], next: 1 };
  const start = Date.now() - count * 1_500;
  for (let i = 0; i < count; i++) logs.records.push(makeRecord(logs, start + i * 1_500, i));
  return logs;
}

function initialState() {
  return {
    nodes: initialNodes(),
    logs: { 'hangar-1': initialLogs('hangar-1', 240), 'hangar-2': initialLogs('hangar-2', 60), 'hangar-3': initialLogs('hangar-3', 12) } as Record<string, NodeLogs>,
    /** Records added to every node per follow request. */
    growth: 1,
    nodesRequests: 0,
  };
}

let state = initialState();

export function resetSystemMockState(): void {
  state = initialState();
}

/** Test helpers. */
export const systemMockState = {
  get nodesRequests() {
    return state.nodesRequests;
  },
  /** Appends a record to a node's buffer. */
  log(node: string, level: LogLevel, msg: string, attrs: Record<string, unknown> = {}) {
    const logs = state.logs[node]!;
    logs.records.push({ seq: logs.next++, time: new Date().toISOString(), level, msg, attrs });
  },
  set growth(n: number) {
    state.growth = n;
  },
  /** Keeps only the answering node (a single-node installation). */
  singleNode() {
    state.nodes = state.nodes.filter((n) => n.current);
  },
};

function page(node: string, logs: NodeLogs, p: URLSearchParams): LogPage | Response {
  const level = p.get('level');
  if (level && !(level in LEVEL_RANK)) return error(400, { code: 'invalid_argument', message: `level "${level}": want debug, info, warn or error` });
  const limit = Number(p.get('limit') ?? 500);
  if (!Number.isInteger(limit) || limit < 1 || limit > 5000) return error(400, { code: 'invalid_argument', message: 'limit: want 1..5000' });
  const q = (p.get('q') ?? '').toLowerCase();
  const min = level ? LEVEL_RANK[level as LogLevel] : 0;
  const match = (r: LogRecord) => LEVEL_RANK[r.level] >= min && (!q || `${r.msg} ${JSON.stringify(r.attrs ?? {})}`.toLowerCase().includes(q));
  const held = logs.records.slice(-CAPACITY);
  const head = held.at(-1)?.seq ?? 0;
  const buffer = { size: held.length, capacity: CAPACITY, oldest_seq: held[0]?.seq ?? 0 };
  const cursorParam = p.get('cursor');
  if (cursorParam === null || cursorParam === '') {
    const items = held.filter(match).slice(-limit);
    return { node, items, cursor: `${logs.boot}.${head}`, more: false, gap: false, buffer };
  }
  const [boot, seqText] = cursorParam.includes('.') ? cursorParam.split('.') : ['', cursorParam];
  const restarted = boot !== '' && boot !== logs.boot;
  const after = restarted ? 0 : Number(seqText);
  const items: LogRecord[] = [];
  let cursor = head;
  let more = false;
  for (const r of held) {
    if (r.seq <= after || !match(r)) continue;
    if (items.length === limit) {
      more = true;
      cursor = items.at(-1)!.seq;
      break;
    }
    items.push(r);
  }
  return { node, items, cursor: `${logs.boot}.${cursor}`, more, gap: restarted, buffer };
}

type SystemSummary = components['schemas']['SystemSummary'];
type SystemCheckState = components['schemas']['SystemCheckState'];

const LEVEL: Record<SystemCheckState['status'], number> = { healthy: 0, degraded: 1, down: 2 };
const STATE: Record<SystemSummary['status'], SystemStatus['state']> = { healthy: 'ok', degraded: 'degraded', down: 'down' };

/** The installation's state rolled up from the mock nodes, the way the server does it. */
export function mockSummary(nodes: SystemNode[]): SystemSummary {
  const sum = summarize(nodes);
  const multi = nodes.length > 1;
  const problems: SystemCheckState[] = [
    ...nodes
      .filter((n) => !n.stale)
      .flatMap((n) =>
        n.subsystems
          .filter((x) => x.status !== 'healthy')
          .map((x) => ({ node: multi ? n.id : undefined, name: x.name, label: x.label, status: x.status, detail: x.detail, since: x.since })),
      ),
    ...nodes
      .filter((n) => n.stale)
      .map((n) => ({ node: n.id, name: 'node', label: `Node ${n.id}`, status: 'degraded' as const, detail: `No heartbeat since ${n.last_heartbeat}` })),
  ].sort((a, b) => LEVEL[b.status] - LEVEL[a.status]);
  return { status: sum.status, headline: sum.headline, nodes_up: sum.up, nodes_total: sum.total, checks_total: sum.checks, checks_passing: sum.passing, problems };
}

/** GET /system/status: the static fields of `base`, the state of the mock nodes. */
export function mockSystemStatus<T extends SystemStatus>(base: T): T {
  const s = mockSummary(state.nodes);
  return { ...base, state: STATE[s.status], message: s.headline, nodes_up: s.nodes_up, nodes_total: s.nodes_total, checks_total: s.checks_total, checks_passing: s.checks_passing, problems: s.problems };
}

export function systemHandlers(guard: Guard) {
  return [
    http.get(api('/system/nodes'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      state.nodesRequests++;
      const now = Date.now();
      const items = state.nodes.map((n) => (n.stale ? n : { ...n, last_heartbeat: new Date(now - (n.current ? 1_000 : 6_000)).toISOString() }));
      const body: SystemNodeList = { current: 'hangar-1', generated_at: new Date(now).toISOString(), items, partial: false, summary: mockSummary(items) };
      return HttpResponse.json(body);
    }),

    http.get(api('/system/health'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      return HttpResponse.json(state.nodes[0]);
    }),

    http.get(api('/system/logs'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = new URL(request.url).searchParams;
      const node = p.get('node') || 'hangar-1';
      const logs = state.logs[node];
      if (!logs) return error(404, { code: 'not_found', message: `node "${node}" is not running or does not answer: not found` });
      if (node === 'hangar-3') return error(404, { code: 'not_found', message: 'node "hangar-3" is not running or does not answer: not found' });
      if (p.get('cursor') && p.get('cursor') !== '0') {
        for (let i = 0; i < state.growth; i++) logs.records.push(makeRecord(logs, Date.now(), logs.next));
      }
      const out = page(node, logs, p);
      return out instanceof Response ? out : HttpResponse.json(out);
    }),
  ];
}
