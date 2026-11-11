import type { Task } from '../api/opsTypes';
import type { components } from '../api/schema';
import { DOCS } from './docs';
import type { Format, OverviewMetrics, OverviewRange, ScanSummary, TrafficBucket, UpstreamHealth } from '../api/types';

type Token = components['schemas']['Token'];
import type { Tone } from './format';
import { upstreamAuthProblem } from './repositories';

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

/**
 * Why a component is not scanned, as a short sentence-case label: Docker
 * images say "Image scanning not configured" rather than looking clean.
 */
export function unsupportedText(reason: string | undefined, format?: string): string {
  if (reason) return reason.charAt(0).toUpperCase() + reason.slice(1);
  return format === 'docker' ? 'Image scanning not configured' : 'No advisory source';
}

/** Security pill text and tone for a component's scan result. */
export function scanPill(scan: ScanSummary): { text: string; tone: Tone } {
  switch (scan.state) {
    case 'pending':
      return { text: 'Not scanned yet', tone: 'neutral' };
    case 'failed':
      return { text: 'Scan failed', tone: 'warn' };
    case 'unsupported':
      return { text: unsupportedText(scan.scan_error), tone: 'neutral' };
    case 'quarantined':
      return { text: 'Quarantined', tone: 'bad' };
    case 'clean':
      return { text: 'No issues', tone: 'ok' };
    case 'findings':
      if (scan.critical) return { text: `${scan.critical} critical`, tone: 'bad' };
      if (scan.high) return { text: `${scan.high} high`, tone: 'warn' };
      if (scan.medium) return { text: `${scan.medium} medium`, tone: 'warn' };
      if (scan.low) return { text: `${scan.low} low`, tone: 'neutral' };
      return { text: 'No issues', tone: 'ok' };
  }
}

export function bucketLabel(iso: string, range: OverviewRange): string {
  const d = new Date(iso);
  if (range === '1h' || range === 'today') {
    return new Intl.DateTimeFormat('en-GB', { hour: '2-digit', minute: '2-digit', hour12: false }).format(d);
  }
  if (range === '7d') return new Intl.DateTimeFormat('en-GB', { weekday: 'short' }).format(d);
  return new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short' }).format(d);
}

/** Five evenly spaced axis labels ending in "Now". */
export function axisLabels(buckets: TrafficBucket[], range: OverviewRange): string[] {
  const n = buckets.length;
  if (n === 0) return [];
  const picks = [0, Math.round(n / 4), Math.round(n / 2), Math.round((3 * n) / 4)].filter(
    (i, k, a) => i < n - 1 && a.indexOf(i) === k,
  );
  return [...picks.map((i) => bucketLabel(buckets[i]!.start, range)), 'Now'];
}

/** Start of the window the server counts requests over for `range` (UTC midnight for "today"). */
export function rangeStart(range: OverviewRange, generatedAt: string): Date {
  const now = new Date(generatedAt).getTime();
  const DAY = 86_400_000;
  switch (range) {
    case '1h':
      return new Date(now - 3_600_000);
    case 'today':
      return new Date(now - (now % DAY));
    case '7d':
      return new Date(now - 7 * DAY);
    case '30d':
      return new Date(now - 30 * DAY);
  }
}

/**
 * When the request counters started later than the range (the server restarted),
 * says from when the numbers count; null when they cover the whole range.
 */
