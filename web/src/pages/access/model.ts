/**
 * Access model for the UI: privileges, token scopes and expiry, user state.
 * Pure functions only, so they are unit-tested without rendering.
 */
import { ApiRequestError } from '../../api/client';
import type { Tone } from '../../lib/format';
import type { Member, Token } from './api';

/* ---------------------------------------------------------------- privileges */

export type Action = 'read' | 'write' | 'delete' | 'admin';
export type Domain = 'repo' | 'system';

export const ACTIONS: { id: Action; label: string; verb: string; hint: string }[] = [
  { id: 'read', label: 'Read', verb: 'read', hint: 'Download and browse packages' },
  { id: 'write', label: 'Write', verb: 'publish to', hint: 'Publish new packages and versions' },
  { id: 'delete', label: 'Delete', verb: 'delete from', hint: 'Delete packages and versions' },
  { id: 'admin', label: 'Admin', verb: 'administer', hint: 'Everything above, plus repository settings' },
];

export const SYSTEM_AREAS: { id: string; label: string; hint: string }[] = [
  { id: 'users', label: 'Users & roles', hint: 'Members, service accounts, roles, sessions' },
  { id: 'tokens', label: 'Tokens', hint: "Other people's and service accounts' tokens" },
  { id: 'repos', label: 'Repositories', hint: 'Create and delete repositories' },
  { id: 'blobstores', label: 'Blob stores', hint: 'Storage configuration' },
  { id: 'tasks', label: 'Tasks', hint: 'Scheduled tasks, run and stop' },
  { id: 'cleanup', label: 'Cleanup', hint: 'Cleanup policies' },
  { id: 'security', label: 'Security', hint: 'Findings, waivers, scan policy' },
  { id: 'audit', label: 'Audit log', hint: 'Read and export the audit log' },
  { id: 'cluster', label: 'Cluster', hint: 'Nodes and replication' },
  { id: 'settings', label: 'Settings', hint: 'Global settings' },
];

export interface Privilege {
  domain: Domain;
  pattern: string;
  action: Action | '*';
}

const ACTION_IDS = new Set<string>(['read', 'write', 'delete', 'admin', '*']);

/** Parses "repo:npm-*:write", "system:users:admin" or "system:*"; null if malformed. */
export function parsePrivilege(s: string): Privilege | null {
  const parts = s.split(':');
  if (parts.length === 2 && parts[0] === 'system' && parts[1] === '*') return { domain: 'system', pattern: '*', action: '*' };
  if (parts.length !== 3) return null;
  const [domain, pattern, action] = parts as [string, string, string];
  if ((domain !== 'repo' && domain !== 'system') || !pattern || !ACTION_IDS.has(action)) return null;
  return { domain, pattern, action: action as Action | '*' };
}

export function formatPrivilege(p: Privilege): string {
  if (p.domain === 'system' && p.pattern === '*' && p.action === '*') return 'system:*';
  return `${p.domain}:${p.pattern}:${p.action}`;
}

/** Validates a repository pattern the way the server does. */
export function repoPatternError(pattern: string): string | null {
  if (!pattern) return 'Enter a repository name or pattern.';
  if (pattern.length > 64) return 'At most 64 characters.';
  if (!/^[a-z0-9*?[\]^!-]+$/.test(pattern)) return 'Use lower-case letters, digits, dashes and the wildcards * and ?.';
  if ((pattern.match(/\[/g) ?? []).length !== (pattern.match(/]/g) ?? []).length) return 'Unbalanced [ ] in the pattern.';
  return null;
}

