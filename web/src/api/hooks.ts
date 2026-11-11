import { keepPreviousData, queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiFetch, resetCsrfToken } from './client';
import type {
  AuthProviders,
  BlobStoreList,
  CleanupPolicyPage,
  ClientEndpoint,
  Format,
  FormatInfo,
  ListRepositoriesQuery,
  LoginRequest,
  OverviewMetrics,
  OverviewRange,
  Page,
  PublishedComponent,
  Repository,
  RepositoryInput,
  RepositoryList,
  Session,
  SystemStatus,
  UpstreamHealth,
} from './types';

export const queryKeys = {
  session: ['session'] as const,
  systemStatus: ['system', 'status'] as const,
  authProviders: ['auth', 'providers'] as const,
  formats: ['formats'] as const,
  overview: (range: OverviewRange) => ['metrics', 'overview', range] as const,
  recentComponents: ['components', 'recent'] as const,
  upstreams: ['upstreams', 'health'] as const,
  upstreamHealth: (range: OverviewRange) => ['upstreams', 'health', range] as const,
  clientEndpoints: ['me', 'client-endpoints'] as const,
  repositories: ['repositories'] as const,
  repositoryList: (q: ListRepositoriesQuery) => ['repositories', 'list', q] as const,
  repositoriesByFormat: (format: Format) => ['repositories', 'by-format', format] as const,
  blobStores: ['blob-stores'] as const,
  cleanupPolicies: ['cleanup-policies'] as const,
};

export const sessionQuery = queryOptions({
  queryKey: queryKeys.session,
  queryFn: ({ signal }) => apiFetch<Session>('/auth/session', { signal, allowUnauthorized: true }),
  staleTime: 5 * 60_000,
  retry: false,
});

export function useSession() {
  return useQuery(sessionQuery);
}

/** GET /system/status; read it through useSystemHealth() for anything that shows health. */
export function useSystemStatus() {
  return useQuery({
    queryKey: queryKeys.systemStatus,
    queryFn: ({ signal }) => apiFetch<SystemStatus>('/system/status', { signal, allowUnauthorized: true }),
    // As often as the System status screen, so the two never tell different stories for long.
    refetchInterval: 15_000,
  });
}

export function useAuthProviders() {
  return useQuery({
    queryKey: queryKeys.authProviders,
    queryFn: ({ signal }) => apiFetch<AuthProviders>('/auth/providers', { signal, allowUnauthorized: true }),
    staleTime: Infinity,
  });
}

export function useLogin() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: LoginRequest) =>
      apiFetch<Session>('/auth/login', { method: 'POST', body, allowUnauthorized: true }),
    onSuccess: (session) => {
      // A new principal: nothing cached for the previous one may leak through.
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== 'auth' && q.queryKey[0] !== 'system' });
      qc.setQueryData(queryKeys.session, session);
    },
  });
}

/** POST /auth/logout: a single sign-on session also ends at the provider, whose logout page the browser visits next. */
export type LogoutResult = { logout_url: string } | undefined;

/**
 * Leaves the app after sign-out: to the identity provider's logout page when
 * the server returned one (it comes back to /login), else straight to /login.
 * Only absolute http(s) URLs are followed.
 */
export function leaveAfterLogout(res: LogoutResult | void, toLogin: () => void, assign: (url: string) => void = (u) => window.location.assign(u)) {
  const url = res?.logout_url;
  if (url && /^https?:\/\//i.test(url)) assign(url);
  else toLogin();
}

export function useLogout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiFetch<LogoutResult>('/auth/logout', { method: 'POST', allowUnauthorized: true }),
    onSettled: () => {
      resetCsrfToken();
      qc.clear();
    },
  });
}

export function useFormats() {
  return useQuery({
    queryKey: queryKeys.formats,
    queryFn: ({ signal }) => apiFetch<{ items: FormatInfo[] }>('/formats', { signal }),
    staleTime: Infinity,
  });
}

/* ---------------------------------------------------------------- overview */

export function useOverview(range: OverviewRange) {
  return useQuery({
    queryKey: queryKeys.overview(range),
    queryFn: ({ signal }) =>
      apiFetch<OverviewMetrics>('/metrics/overview', { query: { range }, signal }),
    refetchInterval: 60_000,
    placeholderData: keepPreviousData,
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

/** Live state of every proxy remote, with the median latency recorded over `range`. */
export function useUpstreamHealth(range: OverviewRange) {
  return useQuery({
    queryKey: queryKeys.upstreamHealth(range),
    queryFn: ({ signal }) => apiFetch<Page<UpstreamHealth>>('/upstreams/health', { query: { range }, signal }),
    refetchInterval: 30_000,
    placeholderData: keepPreviousData,
  });
}

export function useClientEndpoints() {
  return useQuery({
    queryKey: queryKeys.clientEndpoints,
    queryFn: ({ signal }) =>
      apiFetch<Page<ClientEndpoint>>('/me/client-endpoints', { query: { limit: 3 }, signal }),
    staleTime: 10 * 60_000,
  });
}

/* ---------------------------------------------------------------- repositories */

export function useRepositories(query: ListRepositoriesQuery, enabled = true) {
  return useQuery({
    queryKey: queryKeys.repositoryList(query),
    queryFn: ({ signal }) => apiFetch<RepositoryList>('/repositories', { query, signal }),
    placeholderData: keepPreviousData,
    enabled,
  });
}

/** Every repository of one format the caller can read (group member picker). */
export function useRepositoriesByFormat(format: Format | undefined) {
  return useQuery({
    queryKey: queryKeys.repositoriesByFormat(format ?? 'raw'),
    enabled: !!format,
    queryFn: async ({ signal }) => {
      const items: Repository[] = [];
      let cursor: string | undefined;
      // Bounded: a group picker with more than 5000 candidates needs search, not a list.
      for (let i = 0; i < 5; i++) {
        const page = await apiFetch<RepositoryList>('/repositories', {
          query: { format, limit: 1000, cursor },
          signal,
        });
        items.push(...page.items);
        if (!page.next_cursor) break;
        cursor = page.next_cursor;
      }
      return items;
    },
  });
}

export function useBlobStores(enabled = true) {
  return useQuery({
    queryKey: queryKeys.blobStores,
    queryFn: ({ signal }) => apiFetch<BlobStoreList>('/blob-stores', { signal }),
    enabled,
  });
}

/** Cleanup policies to attach. Older servers may not serve the endpoint yet. */
export function useCleanupPolicies() {
  return useQuery({
    queryKey: queryKeys.cleanupPolicies,
    queryFn: ({ signal }) => apiFetch<CleanupPolicyPage>('/cleanup-policies', { signal }),
    retry: false,
  });
}

export function useCreateRepository() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: RepositoryInput) =>
      apiFetch<Repository>('/repositories', { method: 'POST', body }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.repositories });
      void qc.invalidateQueries({ queryKey: queryKeys.upstreams });
      void qc.invalidateQueries({ queryKey: queryKeys.clientEndpoints });
    },
  });
}
