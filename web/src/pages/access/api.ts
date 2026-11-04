/**
 * Access management API: types from api/openapi.yaml (`npm run gen:api`)
 * and React Query hooks for users, roles, tokens, sessions and the audit log.
 */
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiFetch } from '../../api/client';
import { queryKeys } from '../../api/hooks';
import type { components, operations } from '../../api/schema';
import type { Repository, RepositoryList } from '../../api/types';

type Schemas = components['schemas'];

export type Member = Schemas['Member'];
export type MemberInput = Schemas['MemberInput'];
export type MemberPage = Schemas['MemberPage'];
export type Role = Schemas['Role'];
export type RoleInput = Schemas['RoleInput'];
export type Token = Schemas['Token'];
export type TokenInput = Schemas['TokenInput'];
export type TokenCreated = Schemas['TokenCreated'];
export type TokenPage = Schemas['TokenPage'];
export type TokenRotated = Schemas['TokenRotated'];
export type TokenRotateInput = Schemas['TokenRotateInput'];
export type SessionInfo = Schemas['SessionInfo'];
export type EffectivePermissions = Schemas['EffectivePermissions'];
export type ResourcePermissions = Schemas['ResourcePermissions'];
export type PermissionGrant = Schemas['PermissionGrant'];
export type AuditEvent = Schemas['AuditEvent'];
export type AuditEventPage = Schemas['AuditEventPage'];
export type ProfileInput = Schemas['ProfileInput'];
export type TokenPolicy = Schemas['TokenPolicy'];

export type UserQuery = NonNullable<operations['listUsers']['parameters']['query']>;
export type TokenQuery = NonNullable<operations['listTokens']['parameters']['query']>;

export const accessKeys = {
  all: ['access'] as const,
  users: (q: UserQuery) => ['access', 'users', q] as const,
  user: (id: string) => ['access', 'user', id] as const,
  permissions: (id: string) => ['access', 'permissions', id] as const,
  userSessions: (id: string) => ['access', 'sessions', id] as const,
  mySessions: ['access', 'sessions', 'me'] as const,
  roles: ['access', 'roles'] as const,
  tokens: (q: TokenQuery) => ['access', 'tokens', q] as const,
  audit: (action: string, target: string) => ['access', 'audit', action, target] as const,
  repoNames: ['access', 'repo-names'] as const,
};

/** After any change, refetch everything access-related (and the session: roles, warnings). */
function useInvalidate() {
  const qc = useQueryClient();
  return () => {
    void qc.invalidateQueries({ queryKey: accessKeys.all });
    void qc.invalidateQueries({ queryKey: queryKeys.session });
  };
}

/* ---------------------------------------------------------------- users */

export function useMembers(q: UserQuery, enabled = true) {
  return useQuery({
    queryKey: accessKeys.users(q),
    queryFn: ({ signal }) => apiFetch<MemberPage>('/users', { query: q, signal }),
    placeholderData: keepPreviousData,
    enabled,
  });
}

export function useMember(id: string | undefined) {
  return useQuery({
    queryKey: accessKeys.user(id ?? ''),
    queryFn: ({ signal }) => apiFetch<Member>(`/users/${encodeURIComponent(id!)}`, { signal }),
    enabled: !!id,
  });
}

function useMutate<V, R>(fn: (v: V) => Promise<R>) {
  const invalidate = useInvalidate();
  return useMutation({ mutationFn: fn, onSuccess: invalidate });
}

const userPath = (id: string, rest = '') => `/users/${encodeURIComponent(id)}${rest}`;

export const useCreateMember = () =>
  useMutate((body: MemberInput) => apiFetch<Member>('/users', { method: 'POST', body }));
export const useUpdateMember = () =>
  useMutate(({ id, body }: { id: string; body: MemberInput }) => apiFetch<Member>(userPath(id), { method: 'PUT', body }));
export const useDeleteMember = () => useMutate((id: string) => apiFetch<void>(userPath(id), { method: 'DELETE' }));
export const useSetDisabled = () =>
  useMutate(({ id, disabled }: { id: string; disabled: boolean }) =>
    apiFetch<Member>(userPath(id, disabled ? '/disable' : '/enable'), { method: 'POST' }),
  );
export const useUnlock = () => useMutate((id: string) => apiFetch<Member>(userPath(id, '/unlock'), { method: 'POST' }));
export const useRequirePasswordChange = () =>
  useMutate((id: string) => apiFetch<Member>(userPath(id, '/require-password-change'), { method: 'POST' }));
export const useResetPassword = () =>
  useMutate(({ id, password }: { id: string; password: string }) =>
    apiFetch<void>(userPath(id, '/password'), { method: 'PUT', body: { password } }),
  );

export function usePermissions(id: string | undefined) {
  return useQuery({
    queryKey: accessKeys.permissions(id ?? ''),
    queryFn: ({ signal }) => apiFetch<EffectivePermissions>(userPath(id!, '/permissions'), { signal }),
    enabled: !!id,
  });
}

/* ---------------------------------------------------------------- sessions */

