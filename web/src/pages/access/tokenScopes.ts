/**
 * Token permissions for the create-token form: presets, the custom editor,
 * the owner's limits and a plain-language summary. Pure functions only.
 */
import { allows, globMatch, parsePrivilege, repoPatternError, SYSTEM_AREAS, uncoveredScopes, type Action } from './model';

export type PresetId = 'read' | 'publish' | 'repo-admin' | 'security' | 'admin' | 'custom';
export type SystemLevel = '' | 'read' | 'write' | 'admin';

export interface TokenPreset {
  id: PresetId;
  label: string;
  hint: string;
  /** The preset applies to repositories the user picks. */
  picksRepos: boolean;
  danger?: boolean;
}

export const TOKEN_PRESETS: TokenPreset[] = [
  { id: 'read', label: 'Read only', hint: 'Download from every repository', picksRepos: false },
  { id: 'publish', label: 'Publish to selected repositories', hint: 'Download from and publish to the repositories you pick', picksRepos: true },
  { id: 'repo-admin', label: 'Repository admin', hint: 'Manage the repositories you pick: settings, publishing, deletes', picksRepos: true },
  { id: 'security', label: 'Security reviewer', hint: 'Read vulnerability findings and waive them', picksRepos: false },
  { id: 'admin', label: 'Full admin', hint: 'Everything, including users, tokens and settings', picksRepos: false, danger: true },
  { id: 'custom', label: 'Custom', hint: 'Pick repositories, actions and administration areas', picksRepos: true },
];

export const SYSTEM_LEVELS: { id: SystemLevel; label: string }[] = [
  { id: '', label: 'None' },
  { id: 'read', label: 'Read' },
  { id: 'write', label: 'Write' },
  { id: 'admin', label: 'Admin' },
];

export interface PermissionDraft {
  preset: PresetId;
  /** Repository names and globs, for the presets that pick repositories. */
  repos: string[];
  /** Custom: what the token may do in those repositories. */
  repoActions: Action[];
  /** Custom: level per system area id. */
  system: Record<string, SystemLevel>;
}

export const DEFAULT_DRAFT: PermissionDraft = { preset: 'read', repos: [], repoActions: ['read'], system: {} };

const FULL_ADMIN = ['system:*', 'repo:*:admin'];
const SECURITY = ['repo:*:read', 'system:security:read', 'system:security:write'];

/** Scopes for one system area at a level; write also reads (the server checks each action). */
export function systemScopes(area: string, level: SystemLevel): string[] {
  switch (level) {
    case 'read':
      return [`system:${area}:read`];
    case 'write':
      return [`system:${area}:read`, `system:${area}:write`];
    case 'admin':
      return [`system:${area}:admin`];
    default:
      return [];
  }
}

function repoError(repos: string[]): string | null {
  if (repos.length === 0) return 'Pick at least one repository or add a pattern.';
  for (const r of repos) {
    const err = repoPatternError(r);
    if (err) return `${r}: ${err}`;
  }
  return null;
}

/** The scopes a draft asks for, or what is missing. */
export function draftScopes(d: PermissionDraft): { scopes: string[]; error: string | null } {
  let scopes: string[] = [];
  switch (d.preset) {
    case 'read':
      scopes = ['repo:*:read'];
      break;
    case 'security':
      scopes = [...SECURITY];
      break;
    case 'admin':
      scopes = [...FULL_ADMIN];
      break;
    case 'publish':
    case 'repo-admin': {
      const err = repoError(d.repos);
      if (err) return { scopes: [], error: err };
      scopes = d.repos.flatMap((r) => (d.preset === 'publish' ? [`repo:${r}:read`, `repo:${r}:write`] : [`repo:${r}:admin`]));
      break;
    }
    case 'custom': {
      if (d.repos.length > 0) {
        const err = repoError(d.repos);
        if (err) return { scopes: [], error: err };
        if (d.repoActions.length === 0) return { scopes: [], error: 'Pick what the token may do in the repositories.' };
        const actions: Action[] = d.repoActions.includes('admin') ? ['admin'] : d.repoActions;
        scopes = d.repos.flatMap((r) => actions.map((a) => `repo:${r}:${a}`));
      }
      for (const a of SYSTEM_AREAS) scopes.push(...systemScopes(a.id, d.system[a.id] ?? ''));
      break;
    }
  }
  scopes = [...new Set(scopes)];
  if (scopes.length === 0) return { scopes, error: 'Give the token at least one permission.' };
  if (scopes.length > 100) return { scopes: [], error: 'At most 100 permissions; use a pattern such as npm-* instead of a long list.' };
  return { scopes, error: null };
}

