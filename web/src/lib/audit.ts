import { buildUrl } from '../api/client';
import type { AuditOutcome, ListAuditQuery } from '../api/opsTypes';
import type { Tone } from './format';

export type AuditRange = '1h' | '24h' | '7d' | '30d' | '90d' | 'custom';

export const AUDIT_RANGES: { id: AuditRange; label: string; ms?: number }[] = [
  { id: '1h', label: 'Last hour', ms: 3_600_000 },
  { id: '24h', label: 'Last 24 hours', ms: 86_400_000 },
  { id: '7d', label: 'Last 7 days', ms: 7 * 86_400_000 },
  { id: '30d', label: 'Last 30 days', ms: 30 * 86_400_000 },
  { id: '90d', label: 'Last 90 days', ms: 90 * 86_400_000 },
  { id: 'custom', label: 'Custom range' },
];

export const DEFAULT_AUDIT_RANGE: AuditRange = '7d';

export const AUDIT_OUTCOMES: { id: AuditOutcome; label: string; tone: Tone }[] = [
  { id: 'success', label: 'Success', tone: 'ok' },
  { id: 'denied', label: 'Denied', tone: 'warn' },
  { id: 'error', label: 'Error', tone: 'bad' },
];

export function outcomeMeta(o: string): { label: string; tone: Tone } {
  return AUDIT_OUTCOMES.find((x) => x.id === o) ?? { label: o, tone: 'neutral' };
}

export const isAuditRange = (v: unknown): v is AuditRange => AUDIT_RANGES.some((r) => r.id === v);
export const isAuditOutcome = (v: unknown): v is AuditOutcome => AUDIT_OUTCOMES.some((r) => r.id === v);

/** Common action prefixes offered as suggestions; any action or `prefix.*` is accepted. */
export const ACTION_SUGGESTIONS = [
  'auth.*',
  'auth.login',
  'auth.login.failed',
  'repository.*',
  'component.*',
  'component.delete',
  'cleanup.*',
  'task.*',
  'token.*',
  'user.*',
  'role.*',
  'audit.export',
];

/** Filters as they live in the URL. `from`/`to` (ISO) are used only with range=custom. */
export interface AuditSearch {
  actor?: string;
  action?: string;
  target?: string;
  outcome?: AuditOutcome;
  range?: AuditRange;
  from?: string;
  to?: string;
  cursor?: string;
}

const isoOrUndefined = (v: unknown): string | undefined => {
  if (typeof v !== 'string' || !v) return undefined;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
};

const str = (v: unknown) => (typeof v === 'string' && v.trim() ? v.trim() : undefined);

export function validateAuditSearch(s: Record<string, unknown>): AuditSearch {
  return {
    actor: str(s.actor),
    action: str(s.action),
    target: str(s.target),
    outcome: isAuditOutcome(s.outcome) ? s.outcome : undefined,
    range: isAuditRange(s.range) ? s.range : undefined,
    from: isoOrUndefined(s.from),
    to: isoOrUndefined(s.to),
    cursor: str(s.cursor),
  };
}

/** The time window a search stands for, resolved against `now`. */
export function resolveWindow(s: AuditSearch, now: Date): { from?: string; to?: string } {
  const range = s.range ?? DEFAULT_AUDIT_RANGE;
  if (range === 'custom') return { from: s.from, to: s.to };
  const ms = AUDIT_RANGES.find((r) => r.id === range)!.ms!;
  return { from: new Date(now.getTime() - ms).toISOString() };
}

/** API query for GET /audit (and the export) from the URL state. */
export function auditQuery(s: AuditSearch, now: Date): ListAuditQuery {
  const w = resolveWindow(s, now);
  return {
    actor: s.actor,
    action: s.action,
    target: s.target,
    outcome: s.outcome,
    from: w.from,
    to: w.to,
    cursor: s.cursor,
  };
}

export type ExportFormat = 'csv' | 'json';

/** A plain link to the streamed export: the browser downloads it with the session cookie. */
export function auditExportUrl(s: AuditSearch, format: ExportFormat, now: Date): string {
  const { cursor: _cursor, ...q } = auditQuery(s, now);
  void _cursor;
  return buildUrl('/audit/export', { format, ...q });
}

/** "2026-11-04T09:30" (datetime-local, read as UTC) → ISO; empty → undefined. */
export function utcInputToIso(v: string): string | undefined {
  if (!v) return undefined;
  const d = new Date(`${v}:00Z`);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

/** ISO → "2026-11-04T09:30" for a datetime-local input showing UTC. */
export function isoToUtcInput(iso: string | undefined): string {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toISOString().slice(0, 16);
}

/** Flattens event details for display: nested keys become dotted paths, arrays are joined. */
export function flattenDetails(details: Record<string, unknown> | undefined, prefix = ''): [string, string][] {
  const out: [string, string][] = [];
  for (const [k, v] of Object.entries(details ?? {})) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (v && typeof v === 'object' && !Array.isArray(v)) out.push(...flattenDetails(v as Record<string, unknown>, key));
    else if (Array.isArray(v)) out.push([key, v.map((x) => (typeof x === 'object' ? JSON.stringify(x) : String(x))).join(', ')]);
    else out.push([key, v === null || v === undefined ? '—' : String(v)]);
  }
  return out;
}

export function hasFilters(s: AuditSearch): boolean {
  return !!(s.actor || s.action || s.target || s.outcome || (s.range && s.range !== DEFAULT_AUDIT_RANGE));
}