/** Reads a privilege as a sentence: "Publish to repositories matching npm-*". */
export function describePrivilege(s: string): string {
  const p = parsePrivilege(s);
  if (!p) return s;
  if (p.domain === 'system') {
    const area = p.pattern === '*' ? 'every system area' : (SYSTEM_AREAS.find((a) => a.id === p.pattern)?.label ?? p.pattern);
    const what = p.action === '*' || p.action === 'admin' ? 'Full control of' : `${capitalize(actionLabel(p.action))} access to`;
    return `${what} ${area}`;
  }
  const target =
    p.pattern === '*' ? 'every repository' : /[*?[]/.test(p.pattern) ? `repositories matching ${p.pattern}` : `repository ${p.pattern}`;
  const verb = p.action === '*' ? 'administer' : (ACTIONS.find((a) => a.id === p.action)?.verb ?? p.action);
  return `${capitalize(verb)} ${target}`;
}

function actionLabel(a: Action | '*'): string {
  return a === '*' ? 'full' : a;
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** Glob match with *, ? and [...] like Go's path.Match for names without "/". */
export function globMatch(pattern: string, name: string): boolean {
  let re = '^';
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i]!;
    if (c === '*') re += '[^/]*';
    else if (c === '?') re += '[^/]';
    else if (c === '[') {
      const end = pattern.indexOf(']', i + 1);
      if (end < 0) return false;
      let cls = pattern.slice(i + 1, end);
      if (cls.startsWith('^') || cls.startsWith('!')) cls = `^${cls.slice(1)}`;
      re += `[${cls.replace(/\\/g, '\\\\')}]`;
      i = end;
    } else re += c.replace(/[.+^${}()|\\]/g, '\\$&');
  }
  try {
    return new RegExp(`${re}$`).test(name);
  } catch {
    return false;
  }
}

/** Whether privilege strings grant action on a resource (admin and * imply everything). */
export function allows(privileges: readonly string[] | undefined, domain: Domain, name: string, action: Action): boolean {
  return (privileges ?? []).some((s) => {
    const p = parsePrivilege(s);
    if (!p || p.domain !== domain || !globMatch(p.pattern, name)) return false;
    return p.action === action || p.action === 'admin' || p.action === '*';
  });
}

/* ---------------------------------------------------------------- token scopes */

export type RepoScopeMode = 'none' | 'all' | 'pattern' | 'list';

export interface ScopeDraft {
  repoMode: RepoScopeMode;
  pattern: string;
  repos: string[];
  actions: Action[];
  /** System scopes as "area:action" (area may be "*"). */
  system: string[];
}

export const EMPTY_SCOPES: ScopeDraft = { repoMode: 'all', pattern: '', repos: [], actions: ['read'], system: [] };

/** Turns the scope builder's state into privileges, or explains what is missing. */
export function buildScopes(d: ScopeDraft): { scopes: string[]; error: string | null } {
  const scopes: string[] = [];
  if (d.repoMode !== 'none') {
    if (d.actions.length === 0) return { scopes: [], error: 'Pick at least one repository action.' };
    let targets: string[] = [];
    if (d.repoMode === 'all') targets = ['*'];
    else if (d.repoMode === 'pattern') {
      const err = repoPatternError(d.pattern.trim());
      if (err) return { scopes: [], error: err };
      targets = [d.pattern.trim()];
    } else {
      if (d.repos.length === 0) return { scopes: [], error: 'Pick at least one repository.' };
      targets = d.repos;
    }
    // admin covers the rest; keep the list short and readable.
    const actions = d.actions.includes('admin') ? (['admin'] as Action[]) : d.actions;
    for (const t of targets) for (const a of actions) scopes.push(`repo:${t}:${a}`);
  }
  for (const s of d.system) {
    const [area, action] = s.split(':') as [string, string];
    scopes.push(area === '*' && action === '*' ? 'system:*' : `system:${area}:${action}`);
  }
  if (scopes.length === 0) return { scopes, error: 'A token needs at least one scope.' };
  if (scopes.length > 100) return { scopes: [], error: 'At most 100 scopes; use a pattern instead of a long list.' };
  return { scopes: [...new Set(scopes)], error: null };
}

/** Scopes a token asks for that the user's own privileges do not cover (they would have no effect). */
export function uncoveredScopes(scopes: string[], privileges: readonly string[] | undefined): string[] {
  return scopes.filter((s) => {
    const p = parsePrivilege(s);
    if (!p) return true;
    const action: Action = p.action === '*' ? 'admin' : p.action;
    // A glob scope is covered when a privilege covers a sample name it matches.
    const sample = p.pattern === '*' ? '*' : p.pattern.replace(/[*?]/g, 'x').replace(/\[[^\]]*]/g, 'x');
    return !(privileges ?? []).some((h) => {
      const q = parsePrivilege(h);
      if (!q || q.domain !== p.domain) return false;
      const actionOk = q.action === action || q.action === '*' || q.action === 'admin';
      if (!actionOk) return false;
      return q.pattern === '*' || q.pattern === p.pattern || (p.pattern !== '*' && globMatch(q.pattern, sample));
    });
  });
}

