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

/** "2026-11-04T09:30" (datetime-local, the browser's time zone) → ISO; empty → undefined. */
export function localInputToIso(v: string): string | undefined {
  if (!v) return undefined;
  // A date-time without an offset is local time.
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

/** ISO → "2026-11-04T09:30" for a datetime-local input in the browser's time zone. */
export function isoToLocalInput(iso: string | undefined): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const p = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
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

/* ---------------------------------------------------------------- reading events */

export interface AuditCategory {
  id: string;
  label: string;
  action: string;
}

/** Access-related audit events, one action prefix each (the API filters by one prefix). */
export const AUDIT_CATEGORIES: AuditCategory[] = [
  { id: 'users', label: 'Users', action: 'user.*' },
  { id: 'signins', label: 'Sign-ins', action: 'auth.*' },
  { id: 'tokens', label: 'Tokens', action: 'token.*' },
  { id: 'roles', label: 'Roles', action: 'role.*' },
  { id: 'sessions', label: 'Sessions', action: 'session.*' },
];

const AUDIT_VERBS: Record<string, string> = {
  'auth.login': 'signed in',
  'auth.login.failed': 'failed to sign in',
  'auth.logout': 'signed out',
  'auth.token.failed': 'used an invalid token',
  'user.create': 'created',
  'user.update': 'updated',
  'user.delete': 'deleted',
  'user.password': 'changed the password of',
  'user.unlock': 'unlocked',
  'user.disable': 'disabled',
  'user.enable': 'enabled',
  'user.bootstrap': 'bootstrapped',
  'user.list': 'listed users',
  'user.read': 'viewed',
  'user.permissions': 'viewed the permissions of',
  'token.create': 'created',
  'token.revoke': 'revoked',
  'token.issue': 'issued',
  'token.list': 'listed tokens of',
  'role.create': 'created',
  'role.update': 'updated',
  'role.delete': 'deleted',
  'role.bind': 'granted',
  'role.unbind': 'took away',
  'role.list': 'listed roles',
  'role.read': 'viewed',
  'session.revoke': 'ended a session of',
  'session.revoke_all': 'ended the sessions of',
  'session.list': 'listed the sessions of',
};

/** Whether describeAudit has words for `action` (access events); other actions read best as they are. */
export const isDescribedAction = (action: string) => Object.hasOwn(AUDIT_VERBS, action);

/** "created token:…", "granted role:Reader", in plain words. */
export function describeAudit(action: string, target: string, details?: Record<string, unknown>): string {
  const verb = AUDIT_VERBS[action] ?? action;
  const [kind, ...rest] = target.split(':');
  const name = rest.join(':');
  let what = target;
  if (kind === 'user') what = name;
  else if (kind === 'role') what = `role ${name}`;
  else if (kind === 'token') what = typeof details?.name === 'string' ? `token “${details.name}”` : 'a token';
  else if (kind === 'system') what = `${name} settings`;
  if (action === 'role.bind' || action === 'role.unbind') {
    const user = typeof details?.user === 'string' ? details.user : 'a user';
    return action === 'role.bind' ? `granted ${what} to ${user}` : `took ${what} away from ${user}`;
  }
  if (action.startsWith('auth.login') || action === 'auth.logout') return verb;
  return `${verb} ${what}`.trim();
}
