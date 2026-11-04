/**
 * Types and pure helpers for the System status and System logs screens
 * (GET /system/nodes, /system/health and /system/logs in api/openapi.yaml).
 */
import type { components } from '../../api/schema';
import type { Tone } from '../../lib/format';

type Schemas = components['schemas'];

export type HealthStatus = Schemas['SystemStatusLevel'];
export type Subsystem = Schemas['SystemSubsystem'];
export type SystemNode = Schemas['SystemNode'];
export type SystemNodeList = Schemas['SystemNodeList'];
export type LogRecord = Schemas['SystemLogRecord'];
export type LogPage = Schemas['SystemLogPage'];
export type LogLevel = LogRecord['level'];

export const HEALTH: Record<HealthStatus, { label: string; tone: Extract<Tone, 'ok' | 'warn' | 'bad'> }> = {
  healthy: { label: 'Healthy', tone: 'ok' },
  degraded: { label: 'Degraded', tone: 'warn' },
  down: { label: 'Down', tone: 'bad' },
};

const RANK: Record<HealthStatus, number> = { healthy: 0, degraded: 1, down: 2 };

/** The worst of the given states (healthy when there are none). */
export function worst(states: HealthStatus[]): HealthStatus {
  return states.reduce<HealthStatus>((w, s) => (RANK[s] > RANK[w] ? s : w), 'healthy');
}

export interface ClusterSummary {
  status: HealthStatus;
  up: number;
  total: number;
  leader?: SystemNode;
  versions: string[];
  /** One sentence for the summary panel. */
  message: string;
}

/** Overall state of the installation from its nodes. A node is up while it publishes heartbeats. */
export function summarize(nodes: SystemNode[]): ClusterSummary {
  const up = nodes.filter((n) => !n.stale).length;
  const leader = nodes.find((n) => n.role === 'leader' && !n.stale);
  const versions = [...new Set(nodes.filter((n) => !n.stale).map((n) => n.version))].sort();
  const status = worst(nodes.map((n) => n.status));
  let message = 'All nodes and subsystems are healthy.';
  if (nodes.length === 0) message = 'No node has reported yet.';
  else if (up < nodes.length) message = `${nodes.length - up} of ${nodes.length} nodes are not responding.`;
  else if (status === 'down') message = 'A subsystem is down; see the details below.';
  else if (status === 'degraded') message = 'Something needs attention; see the subsystems marked degraded.';
  if (up > 0 && !leader) message += ' No node holds the scheduler lock, so scheduled tasks do not start.';
  return { status, up, total: nodes.length, leader, versions, message };
}

/** "12 s ago", "4 min ago", "3 h ago" from an ISO time. */
export function ago(iso: string, now: Date): string {
  const s = Math.max(0, Math.round((now.getTime() - new Date(iso).getTime()) / 1000));
  if (Number.isNaN(s)) return iso;
  if (s < 60) return `${s} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86_400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86_400)} d ago`;
}

/* ---------------------------------------------------------------- logs */

export const LOG_LEVELS: { value: '' | LogLevel; label: string }[] = [
  { value: '', label: 'All levels' },
  { value: 'debug', label: 'Debug and above' },
  { value: 'info', label: 'Info and above' },
  { value: 'warn', label: 'Warnings and errors' },
  { value: 'error', label: 'Errors only' },
];

export const LEVEL_TONE: Record<LogLevel, Tone> = { debug: 'neutral', info: 'info', warn: 'warn', error: 'bad' };

export function isLogLevel(v: unknown): v is LogLevel {
  return v === 'debug' || v === 'info' || v === 'warn' || v === 'error';
}

/** URL state of the System logs screen. */
export interface SystemLogsSearch {
  node?: string;
  level?: LogLevel;
  q?: string;
}

export function validateLogsSearch(s: Record<string, unknown>): SystemLogsSearch {
  const str = (v: unknown) => (typeof v === 'string' && v.length > 0 ? v.slice(0, 200) : undefined);
  return { node: str(s.node), level: isLogLevel(s.level) ? s.level : undefined, q: str(s.q) };
}

