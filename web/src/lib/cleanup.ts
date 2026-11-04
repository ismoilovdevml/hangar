import type { CleanupCriteria, CleanupPolicy, CleanupPolicyInput } from '../api/opsTypes';
import type { Format } from '../api/types';
import { cronToSchedule, scheduleToCron, type ScheduleDraft } from './cron';
import { formatBytes } from './format';

/** Same rule as repository names; the name is part of the policy's URL. */
export const POLICY_NAME_RE = /^[a-z0-9][a-z0-9-]{1,62}$/;

const MAX_DAYS = 36_500;
const MAX_KEEP = 100_000;

/** A number condition the user can switch on and off without losing the value. */
export interface Toggle {
  on: boolean;
  value: string;
}

/** Editor state: everything is a string while typing; converted on save. */
export interface PolicyDraft {
  name: string;
  description: string;
  format: Format | '';
  olderThan: Toggle;
  notDownloaded: Toggle;
  keepNewest: Toggle;
  keepMatching: string;
  prereleaseOnly: boolean;
  repositories: string[];
  schedule: ScheduleDraft;
  enabled: boolean;
}

export function emptyDraft(): PolicyDraft {
  return {
    name: '',
    description: '',
    format: '',
    olderThan: { on: true, value: '90' },
    notDownloaded: { on: true, value: '60' },
    keepNewest: { on: true, value: '10' },
    keepMatching: '',
    prereleaseOnly: false,
    repositories: [],
    schedule: cronToSchedule('0 3 * * *'),
    enabled: true,
  };
}

const toggle = (v: number | undefined, fallback: string): Toggle =>
  v === undefined || v === null ? { on: false, value: fallback } : { on: true, value: String(v) };

export function draftFromPolicy(p: CleanupPolicy): PolicyDraft {
  const c = p.criteria ?? {};
  return {
    name: p.name,
    description: p.description ?? '',
    format: p.format ?? '',
    olderThan: toggle(c.older_than_days, '90'),
    notDownloaded: toggle(c.not_downloaded_days, '60'),
    keepNewest: toggle(c.keep_newest, '10'),
    keepMatching: (c.keep_matching ?? []).join(', '),
    prereleaseOnly: !!c.prerelease_only,
    repositories: [...(p.repositories ?? [])],
    schedule: cronToSchedule(p.schedule),
    enabled: p.enabled ?? true,
  };
}

/** "release-*, latest\nv*.*.*" → ["release-*", "latest", "v*.*.*"], de-duplicated, order kept. */
export function parseGlobs(text: string): string[] {
  const out: string[] = [];
  for (const g of text.split(/[\s,]+/)) {
    if (g && !out.includes(g)) out.push(g);
  }
  return out;
}

export type DraftErrors = Partial<Record<'name' | 'olderThan' | 'notDownloaded' | 'keepNewest' | 'criteria' | 'repositories' | 'schedule', string>>;

function intIn(text: string, min: number, max: number): number | null {
  if (!/^\d+$/.test(text.trim())) return null;
  const n = Number(text.trim());
  return n >= min && n <= max ? n : null;
}

export function validateDraft(d: PolicyDraft): DraftErrors {
  const e: DraftErrors = {};
  if (!POLICY_NAME_RE.test(d.name)) {
    e.name = d.name ? 'Use 2–63 lowercase letters, digits and dashes, starting with a letter or digit' : 'Enter a name';
  }
  if (d.olderThan.on && intIn(d.olderThan.value, 1, MAX_DAYS) === null) e.olderThan = `Enter whole days, 1–${MAX_DAYS}`;
  if (d.notDownloaded.on && intIn(d.notDownloaded.value, 1, MAX_DAYS) === null) e.notDownloaded = `Enter whole days, 1–${MAX_DAYS}`;
  if (d.keepNewest.on && intIn(d.keepNewest.value, 0, MAX_KEEP) === null) e.keepNewest = `Enter a count, 0–${MAX_KEEP}`;
  // Without any of these the policy would select every version of every package.
  if (!d.olderThan.on && !d.notDownloaded.on && !d.prereleaseOnly && !d.keepNewest.on) {
    e.criteria = 'Choose an age, last-download, pre-release or keep-newest condition; otherwise every version is selected';
  }
  if (d.repositories.length === 0) e.repositories = 'Attach at least one repository';
  if (scheduleToCron(d.schedule) === undefined) e.schedule = 'This schedule is not valid';
  return e;
}

export function criteriaFromDraft(d: PolicyDraft): CleanupCriteria {
  const c: CleanupCriteria = {};
  if (d.olderThan.on) c.older_than_days = Number(d.olderThan.value);
  if (d.notDownloaded.on) c.not_downloaded_days = Number(d.notDownloaded.value);
  if (d.keepNewest.on) c.keep_newest = Number(d.keepNewest.value);
  const globs = parseGlobs(d.keepMatching);
  if (globs.length) c.keep_matching = globs;
  if (d.prereleaseOnly) c.prerelease_only = true;
  return c;
}

/** The request body for preview, create and update. Call only on a valid draft. */
export function draftToInput(d: PolicyDraft, previewToken?: string): CleanupPolicyInput {
  const input: CleanupPolicyInput = {
    name: d.name,
    description: d.description.trim() || undefined,
    format: d.format || null,
    criteria: criteriaFromDraft(d),
    repositories: [...d.repositories].sort(),
    schedule: scheduleToCron(d.schedule) ?? null,
    enabled: d.enabled,
  };
  if (previewToken) input.preview_token = previewToken;
  return input;
}

/**
 * What a preview covers. A save needs a preview of exactly this, so any change
 * here makes the last preview stale; name, description, schedule and the
 * enabled switch do not change what gets deleted.
 */
export function previewKey(d: PolicyDraft): string {
  return JSON.stringify({ c: criteriaFromDraft(d), r: [...d.repositories].sort(), f: d.format || null });
}

/** One line for the policy list: "older than 90 days · unused 60 days · keep newest 20". */
export function summarizeCriteria(c: CleanupCriteria | undefined): string {
  if (!c) return 'No conditions';
  const parts: string[] = [];
  if (c.prerelease_only) parts.push('pre-releases only');
  if (c.older_than_days) parts.push(`older than ${c.older_than_days} days`);
  if (c.not_downloaded_days) parts.push(`unused ${c.not_downloaded_days} days`);
  if (c.keep_newest !== undefined && c.keep_newest !== null) parts.push(`keep newest ${c.keep_newest}`);
  if (c.keep_matching?.length) parts.push(`keep ${c.keep_matching.join(', ')}`);
  return parts.length ? parts.join(' · ') : 'No conditions';
}

export function previewHeadline(count: number, bytes: number): string {
  const n = new Intl.NumberFormat('en-US').format(count);
  if (count === 0) return 'Nothing would be deleted';
  return `${n} ${count === 1 ? 'version' : 'versions'} · ${formatBytes(bytes)} would be freed`;
}

export function policyFilename(name: string, now: Date): string {
  const stamp = now.toISOString().slice(0, 16).replace(/[-:]/g, '').replace('T', '-');
  return `cleanup-preview-${name || 'draft'}-${stamp}.csv`;
}
