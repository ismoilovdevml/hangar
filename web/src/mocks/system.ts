/**
 * Mock API for the System status and System logs screens: three nodes (one
 * degraded, one silent) and an in-memory log buffer per node that grows by a
 * record on every follow request, like a live server.
 */
import { http, HttpResponse } from 'msw';
import type { ApiError } from '../api/types';
import type { LogLevel, LogPage, LogRecord, Subsystem, SystemNode, SystemNodeList } from '../pages/system/model';

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

function sub(name: string, label: string, status: Subsystem['status'], detail: string, facts: [string, string][] = [], latency?: number): Subsystem {
  return {
    name,
    label,
    status,
    detail,
    checked_at: new Date(Date.now() - 4_000).toISOString(),
    latency_ms: latency,
    facts: facts.map(([l, value]) => ({ label: l, value })),
  };
}

function subsystems(degraded: boolean): Subsystem[] {
  return [
    sub('database', 'PostgreSQL', 'healthy', 'Ping 2 ms; 6 of 40 connections in use', [
      ['Server', 'PostgreSQL 17.2'],
      ['Connections in use', '6 of 40'],
      ['Idle connections', '4'],
      ['Waited for a connection', '0 times, 0s in total'],
    ], 2),
    sub('blobstores', 'Blob stores', 'healthy', '2 of 2 reachable', [
      ['default (fs)', 'Reachable; 412.6 GiB free of 1.8 TiB (22%)'],
      ['s3-archive (s3)', 'Reachable (38 ms)'],
    ]),
    sub('eventbus', 'NATS / JetStream', 'healthy', 'Connected; JetStream holds 1 stream', [
      ['Mode', 'External cluster'],
      ['Server', 'nats://nats-1.hangar.example.com:4222'],
      ['Reconnects', '0'],
      ['Streams', '1'],
      ['Consumers', '4'],
      ['Stored', '1.2 MiB'],
    ], 1),
    sub('scheduler', 'Task scheduler', 'healthy', 'Follower: another node schedules tasks; this one executes them', [
      ['Active runs', '0'],
      ['Failed runs (24 h)', '0'],
      ['Overdue tasks', '0'],
    ]),
    sub('search', 'Search index', 'healthy', 'Index valid; searches answer normally', [
      ['Components (estimate)', '18342'],
      ['Probe query', '3ms'],
    ], 3),
    sub('auth', 'External sign-in', 'healthy', 'OIDC enabled', [['OIDC', 'Working']]),
    degraded
      ? sub('upstreams', 'Upstream proxies', 'degraded', '3 proxy repositories: 2 healthy, 1 offline', [
          ['Healthy', '2'],
          ['Slow', '0'],
          ['Blocked', '0'],
          ['Offline', '1'],
          ['Offline remotes', 'maven-central'],
        ])
      : sub('upstreams', 'Upstream proxies', 'healthy', '3 proxy repositories: 3 healthy', [['Healthy', '3']]),
  ];
}

function initialNodes(): SystemNode[] {
  const now = Date.now();
  const iso = (ms: number) => new Date(ms).toISOString();
  const leader = subsystems(false).map((s) =>
    s.name === 'scheduler' ? { ...s, detail: 'Leader: this node schedules tasks' } : s,
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
      role: 'follower', status: 'degraded', stale: false, current: false, subsystems: subsystems(true),
    },
    {
      id: 'hangar-3', hostname: 'hangar-3', url: 'http://10.0.3.11:8080', version: '0.9.4', commit: 'a91f00b7e6c3',
      started_at: iso(now - 7_200_000), uptime_seconds: 5_400, last_heartbeat: iso(now - 1_800_000),
      role: 'follower', status: 'down', stale: true, current: false, subsystems: subsystems(false),
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

export function systemHandlers(guard: Guard) {
  return [
    http.get(api('/system/nodes'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      state.nodesRequests++;
      const now = Date.now();
      const items = state.nodes.map((n) => (n.stale ? n : { ...n, last_heartbeat: new Date(now - (n.current ? 1_000 : 6_000)).toISOString() }));
      const body: SystemNodeList = { current: 'hangar-1', generated_at: new Date(now).toISOString(), items, partial: false };
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
