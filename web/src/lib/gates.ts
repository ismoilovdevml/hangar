/**
 * Which pages and actions the signed-in user may use, from the session's
 * effective privileges. The UI only hides what would fail; the server still
 * authorizes every call.
 */
import { ApiRequestError, PASSWORD_CHANGE_REQUIRED } from '../api/client';
import { allows, type PrivilegeAction } from './can';

/** A page or action, and the privilege an administrator would grant for it. */
export interface Gate {
  /** What the user cannot open, as it reads after "You don't have access to". */
  what: string;
  /** The privilege to ask for, as written in a role. */
  privilege: string;
  allowed: (privileges: readonly string[]) => boolean;
  /** Where the same user can go instead: "<text> <label>." */
  instead?: { text: string; label: string; to: '/account' | '/settings/system' };
}

export function systemGate(area: string, action: PrivilegeAction, what: string): Gate {
  return { what, privilege: `system:${area}:${action}`, allowed: (p) => allows(p, 'system', area, action) };
}

/**
 * Creating a repository needs system:repos:admin, or admin on the repository
 * names the user may create (a role granting `repo:npm-*:admin` creates `npm-team`).
 */
export function canCreateRepositories(privileges: readonly string[]): boolean {
  if (allows(privileges, 'system', 'repos', 'admin')) return true;
  return privileges.some((p) => /^repo:.+:(admin|\*)$/.test(p));
}

export const GATES = {
  tasks: systemGate('tasks', 'read', 'scheduled tasks'),
  newTask: systemGate('tasks', 'write', 'starting imports and pre-warms'),
  blobStores: systemGate('blobstores', 'read', 'blob stores'),
  cleanup: systemGate('cleanup', 'read', 'cleanup policies'),
  audit: systemGate('audit', 'read', 'the audit log'),
  cluster: {
    ...systemGate('cluster', 'read', 'cluster nodes'),
    instead: { text: 'The overall state is on the', label: 'System page', to: '/settings/system' },
  } satisfies Gate,
  logs: systemGate('cluster', 'admin', 'system logs'),
  settings: systemGate('settings', 'read', 'these settings'),
  createRepository: {
    what: 'creating repositories',
    privilege: 'system:repos:admin',
    allowed: canCreateRepositories,
  } satisfies Gate,
  /** People, service accounts, all tokens, roles or the audit log: any one opens the page. */
  access: {
    what: 'people, tokens and roles',
    privilege: 'system:users:read',
    allowed: (p) => ['users', 'tokens', 'audit'].some((area) => allows(p, 'system', area, 'read')),
    instead: { text: 'Your own profile, password, tokens and sessions are in', label: 'My account', to: '/account' },
  } satisfies Gate,
};

/**
 * Whether a gate lets the user in. A server that does not report privileges
 * gets no client-side gating: the server answers 403 when it must.
 */
export function passes(gate: Gate, privileges: readonly string[] | undefined): boolean {
  return !privileges || gate.allowed(privileges);
}

/** A refusal for lack of a privilege (not the CSRF or the password-change refusal). */
export function isForbidden(err: unknown): boolean {
  return err instanceof ApiRequestError && err.status === 403 && err.code !== 'csrf_failed' && err.code !== PASSWORD_CHANGE_REQUIRED;
}
