/**
 * Access model for the UI: privileges, token scopes and expiry, user state.
 * Pure functions only, so they are unit-tested without rendering.
 */
import type { Tone } from '../../lib/format';
import type { Member, Token } from './api';
import { globMatch, parsePrivilege, type Action, type Privilege } from '../../lib/privileges';

/* ---------------------------------------------------------------- privileges */

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

/* ---------------------------------------------------------------- token scopes */

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

export const EXPIRY_PRESETS = [7, 30, 90, 365] as const;

/** Presets that fit the server's maximum lifetime, plus the maximum itself when it is shorter than a year. */
export function expiryPresets(maxDays: number): number[] {
  const out: number[] = EXPIRY_PRESETS.filter((d) => d <= maxDays);
  if (maxDays > 0 && maxDays < 365 && !out.includes(maxDays)) out.push(maxDays);
  return out;
}

/** The moment `days` from now, as an RFC 3339 string. */
export function expiresInDays(days: number, now: Date = new Date()): string {
  return new Date(now.getTime() + days * DAY).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** Expiry for a custom date (yyyy-mm-dd), at 23:59:59 in the browser's zone; error text if outside 1 day … maxDays. */
export function customExpiry(date: string, maxDays: number, now: Date = new Date()): { value: string | null; error: string | null } {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) return { value: null, error: 'Pick a date.' };
  // Without an offset the date-time is local: the end of the day the user picked, where they are.
  const at = new Date(`${date}T23:59:59`);
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

export { formatDate } from '../../lib/format';

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

/** Sessions from the same browser and address, shown as one device. */
export interface SessionGroup<S extends SessionWithDevice = SessionWithDevice> {
  key: string;
  device: string;
  ip: string | null;
  /** This session first, then newest activity first. */
  sessions: S[];
  current: boolean;
}

interface SessionWithDevice {
  id: string;
  ip: string | null;
  user_agent: string;
  last_seen_at: string;
  current: boolean;
}

/** Groups sessions by device and address: this session's device first, then the most recently active. */
export function groupSessions<S extends SessionWithDevice>(sessions: S[]): SessionGroup<S>[] {
  const groups = new Map<string, SessionGroup<S>>();
  for (const s of sessions) {
    const device = describeUserAgent(s.user_agent);
    const key = `${device}\u0000${s.ip ?? ''}`;
    const g = groups.get(key) ?? { key, device, ip: s.ip, sessions: [], current: false };
    g.sessions.push(s);
    g.current ||= s.current;
    groups.set(key, g);
  }
  const latest = (g: SessionGroup<S>) => Date.parse(g.sessions[0]!.last_seen_at);
  const out = [...groups.values()];
  for (const g of out) g.sessions.sort((a, b) => Number(b.current) - Number(a.current) || Date.parse(b.last_seen_at) - Date.parse(a.last_seen_at));
  return out.sort((a, b) => Number(b.current) - Number(a.current) || latest(b) - latest(a));
}


/* ---------------------------------------------------------------- page state */

export interface MemberFilters {
  q?: string;
  role?: string;
  status?: UserStatus;
}

export type AccessTab = 'members' | 'service-accounts' | 'tokens' | 'roles';

export const ACCESS_TABS: AccessTab[] = ['members', 'service-accounts', 'tokens', 'roles'];

export function isAccessTab(v: unknown): v is AccessTab {
  return typeof v === 'string' && (ACCESS_TABS as string[]).includes(v);
}

export interface AccessSearch extends MemberFilters {
  /** `audit` is the Audit tab of v0.1.x: the router sends it on to /audit. */
  tab?: AccessTab | 'audit';
  /** Member or service account whose drawer is open. */
  user?: string;
  /** With tab=audit: the old tab's event category, kept as the Audit log's action filter. */
  events?: string;
  /** Tokens tab filters. */
  owner_kind?: 'user' | 'service_account';
  token_status?: 'active' | 'expired';
}


/* ---------------------------------------------------------------- misc */

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

/** One repository in the effective-permissions report, as far as grouping needs. */
export interface RepoAccess {
  name: string;
  actions: string[];
  grants: { action: string; source: string; role?: string; privilege?: string }[];
}

export interface RepoAccessGroup<T extends RepoAccess> {
  /** Same actions, granted the same way. */
  actions: string[];
  grants: T['grants'];
  repos: T[];
}

/**
 * Repositories that grant the same actions through the same roles, largest
 * group first: a role like `repo:*:admin` makes one row of what would be a
 * row per repository; repositories that differ are the exceptions.
 */
export function groupRepoAccess<T extends RepoAccess>(repos: readonly T[]): RepoAccessGroup<T>[] {
  const groups = new Map<string, RepoAccessGroup<T>>();
  for (const r of repos) {
    const grants = [...r.grants].sort((a, b) => `${a.action}${a.source}${a.privilege ?? ''}`.localeCompare(`${b.action}${b.source}${b.privilege ?? ''}`));
    const key = JSON.stringify([r.actions, grants.map((g) => [g.action, g.source, g.role, g.privilege])]);
    const g: RepoAccessGroup<T> = groups.get(key) ?? { actions: r.actions, grants: r.grants, repos: [] };
    g.repos.push(r);
    groups.set(key, g);
  }
  return [...groups.values()].sort((a, b) => b.repos.length - a.repos.length);
}

/** "npm-a, npm-b and 3 more" */
export function nameList(names: readonly string[], shown = 3): string {
  if (names.length <= shown) return names.length > 1 ? `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}` : (names[0] ?? '');
  return `${names.slice(0, shown).join(', ')} and ${names.length - shown} more`;
}