/** Most records the screen keeps; older ones scroll away. */
export const MAX_CLIENT_RECORDS = 5000;

/** Appends newer records, keeping at most `max` (the newest). */
export function appendRecords(prev: LogRecord[], next: LogRecord[], max = MAX_CLIENT_RECORDS): LogRecord[] {
  if (next.length === 0) return prev;
  const all = prev.concat(next);
  return all.length > max ? all.slice(all.length - max) : all;
}

/** "13:48:39.509" in UTC. */
export function clock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().slice(11, 23);
}

/** Attributes shown inline after the message, in this order first. */
const INLINE_FIRST = ['error', 'status', 'method', 'path', 'repository', 'principal', 'duration_ms'];
const INLINE_SKIP = new Set(['request_id', 'trace_id', 'span_id', 'user_agent', 'bytes_in', 'bytes_out', 'route', 'remote_ip']);

function scalar(v: unknown): string {
  if (typeof v === 'string') return /\s/.test(v) ? JSON.stringify(v) : v;
  if (v === null || typeof v !== 'object') return String(v);
  return JSON.stringify(v);
}

/** Up to `max` key=value pairs for the collapsed line. */
export function inlineAttrs(attrs: LogRecord['attrs'], max = 4): string {
  if (!attrs) return '';
  const keys = Object.keys(attrs).filter((k) => !INLINE_SKIP.has(k));
  keys.sort((a, b) => {
    const ia = INLINE_FIRST.indexOf(a);
    const ib = INLINE_FIRST.indexOf(b);
    return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib) || a.localeCompare(b);
  });
  return keys
    .slice(0, max)
    .map((k) => `${k}=${scalar(attrs[k])}`)
    .join(' ');
}

/** The record as pretty JSON, for the expanded detail. */
export function recordJson(r: LogRecord): string {
  return JSON.stringify({ time: r.time, level: r.level, msg: r.msg, ...(r.attrs ?? {}) }, null, 2);
}

/** Records as JSON lines, one object per line (the download format). */
export function toJsonLines(records: LogRecord[]): string {
  return records.map((r) => JSON.stringify({ time: r.time, level: r.level, msg: r.msg, ...(r.attrs ?? {}) })).join('\n') + (records.length ? '\n' : '');
}

/* ---------------------------------------------------------------- virtual list */

export const ROW_HEIGHT = 32;
export const DETAIL_LINE_HEIGHT = 18;
/** Padding and border around an expanded record's JSON. */
export const DETAIL_CHROME = 28;
/** Expanded details taller than this scroll inside. */
export const DETAIL_MAX_LINES = 18;

/** Height of one row, collapsed or with its JSON detail open. */
export function rowHeight(r: LogRecord, expanded: boolean): number {
  if (!expanded) return ROW_HEIGHT;
  const lines = Math.min(recordJson(r).split('\n').length, DETAIL_MAX_LINES);
  return ROW_HEIGHT + DETAIL_CHROME + lines * DETAIL_LINE_HEIGHT;
}

/** Top offset of every row plus the total height (last element). */
export function offsets(heights: number[]): number[] {
  const out = new Array<number>(heights.length + 1);
  out[0] = 0;
  for (let i = 0; i < heights.length; i++) out[i + 1] = out[i]! + heights[i]!;
  return out;
}

/** Index range [start, end) of the rows visible in a viewport, plus overscan. */
export function visibleRange(tops: number[], scrollTop: number, viewport: number, overscan = 8): [number, number] {
  const n = tops.length - 1;
  if (n <= 0) return [0, 0];
  let lo = 0;
  let hi = n - 1;
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (tops[mid]! <= scrollTop) lo = mid;
    else hi = mid - 1;
  }
  let end = lo;
  while (end < n && tops[end]! < scrollTop + viewport) end++;
  return [Math.max(0, lo - overscan), Math.min(n, end + overscan)];
}
