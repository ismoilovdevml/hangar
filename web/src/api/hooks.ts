import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiFetch } from './client';
import type {
  AuthProviders,
  ClientEndpoint,
  LoginRequest,
  OverviewMetrics,
  OverviewRange,
  Page,
  PublishedComponent,
  Session,
  SystemStatus,
  UpstreamHealth,
} from './types';

export const queryKeys = {
  session: ['session'] as const,
  systemStatus: ['system', 'status'] as const,
  authProviders: ['auth', 'providers'] as const,
  overview: (range: OverviewRange) => ['metrics', 'overview', range] as const,
  recentComponents: ['components', 'recent'] as const,
  upstreams: ['upstreams', 'health'] as const,
  clientEndpoints: ['me', 'client-endpoints'] as const,
};

export const sessionQuery = queryOptions({
  queryKey: queryKeys.session,
  queryFn: ({ signal }) => apiFetch<Session>('/auth/session', { signal }),
  staleTime: 5 * 60_000,
  retry: false,
});

export function useSession() {
  return useQuery(sessionQuery);
}

export function useSystemStatus() {
  return useQuery({
    queryKey: queryKeys.systemStatus,
    queryFn: ({ signal }) => apiFetch<SystemStatus>('/system/status', { signal }),
    refetchInterval: 30_000,
  });
}

export function useAuthProviders() {
  return useQuery({
    queryKey: queryKeys.authProviders,
    queryFn: ({ signal }) => apiFetch<AuthProviders>('/auth/providers', { signal }),
    staleTime: Infinity,
  });
}

export function useLogin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: LoginRequest) => apiFetch<Session>('/auth/login', { method: 'POST', body }),
    onSuccess: (session) => qc.setQueryData(queryKeys.session, session),
  });
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiFetch<void>('/auth/logout', { method: 'POST' }),
    onSettled: () => qc.clear(),
  });
}

export function useOverview(range: OverviewRange) {
  return useQuery({
    queryKey: queryKeys.overview(range),
    queryFn: ({ signal }) =>
      apiFetch<OverviewMetrics>('/metrics/overview', { query: { range }, signal }),
    refetchInterval: 60_000,
    placeholderData: (prev) => prev,
  });
}

export const RECENT_LIMIT = 6;

export function useRecentComponents() {
  return useQuery({
    queryKey: queryKeys.recentComponents,
    queryFn: ({ signal }) =>
      apiFetch<Page<PublishedComponent>>('/components', {
        query: { sort: '-published_at', limit: RECENT_LIMIT },
        signal,
      }),
  });
}

export function useUpstreamHealth() {
  return useQuery({
    queryKey: queryKeys.upstreams,
    queryFn: ({ signal }) => apiFetch<Page<UpstreamHealth>>('/upstreams/health', { signal }),
    refetchInterval: 30_000,
  });
}

export function useClientEndpoints() {
  return useQuery({
    queryKey: queryKeys.clientEndpoints,
    queryFn: ({ signal }) => apiFetch<Page<ClientEndpoint>>('/me/client-endpoints', { signal }),
    staleTime: 10 * 60_000,
  });
}
