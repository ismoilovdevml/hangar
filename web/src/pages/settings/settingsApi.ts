/**
 * Data access for the Settings section: sign-in providers (OIDC, LDAP) and
 * outbound proxies. Types come from api/openapi.yaml.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiRequestError, apiFetch } from '../../api/client';
import { queryKeys } from '../../api/hooks';
import type { components } from '../../api/schema';

type Schemas = components['schemas'];

export type OIDCSettings = Schemas['OIDCProviderSettings'];
export type OIDCInput = Schemas['OIDCSettingsInput'];
export type LDAPSettings = Schemas['LDAPProviderSettings'];
export type LDAPInput = Schemas['LDAPSettingsInput'];
export type GroupRole = Schemas['GroupRole'];
export type TestResult = Schemas['SettingsTestResult'];
export type TestStep = Schemas['SettingsTestStep'];
export type OutboundProxy = Schemas['OutboundProxy'];
export type OutboundProxyInput = Schemas['OutboundProxyInput'];
export type OutboundProxyList = Schemas['OutboundProxyList'];
export type IdentityProvider = Schemas['IdentityProvider'];

export const settingsKeys = {
  all: ['settings'] as const,
  oidc: ['settings', 'oidc'] as const,
  ldap: ['settings', 'ldap'] as const,
  providers: ['settings', 'identity-providers'] as const,
  proxies: ['settings', 'proxies'] as const,
};

export function useOidcSettings(enabled = true) {
  return useQuery({
    queryKey: settingsKeys.oidc,
    queryFn: ({ signal }) => apiFetch<OIDCSettings>('/identity-providers/oidc', { signal }),
    enabled,
  });
}

export function useLdapSettings(enabled = true) {
  return useQuery({
    queryKey: settingsKeys.ldap,
    queryFn: ({ signal }) => apiFetch<LDAPSettings>('/identity-providers/ldap', { signal }),
    enabled,
  });
}

export function useIdentityProviders(enabled = true) {
  return useQuery({
    queryKey: settingsKeys.providers,
    queryFn: ({ signal }) => apiFetch<{ items: IdentityProvider[] }>('/identity-providers', { signal }),
    enabled,
    refetchInterval: 30_000,
  });
}

/** Saved sign-in settings change the login page and the provider status at once. */
function useInvalidateSignIn() {
  const qc = useQueryClient();
  return () => {
    void qc.invalidateQueries({ queryKey: settingsKeys.providers });
    void qc.invalidateQueries({ queryKey: queryKeys.authProviders });
  };
}

export function useSaveOidc() {
  const qc = useQueryClient();
  const invalidate = useInvalidateSignIn();
  return useMutation({
    mutationFn: (body: OIDCInput) => apiFetch<OIDCSettings>('/identity-providers/oidc', { method: 'PUT', body }),
    onSuccess: (saved) => {
      qc.setQueryData(settingsKeys.oidc, saved);
      invalidate();
    },
  });
}

export function useSaveLdap() {
  const qc = useQueryClient();
  const invalidate = useInvalidateSignIn();
  return useMutation({
    mutationFn: (body: LDAPInput) => apiFetch<LDAPSettings>('/identity-providers/ldap', { method: 'PUT', body }),
    onSuccess: (saved) => {
      qc.setQueryData(settingsKeys.ldap, saved);
      invalidate();
    },
  });
}

export function useTestOidc() {
  return useMutation({
    mutationFn: (settings: OIDCInput | undefined) =>
      apiFetch<TestResult>('/identity-providers/oidc:test', { method: 'POST', body: settings ? { settings } : {} }),
  });
}

export function useTestLdap() {
  return useMutation({
    mutationFn: ({ settings, username }: { settings?: LDAPInput; username?: string }) =>
      apiFetch<TestResult>('/identity-providers/ldap:test', { method: 'POST', body: { settings, username } }),
  });
}

/* ---------------------------------------------------------------- proxies */

export function useProxies(enabled = true) {
  return useQuery({
    queryKey: settingsKeys.proxies,
    queryFn: ({ signal }) => apiFetch<OutboundProxyList>('/proxies', { signal }),
    enabled,
  });
}

const proxyPath = (name: string, rest = '') => `/proxies/${encodeURIComponent(name)}${rest}`;

function useProxyMutation<A, R>(fn: (args: A) => Promise<R>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: settingsKeys.proxies });
    },
  });
}

export const useCreateProxy = () =>
  useProxyMutation((body: OutboundProxyInput) => apiFetch<OutboundProxy>('/proxies', { method: 'POST', body }));
export const useUpdateProxy = () =>
  useProxyMutation(({ name, body }: { name: string; body: OutboundProxyInput }) =>
    apiFetch<OutboundProxy>(proxyPath(name), { method: 'PUT', body }),
  );
export const useDeleteProxy = () => useProxyMutation((name: string) => apiFetch<void>(proxyPath(name), { method: 'DELETE' }));

export function useTestProxy() {
  return useMutation({
    mutationFn: ({ name, url, settings }: { name: string; url?: string; settings?: OutboundProxyInput }) =>
      apiFetch<TestResult>(proxyPath(name, ':test'), { method: 'POST', body: { url: url || undefined, settings } }),
  });
}

/* ---------------------------------------------------------------- errors */

/** Field errors of a 400 answer (details.fields), keyed like the request body ("issuer_url", "group_roles[1].roles"). */
export function fieldErrorsOf(err: unknown): Record<string, string> {
  if (err instanceof ApiRequestError && err.status === 400) {
    const fields = (err.body?.details as { fields?: unknown } | undefined)?.fields;
    if (fields && typeof fields === 'object') return fields as Record<string, string>;
  }
  return {};
}

/** A sentence for an error that is not about one field. */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return 'You do not have permission to change these settings.';
    if (err.status === 400 && Object.keys(fieldErrorsOf(err)).length > 0) return 'Fix the highlighted fields and save again.';
    return err.body?.message ?? err.message;
  }
  return err instanceof Error ? err.message : 'Something went wrong.';
}
