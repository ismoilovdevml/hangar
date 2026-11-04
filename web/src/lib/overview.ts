import type { OverviewMetrics, OverviewRange, ScanSummary, TrafficBucket } from '../api/types';
import type { Tone } from './format';

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** The one-line summary under the greeting. */
export function summarySentence(m: OverviewMetrics): string {
  const { published_since_yesterday: pub, failed_pushes: failed } = m.summary;
  const lead = failed === 0 ? 'Everything is calm.' : 'A few things need a look.';
  const pushes = failed === 0 ? 'no failed pushes' : plural(failed, 'failed push', 'failed pushes');
  return `${lead} ${plural(pub, 'package')} published since yesterday, ${pushes}.`;
}

/** Security pill text and tone for a component's scan result. */
export function scanPill(scan: ScanSummary): { text: string; tone: Tone } {
  switch (scan.state) {
    case 'pending':
      return { text: 'Scanning', tone: 'neutral' };
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
