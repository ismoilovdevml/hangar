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
