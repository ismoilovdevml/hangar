/**
 * Pure helpers of the Vulnerabilities screen: URL filters, severity and
 * status wording, the fix plan wording and waiver validation.
 */
import { ApiRequestError } from '../api/client';
import type { FixPlanQuery } from '../api/findings';
import type { Finding, PackagePlan, Severity, UpgradeLine } from '../api/types';
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

/** Tabs of the fix plan; "in_use" (the default) is not written to the URL. */
export const PLAN_TABS = [
  { id: 'in_use', label: 'In use first' },
  { id: 'all', label: 'All packages' },
  { id: 'waived', label: 'Waived' },
] as const;
export type PlanTab = (typeof PLAN_TABS)[number]['id'];

/** Filters of /vulnerabilities, all in the query string. */
export interface VulnSearch {
  tab?: Exclude<PlanTab, 'in_use'>;
  /** Only findings assigned to the signed-in user. */
  mine?: boolean;
  repo?: string;
  q?: string;
  /** The package rows expanded into their findings (package keys). */
  open?: string[];
  /** The finding open in the drawer. */
  finding?: string;
}

const str = (v: unknown) => (typeof v === 'string' && v.trim().length > 0 ? v.trim() : undefined);

export function validateVulnSearch(s: Record<string, unknown>): VulnSearch {
  const tab = s.tab === 'all' || s.tab === 'waived' ? s.tab : undefined;
  const open = (Array.isArray(s.open) ? s.open : typeof s.open === 'string' ? [s.open] : [])
    .map(str)
    .filter((v): v is string => !!v)
    .slice(0, 50);
  return {
    tab,
    mine: s.mine === true || s.mine === 'true' || s.mine === 1 ? true : undefined,
    repo: str(s.repo),
    q: str(s.q),
    open: open.length ? open : undefined,
    finding: str(s.finding),
  };
}

/** The pull window of the screen. */
export const PULL_WINDOW = '7d';

/** The API query for the plan filters. */
export function planQuery(s: VulnSearch): FixPlanQuery {
  return { tab: s.tab ?? 'in_use', window: PULL_WINDOW, assignee: s.mine ? 'me' : undefined, repository: s.repo, q: s.q };
}

export const hasFilters = (s: VulnSearch) => !!(s.mine || s.repo || s.q);

/** "8.0.4 / 10.0.10": the distinct upgrade targets; null when none is known. */
export function upgradeTargets(p: Pick<PackagePlan, 'upgrades'>): string | null {
  const to = [...new Set(p.upgrades.map((u) => u.to).filter((v): v is string => !!v))];
  return to.length ? to.join(' / ') : null;
}

/** The upgrade commands of a package, one per line (for the clipboard). */
export function upgradeCommands(p: Pick<PackagePlan, 'upgrades'>): string {
  return [...new Set(p.upgrades.map((u) => u.command).filter((v): v is string => !!v))].join('\n');
}

/** The upgrade line that covers one vulnerable version. */
export function lineFor(p: Pick<PackagePlan, 'upgrades'>, version: string): UpgradeLine | undefined {
  return p.upgrades.find((u) => u.from.includes(version));
}

/** "GHSA-5crp-9r3c-p9vr · CVE-2024-21907", or "CVE-2018-8292 + 3 more". */
export function advisoryIds(p: Pick<PackagePlan, 'advisories'>): string {
  const first = p.advisories[0];
  if (!first) return '';
  const cve = first.aliases.find((a) => a.startsWith('CVE-'));
  const ids = first.id === cve || !cve ? first.id : `${first.id} · ${cve}`;
  const more = p.advisories.length - 1;
  return more > 0 ? `${ids} + ${more} more` : ids;
}

/** "2 high · 8.1": the findings count at the highest severity, and the highest score. */
export function findingsSummary(p: Pick<PackagePlan, 'findings' | 'max_severity' | 'max_cvss' | 'severities'>): string {
  const n = p.severities[p.max_severity as keyof typeof p.severities] ?? p.findings;
  const others = p.findings - n;
  const head = `${n} ${SEVERITY_META[p.max_severity].label.toLowerCase()}${others > 0 ? ` + ${others}` : ''}`;
  return p.max_cvss != null ? `${head} · ${p.max_cvss.toFixed(1)}` : head;
}

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

/** The message of a failed request, for an inline error. */
export const errorText = (err: unknown) => (err instanceof ApiRequestError ? err.message : 'Something went wrong. Try again.');
