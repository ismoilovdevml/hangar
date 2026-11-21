/**
 * Data access for CI OIDC trust policies (api/openapi.yaml /trust-policies):
 * system:settings:read to list, system:settings:admin plus every privilege of
 * the policy's scopes to change one. Queries use the app-wide retry policy
 * (lib/queryClient); mutations are never retried.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiFetch } from '../../api/client';
import type { components } from '../../api/schema';
import { useMembers } from '../access/api';

type Schemas = components['schemas'];

export type TrustPolicy = Schemas['TrustPolicy'];
export type TrustPolicyInput = Schemas['TrustPolicyInput'];
export type TrustPolicyCondition = Schemas['TrustPolicyCondition'];

export const trustPolicyKeys = {
  all: ['trust-policies'] as const,
};

const path = (name: string) => `/trust-policies/${encodeURIComponent(name)}`;

export function useTrustPolicies(enabled = true) {
  return useQuery({
    queryKey: trustPolicyKeys.all,
    queryFn: ({ signal }) => apiFetch<{ items: TrustPolicy[] }>('/trust-policies', { signal }),
    enabled,
  });
}

function usePolicyMutation<A, R>(fn: (args: A) => Promise<R>) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: fn,
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: trustPolicyKeys.all });
    },
  });
}

export const useCreateTrustPolicy = () => usePolicyMutation((body: TrustPolicyInput) => apiFetch<TrustPolicy>('/trust-policies', { method: 'POST', body }));
export const useUpdateTrustPolicy = () =>
  usePolicyMutation(({ name, body }: { name: string; body: TrustPolicyInput }) => apiFetch<TrustPolicy>(path(name), { method: 'PUT', body }));
export const useDeleteTrustPolicy = () => usePolicyMutation((name: string) => apiFetch<void>(path(name), { method: 'DELETE' }));

/** Names of the service accounts, for suggestions; empty when the caller cannot list users. */
export function useServiceAccountNames(enabled: boolean): string[] {
  const q = useMembers({ kind: 'service_account', status: 'active', limit: 200 }, enabled);
  return (q.data?.items ?? []).map((m) => m.username);
}