/** The highest level of a system area the owner holds. */
export function ownerLevel(privileges: readonly string[] | undefined, area: string): SystemLevel {
  if (allows(privileges, 'system', area, 'admin')) return 'admin';
  if (allows(privileges, 'system', area, 'write') && allows(privileges, 'system', area, 'read')) return 'write';
  if (allows(privileges, 'system', area, 'read')) return 'read';
  return '';
}

const LEVEL_RANK: Record<SystemLevel, number> = { '': 0, read: 1, write: 2, admin: 3 };

export function levelAllowed(owner: SystemLevel, wanted: SystemLevel): boolean {
  return LEVEL_RANK[wanted] <= LEVEL_RANK[owner];
}

/** Whether the owner may take `action` on a repository name or every repository a glob matches. */
export function ownerCanRepo(privileges: readonly string[] | undefined, target: string, action: Action): boolean {
  if (/[*?[]/.test(target)) return uncoveredScopes([`repo:${target}:${action}`], privileges).length === 0;
  return allows(privileges, 'repo', target, action);
}

/** Whether the owner holds `action` on at least some repositories. */
export function ownerHasRepoAction(privileges: readonly string[] | undefined, action: Action): boolean {
  return (privileges ?? []).some((s) => {
    const p = parsePrivilege(s);
    return !!p && p.domain === 'repo' && (p.action === action || p.action === 'admin' || p.action === '*');
  });
}

/** Whether a preset can work for the owner at all; the reason when it cannot. */
export function presetAvailability(id: PresetId, privileges: readonly string[] | undefined): { ok: boolean; reason?: string } {
  switch (id) {
    case 'read':
      return ownerHasRepoAction(privileges, 'read') ? { ok: true } : { ok: false, reason: 'Your roles do not let you read any repository.' };
    case 'publish':
      return ownerHasRepoAction(privileges, 'write') ? { ok: true } : { ok: false, reason: 'Your roles do not let you publish to any repository.' };
    case 'repo-admin':
      return ownerHasRepoAction(privileges, 'admin') ? { ok: true } : { ok: false, reason: 'Your roles do not let you manage any repository.' };
    case 'security':
      return ownerLevel(privileges, 'security') === 'admin' || ownerLevel(privileges, 'security') === 'write'
        ? { ok: true }
        : { ok: false, reason: 'Waiving findings needs the security write privilege, which your roles do not grant.' };
    case 'admin':
      return uncoveredScopes(FULL_ADMIN, privileges).length === 0
        ? { ok: true }
        : { ok: false, reason: 'Only administrators can create a full-admin token.' };
    default:
      return { ok: true };
  }
}

/* ---------------------------------------------------------------- summary */

function listText(items: string[]): string {
  if (items.length <= 1) return items[0] ?? '';
  return `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`;
}

const areaLabel = (id: string) => {
  const label = SYSTEM_AREAS.find((a) => a.id === id)?.label ?? id;
  return label.charAt(0).toLowerCase() + label.slice(1);
};

/** Scopes as they act: read-only tokens are narrowed to read. */
export function effectiveScopes(scopes: string[], readOnly: boolean): string[] {
  if (!readOnly) return scopes;
  return [
    ...new Set(
      scopes.map((s) => {
        const p = parsePrivilege(s);
        if (!p) return s;
        return `${p.domain}:${p.pattern}:read`;
      }),
    ),
  ];
}

/**
 * What a token can and cannot do, in plain sentences:
 * "Can download from npm-*", "Can publish to nuget-hosted", "Cannot manage users & roles".
 */
export function summarizeScopes(scopes: string[], readOnly = false): { can: string[]; cannot: string[] } {
  const eff = effectiveScopes(scopes, readOnly);
  const repo: Record<string, Set<string>> = { read: new Set(), write: new Set(), delete: new Set(), admin: new Set() };
  const sys: Record<string, SystemLevel> = {};
  let allSystem: Action | '*' | null = null;
  for (const s of eff) {
    const p = parsePrivilege(s);
    if (!p) continue;
    if (p.domain === 'repo') {
      const action = p.action === '*' ? 'admin' : p.action;
      repo[action]!.add(p.pattern);
    } else if (p.pattern === '*') {
      allSystem = p.action;
    } else {
      const level: SystemLevel = p.action === 'admin' || p.action === '*' ? 'admin' : p.action === 'write' || p.action === 'delete' ? 'write' : 'read';
      if (LEVEL_RANK[level] > LEVEL_RANK[sys[p.pattern] ?? '']) sys[p.pattern] = level;
    }
  }
  // admin covers the rest for the same target; '*' covers every name.
  const covered = (action: string, target: string) =>
    (action !== 'admin' && (repo.admin!.has(target) || repo.admin!.has('*'))) || (target !== '*' && repo[action]!.has('*'));
  const targets = (action: string) => {
    const out = [...repo[action]!].filter((t) => !covered(action, t));
    return out.map((t) => (t === '*' ? 'every repository' : t));
  };
  const can: string[] = [];
  const verbs: [string, string][] = [
    ['admin', 'Can manage'],
    ['write', 'Can publish to'],
    ['delete', 'Can delete packages in'],
    ['read', 'Can download from'],
  ];
  for (const [action, verb] of verbs) {
    const t = targets(action);
    if (t.length) can.push(`${verb} ${listText(t)}`);
  }
  if (allSystem === '*' || allSystem === 'admin') can.push('Can manage every administration area: users, tokens, settings and more');
  else if (allSystem) can.push(`Can ${allSystem === 'read' ? 'view' : 'change'} every administration area`);
  const byLevel = (level: SystemLevel) => Object.keys(sys).filter((a) => sys[a] === level).map(areaLabel);
  if (!allSystem || allSystem === 'read') {
    const m = byLevel('admin');
    const w = byLevel('write');
    const r = byLevel('read');
    if (m.length) can.push(`Can manage ${listText(m)}`);
    if (w.length) can.push(`Can view and change ${listText(w)}`);
    if (r.length && allSystem !== 'read') can.push(`Can view ${listText(r)}`);
  }

  const cannot: string[] = [];
  if (readOnly) cannot.push('Cannot change anything: the token is read-only');
  else if (repo.write!.size === 0 && repo.admin!.size === 0) cannot.push('Cannot publish packages');
  if (allSystem !== '*' && allSystem !== 'admin') {
    const missing = ['users', 'tokens', 'settings', 'repos'].filter((a) => sys[a] !== 'admin').map(areaLabel);
    if (missing.length) cannot.push(`Cannot manage ${listText(missing)}`);
  }
  return { can, cannot };
}

/** One line for the token table: the preset it matches, or the first thing it can do. */
export function shortSummary(scopes: string[], readOnly = false): string {
  const set = new Set(scopes);
  const same = (want: string[]) => want.length === set.size && want.every((s) => set.has(s));
  if (same(['repo:*:read'])) return 'Read only';
  if (same(FULL_ADMIN) || same(['system:*'])) return 'Full admin';
  if (same(SECURITY)) return 'Security reviewer';
  const { can } = summarizeScopes(scopes, readOnly);
  const first = can[0] ?? 'No permissions';
  return can.length > 1 ? `${first} (+${can.length - 1} more)` : first;
}

/** Repository names a target list reaches, for the picker's "matches" hint. */
export function matchingRepos(targets: string[], names: readonly string[]): string[] {
  return names.filter((n) => targets.some((t) => globMatch(t, n)));
}

/* ---------------------------------------------------------------- restrictions */

const IPV4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})(?:\/(\d{1,2}))?$/;
const IPV6 = /^[0-9a-f:.]+(?:\/(\d{1,3}))?$/i;

/** Splits the networks field (lines, commas or spaces) into entries. */
export function parseNetworks(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

/** Why an allowed-network entry is not an IP address or CIDR network, or null. */
export function networkError(entry: string): string | null {
  const v4 = IPV4.exec(entry);
  if (v4) {
    const octetsOk = v4.slice(1, 5).every((o) => Number(o) <= 255);
    const bitsOk = v4[5] === undefined || Number(v4[5]) <= 32;
    return octetsOk && bitsOk ? null : `${entry} is not a valid IPv4 address or network.`;
  }
  const v6 = IPV6.exec(entry);
  if (v6 && entry.includes(':') && (v6[1] === undefined || Number(v6[1]) <= 128)) return null;
  return `${entry} is not an IP address or CIDR network such as 203.0.113.0/24.`;
}

/** Lifetime of the replacement: the old token's, capped at the server maximum. */
export function rotatedLifetimeDays(t: { created_at: string; expires_at: string }, maxDays: number): number {
  const days = Math.round((new Date(t.expires_at).getTime() - new Date(t.created_at).getTime()) / 86_400_000);
  return Math.max(1, Math.min(days, maxDays));
}