export function useUserSessions(id: string | undefined) {
  return useQuery({
    queryKey: accessKeys.userSessions(id ?? ''),
    queryFn: ({ signal }) => apiFetch<{ items: SessionInfo[] }>(userPath(id!, '/sessions'), { signal }),
    enabled: !!id,
  });
}

export const useEndUserSessions = () =>
  useMutate((id: string) => apiFetch<{ ended: number }>(userPath(id, '/sessions'), { method: 'DELETE' }));
export const useEndUserSession = () =>
  useMutate(({ id, sid }: { id: string; sid: string }) =>
    apiFetch<void>(userPath(id, `/sessions/${encodeURIComponent(sid)}`), { method: 'DELETE' }),
  );

export function useMySessions() {
  return useQuery({
    queryKey: accessKeys.mySessions,
    queryFn: ({ signal }) => apiFetch<{ items: SessionInfo[] }>('/me/sessions', { signal }),
  });
}

export const useEndMyOtherSessions = () =>
  useMutate(() => apiFetch<{ ended: number }>('/me/sessions', { method: 'DELETE' }));
export const useEndMySession = () =>
  useMutate((sid: string) => apiFetch<void>(`/me/sessions/${encodeURIComponent(sid)}`, { method: 'DELETE' }));
export const useUpdateProfile = () =>
  useMutate((body: ProfileInput) => apiFetch<Member>('/me/profile', { method: 'PUT', body }));
export const useChangePassword = () =>
  useMutate((body: { current_password: string; new_password: string }) =>
    apiFetch<void>('/me/password', { method: 'PUT', body }),
  );

/* ---------------------------------------------------------------- roles */

export function useRoles(enabled = true) {
  return useQuery({
    queryKey: accessKeys.roles,
    queryFn: ({ signal }) => apiFetch<{ items: Role[] }>('/roles', { signal }),
    enabled,
  });
}

const rolePath = (name: string, rest = '') => `/roles/${encodeURIComponent(name)}${rest}`;

export const useCreateRole = () => useMutate((body: RoleInput) => apiFetch<Role>('/roles', { method: 'POST', body }));
export const useUpdateRole = () =>
  useMutate(({ name, body }: { name: string; body: RoleInput }) => apiFetch<Role>(rolePath(name), { method: 'PUT', body }));
export const useDeleteRole = () => useMutate((name: string) => apiFetch<void>(rolePath(name), { method: 'DELETE' }));
export const useAddRoleMember = () =>
  useMutate(({ role, userId }: { role: string; userId: string }) =>
    apiFetch<void>(rolePath(role, '/members'), { method: 'POST', body: { user_id: userId } }),
  );
export const useRemoveRoleMember = () =>
  useMutate(({ role, userId }: { role: string; userId: string }) =>
    apiFetch<void>(rolePath(role, `/members/${encodeURIComponent(userId)}`), { method: 'DELETE' }),
  );

/* ---------------------------------------------------------------- tokens */

export function useTokens(q: TokenQuery, enabled = true) {
  return useQuery({
    queryKey: accessKeys.tokens(q),
    queryFn: ({ signal }) => apiFetch<TokenPage>('/tokens', { query: q, signal }),
    placeholderData: keepPreviousData,
    enabled,
  });
}

export const useCreateToken = () =>
  useMutate((body: TokenInput) => apiFetch<TokenCreated>('/tokens', { method: 'POST', body }));
export const useRevokeToken = () =>
  useMutate((id: string) => apiFetch<void>(`/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' }));
export const useRotateToken = () =>
  useMutate(({ id, body }: { id: string; body: TokenRotateInput }) =>
    apiFetch<TokenRotated>(`/tokens/${encodeURIComponent(id)}/rotate`, { method: 'POST', body }),
  );

/** Repositories with format, type and client URL, for the snippets shown with a new token (first 1000). */
export function useRepositoryTargets(enabled = true) {
  return useQuery({
    queryKey: [...accessKeys.repoNames, 'targets'] as const,
    queryFn: async ({ signal }): Promise<Repository[]> =>
      (await apiFetch<RepositoryList>('/repositories', { query: { limit: 1000 }, signal })).items,
    enabled,
    staleTime: 60_000,
  });
}

/** Repository names for the token scope picker (first 1000). */
export function useRepositoryNames(enabled: boolean) {
  return useQuery({
    queryKey: accessKeys.repoNames,
    queryFn: async ({ signal }) => {
      const list = await apiFetch<RepositoryList>('/repositories', { query: { limit: 1000 }, signal });
      return list.items.map((r) => r.name);
    },
    enabled,
    staleTime: 60_000,
  });
}

/* ---------------------------------------------------------------- audit */

export function useAccessAudit(action: string, target = '', limit = 50, enabled = true) {
  return useQuery({
    queryKey: [...accessKeys.audit(action, target), limit],
    queryFn: ({ signal }) =>
      apiFetch<AuditEventPage>('/audit', { query: { action, target: target || undefined, limit }, signal }),
    enabled,
  });
}

/** Fetches the next audit page for "Load more". */
export function fetchAuditPage(action: string, target: string, cursor: string, limit = 50) {
  return apiFetch<AuditEventPage>('/audit', { query: { action, target: target || undefined, cursor, limit } });
}
