/**
 * Pure helpers of the Vulnerabilities screen: URL filters, severity and
 * status wording, waiver validation and the trend summary.
 */
import type { FindingQuery } from '../api/findings';
import type { Finding, FindingTrendPoint, Severity } from '../api/types';
import type { Tone } from './format';

export const SEVERITIES = ['critical', 'high', 'medium', 'low', 'unknown'] as const satisfies readonly Severity[];

export const SEVERITY_META: Record<Severity, { label: string; tone: Tone; color: string }> = {
  critical: { label: 'Critical', tone: 'bad', color: 'var(--bad)' },
  high: { label: 'High', tone: 'peach', color: 'var(--chart-2)' },
  medium: { label: 'Medium', tone: 'warn', color: 'var(--warn)' },
  low: { label: 'Low', tone: 'neutral', color: 'var(--chart-1)' },
  unknown: { label: 'Unknown', tone: 'neutral', color: 'var(--ink-3)' },
};

export const isSeverity = (v: unknown): v is Severity => typeof v === 'string' && (SEVERITIES as readonly string[]).includes(v);

/** Status filter of the list: "active" (the default) is everything not fixed. */
export const STATUS_FILTERS = [
  { id: 'active', label: 'Not fixed', api: undefined },
  { id: 'open', label: 'Open', api: 'open,assigned' },
  { id: 'waived', label: 'Waived', api: 'waived' },
  { id: 'fixed', label: 'Fixed', api: 'fixed' },
] as const;
export type StatusFilter = (typeof STATUS_FILTERS)[number]['id'];

export const FIX_FILTERS = [
  { id: 'any', label: 'Any' },
  { id: 'yes', label: 'Fix available' },
  { id: 'no', label: 'No fix yet' },
] as const;
export type FixFilter = (typeof FIX_FILTERS)[number]['id'];

/** Filters of /vulnerabilities, all in the query string. */
export interface VulnSearch {
  severity?: Severity;
  status?: Exclude<StatusFilter, 'active'>;
  fix?: Exclude<FixFilter, 'any'>;
  repo?: string;
  q?: string;
  cursor?: string;
  /** The finding open in the drawer. */
  finding?: string;
}

const str = (v: unknown) => (typeof v === 'string' && v.trim().length > 0 ? v.trim() : undefined);

export function validateVulnSearch(s: Record<string, unknown>): VulnSearch {
  const status = STATUS_FILTERS.find((f) => f.id !== 'active' && f.id === s.status)?.id as VulnSearch['status'];
  const fix = s.fix === 'yes' || s.fix === 'no' ? s.fix : undefined;
  return {
    severity: isSeverity(s.severity) ? s.severity : undefined,
    status,
    fix,
    repo: str(s.repo),
    q: str(s.q),
    cursor: str(s.cursor),
    finding: str(s.finding),
  };
}

export const PAGE_SIZE = 50;

/** The API query for the list filters. */
export function findingQuery(s: VulnSearch): FindingQuery {
  return {
    severity: s.severity,
    status: STATUS_FILTERS.find((f) => f.id === (s.status ?? 'active'))?.api,
    fix_available: s.fix === 'yes' ? true : s.fix === 'no' ? false : undefined,
    repository: s.repo,
    q: s.q,
    cursor: s.cursor,
    limit: PAGE_SIZE,
  };
}

export const hasFilters = (s: VulnSearch) => !!(s.severity || s.status || s.fix || s.repo || s.q);

/** Package and version as the advisory names them: `Newtonsoft.Json@12.0.1`. */
export const packageAt = (f: Pick<Finding, 'package' | 'installed'>) => (f.installed ? `${f.package}@${f.installed}` : f.package);

const dayFmt = new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' });

/** "4 Dec 2026" (UTC). */
export const shortDate = (iso: string) => dayFmt.format(new Date(iso));

/** The status of a finding in words, with the waiver expiry when it matters. */
export function findingStatus(f: Pick<Finding, 'status' | 'waiver_until' | 'quarantined'>, now: Date = new Date()): { label: string; tone: Tone } {
  switch (f.status) {
    case 'waived':
      return { label: f.waiver_until ? `Waived until ${shortDate(f.waiver_until)}` : 'Waived', tone: 'info' };
    case 'fixed':
      return { label: 'Fixed', tone: 'ok' };
    case 'assigned':
      return { label: 'Assigned', tone: 'warn' };
    default:
      if (f.quarantined) return { label: 'Open · quarantined', tone: 'bad' };
      if (f.waiver_until && new Date(f.waiver_until) <= now) return { label: `Open · waiver expired ${shortDate(f.waiver_until)}`, tone: 'warn' };
      return { label: 'Open', tone: 'warn' };
  }
}

/** CVE ids first, then the other aliases. */
export function sortedAliases(aliases: readonly string[] | undefined): string[] {
  return [...(aliases ?? [])].sort((a, b) => Number(!a.startsWith('CVE-')) - Number(!b.startsWith('CVE-')) || a.localeCompare(b));
}

/** Where an advisory id can be read: OSV for every id. */
export function advisoryUrl(id: string): string {
  return `https://osv.dev/vulnerability/${encodeURIComponent(id)}`;
}

/** yyyy-mm-dd of a date in local time, for <input type="date">. */
export function dateInputValue(d: Date): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

export const MAX_WAIVER_DAYS = 365;

/** The waiver expiry for a date input: the end of that day in UTC. */
export function waiverUntil(dateValue: string): Date | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(dateValue)) return null;
  const d = new Date(`${dateValue}T23:59:59Z`);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** Checks a waiver as the server does; returns the problem per field. */
export function validateWaiver(reason: string, dateValue: string, now: Date = new Date()): { reason?: string; until?: string } {
  const errors: { reason?: string; until?: string } = {};
  const r = reason.trim();
  if (r.length < 3) errors.reason = 'Say why this risk is accepted (at least 3 characters).';
  else if (r.length > 2000) errors.reason = 'Keep the reason under 2000 characters.';
  const until = waiverUntil(dateValue);
  if (!until) errors.until = 'Pick the date the waiver ends.';
  else if (until <= now) errors.until = 'The waiver must end in the future.';
  else if (until.getTime() - now.getTime() > (MAX_WAIVER_DAYS + 1) * 86_400_000) errors.until = 'A waiver can last at most a year.';
  return errors;
}

/** One sentence describing the trend, for screen readers and the chart caption. */
export function trendSummary(points: readonly FindingTrendPoint[]): string {
  if (points.length === 0) return 'No finding history yet.';
  const total = (p: FindingTrendPoint) => p.critical + p.high + p.medium + p.low;
  const first = points[0]!;
  const last = points[points.length - 1]!;
  const added = points.reduce((n, p) => n + p.new, 0);
  const fixed = points.reduce((n, p) => n + p.resolved, 0);
  const now = total(last);
  const then = total(first);
  const change = now === then ? 'unchanged' : now > then ? `up from ${then}` : `down from ${then}`;
  return `${now} unresolved findings today (${last.critical} critical), ${change} ${points.length} days ago; ${added} detected and ${fixed} fixed in that time.`;
}

/** Short day label for the chart axis: "4 Nov". */
export function trendDayLabel(date: string): string {
  return new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short', timeZone: 'UTC' }).format(new Date(`${date}T00:00:00Z`));
}