/* ---------------------------------------------------------------- expiry */

const DAY = 86_400_000;

export const EXPIRY_PRESETS = [7, 30, 90, 180, 365] as const;

/** Presets that fit the server's maximum lifetime, always including the maximum itself. */
export function expiryPresets(maxDays: number): number[] {
  const out: number[] = EXPIRY_PRESETS.filter((d) => d <= maxDays);
  if (maxDays > 0 && !out.includes(maxDays) && maxDays < 3650) out.push(maxDays);
  return out;
}

/** End of the day `days` from now, in UTC, as an RFC 3339 string. */
export function expiresInDays(days: number, now: Date = new Date()): string {
  return new Date(now.getTime() + days * DAY).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** Expiry for a custom date (yyyy-mm-dd), at 23:59:59 UTC; error text if outside 1 day … maxDays. */
export function customExpiry(date: string, maxDays: number, now: Date = new Date()): { value: string | null; error: string | null } {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) return { value: null, error: 'Pick a date.' };
  const at = new Date(`${date}T23:59:59Z`);
  if (Number.isNaN(at.getTime())) return { value: null, error: 'Pick a valid date.' };
  if (at.getTime() <= now.getTime()) return { value: null, error: 'The date must be in the future.' };
  if (at.getTime() > now.getTime() + maxDays * DAY) return { value: null, error: `At most ${maxDays} days from today.` };
  return { value: at.toISOString().replace(/\.\d{3}Z$/, 'Z'), error: null };
}

/** Parses a Go duration string ("336h0m0s", "90s", "1h30m") into milliseconds; null if malformed. */
export function parseGoDuration(s: string | undefined): number | null {
  if (!s) return null;
  const re = /(\d+(?:\.\d+)?)(h|ms|m|s)/g;
  let total = 0;
  let consumed = 0;
  for (const m of s.matchAll(re)) {
    const n = Number(m[1]);
    total += n * ({ h: 3_600_000, m: 60_000, s: 1000, ms: 1 } as const)[m[2] as 'h' | 'm' | 's' | 'ms'];
    consumed += m[0].length;
  }
  return consumed === s.length && consumed > 0 ? total : null;
}

export type TokenState = 'active' | 'expiring' | 'expired';

export function tokenState(t: Pick<Token, 'expires_at'>, now: Date, warnMs = 14 * DAY): TokenState {
  const left = new Date(t.expires_at).getTime() - now.getTime();
  if (left <= 0) return 'expired';
  return left <= warnMs ? 'expiring' : 'active';
}

export const TOKEN_STATE_META: Record<TokenState, { label: string; tone: Tone }> = {
  active: { label: 'Active', tone: 'ok' },
  expiring: { label: 'Expiring soon', tone: 'warn' },
  expired: { label: 'Expired', tone: 'bad' },
};

/** "in 28 days", "in 5 h", "expired 3 days ago". */
export function expiryText(iso: string, now: Date = new Date()): string {
  const diff = new Date(iso).getTime() - now.getTime();
  const abs = Math.abs(diff);
  const h = Math.floor(abs / 3_600_000);
  const d = Math.floor(abs / DAY);
  const span = d >= 1 ? `${d} day${d === 1 ? '' : 's'}` : h >= 1 ? `${h} h` : `${Math.max(1, Math.floor(abs / 60_000))} min`;
  return diff > 0 ? `in ${span}` : `${span} ago`;
}

export function formatDate(iso: string): string {
  return new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' }).format(new Date(iso));
}

export function formatDateTime(iso: string): string {
  return new Intl.DateTimeFormat('en-GB', {
    day: 'numeric',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
    timeZone: 'UTC',
    timeZoneName: 'short',
  }).format(new Date(iso));
}

/* ---------------------------------------------------------------- users */