export function coverageNote(m: OverviewMetrics, now: Date = new Date()): string | null {
  const from = m.requests.coverage_start;
  if (!from) return null;
  const start = new Date(from);
  if (Number.isNaN(start.getTime())) return null;
  if (start.getTime() - rangeStart(m.range, m.generated_at).getTime() < 60_000) return null;
  const sameDay = start.toDateString() === now.toDateString();
  const fmt = new Intl.DateTimeFormat('en-GB', sameDay
    ? { hour: '2-digit', minute: '2-digit', hour12: false }
    : { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit', hour12: false });
  return `Counted since ${fmt.format(start)}`;
}

/** Package coordinates in the format's idiom, from namespace + name. */
export function componentName(c: { format: string; namespace?: string; name: string }): string {
  const ns = c.namespace?.trim();
  if (!ns || c.name.startsWith(ns)) return c.name;
  return c.format === 'maven' ? `${ns}:${c.name}` : `${ns}/${c.name}`;
}

/* ---------------------------------------------------------------- overview cards */

/**
 * Share of requests answered without a server error or an auth refusal; null without requests or outcome counts.
 * Authentication challenges (401s to requests without credentials, which clients retry with them) count on neither side.
 */
export function successRatio(r: OverviewMetrics['requests']): number | null {
  if (r.total === null || r.server_errors === null || r.denied === null) return null;
  const counted = r.total - (r.auth_challenges ?? 0);
  if (counted <= 0) return null;
  return Math.max(0, counted - r.server_errors - r.denied) / counted;
}

/** "98.6" for a ratio, without a misleading "100.0" while any request failed. */
export function ratioPercent(ratio: number): string {
  const v = Math.floor(ratio * 1000) / 10;
  return v.toFixed(1);
}

/** The host of a URL, or the URL itself when it does not parse. */
export function urlHost(url: string): string {
  try {
    return new URL(url).host || url;
  } catch {
    return url;
  }
}

const UPSTREAM_RANK: Record<UpstreamHealth['state'], number> = { offline: 0, blocked: 1, slow: 2, unknown: 3, healthy: 4 };

/** The upstream to show first: the worst state, then the busiest. */
export function headlineUpstream(items: UpstreamHealth[]): UpstreamHealth | null {
  return (
    [...items].sort((a, b) => UPSTREAM_RANK[a.state] - UPSTREAM_RANK[b.state] || (b.requests ?? 0) - (a.requests ?? 0))[0] ?? null
  );
}

const RANGE_WORDS: Record<OverviewRange, string> = { '1h': 'in the last hour', today: 'today', '7d': 'in 7 days', '30d': 'in 30 days' };

export interface OverallStatus {
  tone: 'ok' | 'warn' | 'bad';
  text: string;
}

/**
 * The Overview's one-line state, from the same signals as its cards and
 * to-do list: node health, unreachable upstreams and server errors in the
 * range. It never reads "All systems normal" while any of them is red.
 */
export function overallStatus(input: {
  system: { state: 'ok' | 'degraded' | 'down'; message: string };
  upstreams?: UpstreamHealth[];
  metrics?: OverviewMetrics;
}): OverallStatus {
  const { system, upstreams = [], metrics } = input;
  if (system.state === 'down') return { tone: 'bad', text: system.message };
  const unreachable = upstreams.filter((u) => u.state === 'offline' || u.state === 'blocked');
  if (unreachable.length === 1) {
    const u = unreachable[0]!;
    // Worded like the to-do item: a refused credential is named as such.
    const auth = u.state === 'blocked' ? upstreamAuthProblem(u.reason) : null;
    return { tone: 'bad', text: auth ? `${u.name} upstream: ${auth.label}` : `${u.name} upstream is ${u.state}` };
  }
  if (unreachable.length > 1) return { tone: 'bad', text: `${unreachable.length} upstreams unreachable` };
  if (system.state === 'degraded') return { tone: 'warn', text: system.message };
  const errors = metrics?.requests.server_errors ?? 0;
  if (errors > 0) return { tone: 'warn', text: `${plural(errors, 'server error')} ${RANGE_WORDS[metrics!.range]}` };
  return { tone: 'ok', text: system.message };
}

/** Where a to-do item leads. */
export type TodoLink =
  | { kind: 'account-tokens' }
  | { kind: 'access-tokens' }
  | { kind: 'audit-pushes' }
  | { kind: 'vulnerabilities' }
  | { kind: 'system-status' }
  | { kind: 'tasks' }
  | { kind: 'task'; id: string }
  | { kind: 'blob-stores' }
  | { kind: 'repository'; name: string }
  /** Documentation outside the app (opens in a new tab). */
  | { kind: 'docs'; href: string };

export interface TodoItem {
  id: string;
  /** bad = act now, warn = recommended, info = setup. */
  tone: 'bad' | 'warn' | 'info';
  tag: string;
  title: string;
  detail: string;
  link: TodoLink;
  /** A reminder the user may dismiss for good: nothing in the app can tell when it is done. */
  dismissible?: boolean;
}

export interface TodoInput {
  metrics: OverviewMetrics;
  upstreams?: UpstreamHealth[];
  system?: { state: 'ok' | 'degraded' | 'down'; message: string; nodes_up: number; nodes_total: number };
  tokenWarnings?: Token[];
  /** The signed-in username: their own tokens are renewed on My account. */
  username?: string;
  /** Scheduled tasks, when the user may read them. */
  tasks?: Task[];
  canAdminBlobStores?: boolean;
  now?: Date;
}

const TONE_ORDER = { bad: 0, warn: 1, info: 2 } as const;

function expiresIn(iso: string, now: Date): string {
  const ms = new Date(iso).getTime() - now.getTime();
  if (ms <= 0) return 'Expired';
  const h = Math.floor(ms / 3_600_000);
  if (h < 1) return 'Expires in under an hour';
  if (h < 48) return `Expires in ${h} h`;
  return `Expires in ${Math.floor(h / 24)} days`;
}

/** What needs someone's attention, built only from what the server reports; most urgent first. */
export function todoItems(input: TodoInput): TodoItem[] {
  const { metrics: m, now = new Date() } = input;
  const out: TodoItem[] = [];

  const sys = input.system;
  if (sys && sys.state !== 'ok') {
    out.push({
      id: 'system',
      tone: sys.state === 'down' ? 'bad' : 'warn',
      tag: sys.state === 'down' ? 'OUTAGE' : 'DEGRADED',
      title: sys.message,
      detail: `${sys.nodes_up} of ${sys.nodes_total} ${sys.nodes_total === 1 ? 'node' : 'nodes'} up`,
      link: { kind: 'system-status' },
    });
  }

  for (const u of input.upstreams ?? []) {
    if (u.state !== 'offline' && u.state !== 'blocked') continue;
    const auth = u.state === 'blocked' ? upstreamAuthProblem(u.reason) : null;
    out.push({
      id: `upstream-${u.name}`,
      tone: 'bad',
      tag: 'UPSTREAM',
      title: auth ? `${u.name}: ${auth.label}` : `${u.name} is ${u.state}`,
      detail: auth
        ? `${urlHost(u.url)} refuses the credentials: ${auth.hint}; uncached packages fail`
        : `${urlHost(u.url)} cannot be reached; uncached packages fail`,
      link: { kind: 'repository', name: u.name },
    });
  }

  const rejected = m.activity.pushes_rejected;
  if (rejected > 0) {
    out.push({
      id: 'pushes',
      tone: 'bad',
      tag: 'PUSHES FAILING',
      title: `${plural(rejected, 'push', 'pushes')} rejected`,
      detail: 'Failed or denied publish attempts in this period',
      link: { kind: 'audit-pushes' },
    });
  }

  if (m.issues.critical > 0) {
    out.push({
      id: 'critical',
      tone: 'bad',
      tag: 'SECURITY',
      title: `Review ${plural(m.issues.critical, 'critical vulnerability', 'critical vulnerabilities')}`,
      detail: `In ${plural(m.issues.packages_affected, 'package')} · ${m.issues.quarantined} quarantined`,
      link: { kind: 'vulnerabilities' },
    });
  }

  for (const t of input.tokenWarnings ?? []) {
    if (!t.expires_at) continue;
    const own = t.owner === input.username;
    out.push({
      id: `token-${t.id}`,
      tone: 'warn',
      tag: 'EXPIRES SOON',
      title: `Renew the ${t.name} token`,
      detail: `${expiresIn(t.expires_at, now)} · ${own ? 'your token' : t.owner}`,
      link: own ? { kind: 'account-tokens' } : { kind: 'access-tokens' },
    });
  }

  if (input.tasks) {
    const backups = input.tasks.filter((t) => t.type === 'db_backup');
    const scheduled = backups.filter((t) => t.enabled && t.schedule);
    const failed = backups.find((t) => t.last_run?.status === 'failed');
    if (backups.length === 0) {
      // The server has no backup task to schedule: backups run outside it (deploy/scripts/backup.sh),
      // so the reminder points at how, and can be dismissed once they are set up.
      out.push({
        id: 'backup-outside',
        tone: 'info',
        tag: 'RECOMMENDED',
        title: 'Back up the database',
        detail: 'Hangar does not back itself up: schedule the backup script',
        link: { kind: 'docs', href: DOCS.backups },
        dismissible: true,
      });
    } else if (failed) {
      out.push({
        id: 'backup-failed',
        tone: 'bad',
        tag: 'BACKUP FAILED',
        title: `${failed.name} failed`,
        detail: failed.last_run?.message || 'The last database backup did not finish',
        link: { kind: 'task', id: failed.id },
      });
    } else if (scheduled.length === 0) {
      out.push({
        id: 'backup',
        tone: 'warn',
        tag: 'RECOMMENDED',
        title: 'Schedule a database backup',
        detail: backups.some((t) => t.last_run) ? 'No backup is scheduled' : 'No backup has ever run',
        link: { kind: 'tasks' },
      });
    }
  }

  if (m.issues.scanning && !m.issues.scanning.enabled) {
    out.push({
      id: 'scanning',
      tone: 'info',
      tag: 'SETUP',
      title: 'Turn on vulnerability scanning',
      detail: 'Packages are not checked for known vulnerabilities',
      link: { kind: 'vulnerabilities' },
    });
  }

  if (input.canAdminBlobStores && (m.storage.capacity_bytes === null || m.storage.capacity_bytes <= 0)) {
    out.push({
      id: 'quota',
      tone: 'info',
      tag: 'SETUP',
      title: 'Set a storage quota',
      detail: 'Get warned before the disk fills',
      link: { kind: 'blob-stores' },
    });
  }

  return out.sort((a, b) => TONE_ORDER[a.tone] - TONE_ORDER[b.tone]);
}

/** The storage card's footnote: which formats fill it and whether a quota applies. */
export function storageNote(m: OverviewMetrics, label: (f: Format) => string): string {
  const rows = m.storage_by_format.filter((f) => f.bytes > 0);
  const total = rows.reduce((n, f) => n + f.bytes, 0);
  const cap = m.storage.capacity_bytes;
  const quota = cap === null || cap <= 0 ? 'no quota' : `${Math.round((m.storage.used_bytes / cap) * 100)}% of quota`;
  if (rows.length === 0 || total <= 0) return `Nothing stored yet · ${quota}`;
  if (rows.length === 1) return `All ${label(rows[0]!.format)} · ${quota}`;
  const top = rows[0]!;
  return `${label(top.format)} ${Math.round((top.bytes / total) * 100)}% · ${rows.length} formats · ${quota}`;
}