export type UserStatus = 'active' | 'disabled' | 'locked';

export function userStatus(m: Pick<Member, 'disabled' | 'locked_until'>, now: Date = new Date()): UserStatus {
  if (m.disabled) return 'disabled';
  if (m.locked_until && new Date(m.locked_until).getTime() > now.getTime()) return 'locked';
  return 'active';
}

export const USER_STATUS_META: Record<UserStatus, { label: string; tone: 'ok' | 'warn' | 'bad' | 'neutral' }> = {
  active: { label: 'Active', tone: 'ok' },
  disabled: { label: 'Disabled', tone: 'neutral' },
  locked: { label: 'Locked', tone: 'warn' },
};

export const SOURCE_LABEL: Record<string, string> = {
  local: 'Local',
  oidc: 'Single sign-on',
  ldap: 'Directory (LDAP)',
  saml: 'SAML',
};

const ROLE_TONE: Record<string, Tone> = { Admin: 'violet', Publisher: 'info', Reader: 'neutral', 'CI deployer': 'peach' };

export function roleTone(role: string): Tone {
  return ROLE_TONE[role] ?? 'sage';
}

export const USERNAME_RE = /^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$/;
export const ROLE_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$/;

/** Short description of a browser from its User-Agent: "Firefox on Linux". */
export function describeUserAgent(ua: string): string {
  if (!ua) return 'Unknown device';
  const browser = /Edg\//.test(ua)
    ? 'Edge'
    : /Firefox\//.test(ua)
      ? 'Firefox'
      : /Chrome\//.test(ua)
        ? 'Chrome'
        : /Safari\//.test(ua)
          ? 'Safari'
          : /curl\//.test(ua)
            ? 'curl'
            : null;
  const os = /Windows/.test(ua)
    ? 'Windows'
    : /iPhone|iPad/.test(ua)
      ? 'iOS'
      : /Mac OS X|Macintosh/.test(ua)
        ? 'macOS'
        : /Android/.test(ua)
          ? 'Android'
          : /Linux/.test(ua)
            ? 'Linux'
            : null;
  if (browser && os) return `${browser} on ${os}`;
  return browser ?? os ?? ua.slice(0, 40);
}

/* ---------------------------------------------------------------- audit */

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

/* ---------------------------------------------------------------- page state */

export interface MemberFilters {
  q?: string;
  role?: string;
  status?: UserStatus;
}

export type AccessTab = 'members' | 'service-accounts' | 'tokens' | 'roles' | 'audit';

export const ACCESS_TABS: AccessTab[] = ['members', 'service-accounts', 'tokens', 'roles', 'audit'];

export function isAccessTab(v: unknown): v is AccessTab {
  return typeof v === 'string' && (ACCESS_TABS as string[]).includes(v);
}

export interface AccessSearch extends MemberFilters {
  tab?: AccessTab;
  /** Member or service account whose drawer is open. */
  user?: string;
  /** Audit category id. */
  events?: string;
  /** Tokens tab filters. */
  owner_kind?: 'user' | 'service_account';
  token_status?: 'active' | 'expired';
}


/* ---------------------------------------------------------------- misc */

/** The message to show for a failed request. */
export function errorText(err: unknown): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return 'You do not have permission to do this.';
    if (err.status === 423 && err.retryAfter) return `Locked after repeated wrong passwords. Try again in ${Math.ceil(err.retryAfter / 60)} min.`;
    return err.body?.message ?? err.message;
  }
  return err instanceof Error ? err.message : 'Something went wrong.';
}

/** Generates a random passphrase-like password (for "Generate" in the password fields). */
export function generatePassword(): string {
  const alphabet = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789';
  const limit = 256 - (256 % alphabet.length); // rejection sampling keeps it uniform
  let out = '';
  while (out.length < 20) {
    const bytes = new Uint8Array(32);
    crypto.getRandomValues(bytes);
    for (const b of bytes) if (b < limit && out.length < 20) out += alphabet[b % alphabet.length];
  }
  return `${out.slice(0, 5)}-${out.slice(5, 10)}-${out.slice(10, 15)}-${out.slice(15)}`;
}
