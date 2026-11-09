/**
 * Queries and mutations for packages (components, assets, versions), search,
 * single repositories and blob stores.
 */
import {
  keepPreviousData,
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type InfiniteData,
} from '@tanstack/react-query';
import { apiFetch, ApiRequestError } from './client';
import { queryKeys } from './hooks';
import type {
  AssetPage,
  BlobStore,
  BlobStoreInput,
  ComponentDetail,
  ComponentFindings,
  FindingPage,
  PublishedComponentPage,
  Repository,
  RepositoryInput,
  RepositoryList,
  RoutingRules,
  RoutingSuggestions,
  RoutingTest,
  SearchResult,
  SearchSort,
  SigningKey,
} from './types';

export const catalogKeys = {
  components: ['components'] as const,
  lookup: (repo: string, name: string) => ['components', 'lookup', repo, name] as const,
  component: (id: string) => ['components', 'detail', id] as const,
  versions: (id: string) => ['components', 'versions', id] as const,
  assets: (id: string) => ['components', 'assets', id] as const,
  dependents: (id: string) => ['components', 'dependents', id] as const,
  findings: (id: string) => ['components', 'findings', id] as const,
  search: ['search'] as const,
  searchQuery: (q: string, sort: SearchSort) => ['search', 'query', q, sort] as const,
  packages: (repo: string) => ['search', 'packages', repo] as const,
  repository: (name: string) => ['repositories', 'detail', name] as const,
  allRepositories: ['repositories', 'all'] as const,
  blobStore: (name: string) => ['blob-stores', 'detail', name] as const,
  /** Keyed by the stored signing_key setting too, so switching to the shared key loads it. */
  signingKey: (name: string, key: string) => ['signing-key', name, key] as const,
};

/** Endpoints a server may not implement yet answer 501; those tabs are hidden, not faked. */
const isUnsupported = (err: unknown) => err instanceof ApiRequestError && err.status === 501;

/**
 * Optional per-component endpoints this server answered 501 for. Remembered
 * for the page's lifetime so every package view does not ask again; a server
 * upgrade is picked up on the next load.
 */
const unsupported = new Set<'dependents' | 'findings'>();

/** Forgets what the server did not support (tests). */
export function resetUnsupported(): void {
  unsupported.clear();
}

async function optional<T>(kind: 'dependents' | 'findings', load: () => Promise<T>): Promise<T | null> {
  if (unsupported.has(kind)) return null;
  try {
    return await load();
  } catch (err) {
    if (isUnsupported(err)) {
      unsupported.add(kind);
      return null;
    }
    throw err;
  }
}

type Cursor = string | undefined;
const nextCursor = (last: { next_cursor?: string | null }): Cursor => last.next_cursor ?? undefined;

/* ---------------------------------------------------------------- components */

/** Every version of `name` in `repo`, newest first (all namespaces sharing the base name). */
export function useComponentLookup(repo: string | undefined, name: string | undefined) {
  return useQuery({
    queryKey: catalogKeys.lookup(repo ?? '', name ?? ''),
    enabled: !!repo && !!name,
    queryFn: ({ signal }) =>
      apiFetch<PublishedComponentPage>('/components', {
        query: { repository: repo, name, sort: '-published_at', limit: 1000 },
        signal,
      }),
  });
}

export function useComponent(id: string | undefined) {
  return useQuery({
    queryKey: catalogKeys.component(id ?? ''),
    enabled: !!id,
    queryFn: ({ signal }) => apiFetch<ComponentDetail>(`/components/${encodeURIComponent(id!)}`, { signal }),
    placeholderData: keepPreviousData,
  });
}

export const VERSIONS_PAGE = 50;

export function useComponentVersions(id: string | undefined) {
  return useInfiniteQuery({
    queryKey: catalogKeys.versions(id ?? ''),
    enabled: !!id,
    initialPageParam: undefined as Cursor,
    queryFn: ({ signal, pageParam }) =>
      apiFetch<PublishedComponentPage>(`/components/${encodeURIComponent(id!)}/versions`, {
        query: { limit: VERSIONS_PAGE, cursor: pageParam },
        signal,
      }),
    getNextPageParam: nextCursor,
  });
}

export function useComponentAssets(id: string | undefined) {
  return useInfiniteQuery({
    queryKey: catalogKeys.assets(id ?? ''),
    enabled: !!id,
    initialPageParam: undefined as Cursor,
    queryFn: ({ signal, pageParam }) =>
      apiFetch<AssetPage>(`/components/${encodeURIComponent(id!)}/assets`, {
        query: { limit: 200, cursor: pageParam },
        signal,
      }),
    getNextPageParam: nextCursor,
  });
}

/** Dependents, or null when the server does not report them. */
export function useComponentDependents(id: string | undefined) {
  return useQuery({
    queryKey: catalogKeys.dependents(id ?? ''),
    enabled: !!id,
    retry: false,
    queryFn: ({ signal }) =>
      optional('dependents', () =>
        apiFetch<PublishedComponentPage>(`/components/${encodeURIComponent(id!)}/dependents`, { query: { limit: 100 }, signal }),
      ),
  });
}

/**
 * Security findings, or null when there is nothing honest to list: the
 * server has no scanner, the format is not scanned (Docker images), or the
 * component has not been scanned successfully yet and has no findings. An
 * empty list therefore always means "scanned, nothing known".
 */
export function useComponentFindings(id: string | undefined) {
  return useQuery({
    queryKey: catalogKeys.findings(id ?? ''),
    enabled: !!id,
    retry: false,
    queryFn: async ({ signal }): Promise<FindingPage | null> => {
      const page = await optional('findings', () =>
        apiFetch<ComponentFindings>(`/components/${encodeURIComponent(id!)}/findings`, { query: { limit: 200 }, signal }),
      );
      if (!page) return null;
      if (page.scan && page.scan.status !== 'ok' && page.items.length === 0) return null;
      return page;
    },
  });
}

export function useDeleteComponent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/components/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    onSuccess: (_void, id) => {
      // Everything about the deleted component (detail, versions, files...) is gone; asking again would 404.
      qc.removeQueries({ predicate: (q) => q.queryKey[0] === 'components' && q.queryKey[2] === id });
      void qc.invalidateQueries({ queryKey: catalogKeys.components });
      void qc.invalidateQueries({ queryKey: catalogKeys.search });
      void qc.invalidateQueries({ queryKey: queryKeys.repositories });
      void qc.invalidateQueries({ queryKey: queryKeys.blobStores });
    },
  });
}

/* ---------------------------------------------------------------- search */

export const SEARCH_PAGE = 25;

export function useSearch(q: string, sort: SearchSort) {
  return useInfiniteQuery({
    queryKey: catalogKeys.searchQuery(q, sort),
    enabled: q.trim().length > 0,
    retry: (n, err) => !(err instanceof ApiRequestError && err.status < 500) && n < 2,
    initialPageParam: undefined as Cursor,
    queryFn: ({ signal, pageParam }) =>
      apiFetch<SearchResult>('/search', { query: { q, sort, limit: SEARCH_PAGE, cursor: pageParam }, signal }),
    getNextPageParam: nextCursor,
    placeholderData: keepPreviousData,
  });
}

export const PACKAGES_PAGE = 200;

/** One row per package (newest version, version count) in a repository. */
export function useRepositoryPackages(repo: string | undefined) {
  return useInfiniteQuery({
    queryKey: catalogKeys.packages(repo ?? ''),
    enabled: !!repo,
    initialPageParam: undefined as Cursor,
    queryFn: ({ signal, pageParam }) =>
      apiFetch<SearchResult>('/search', {
        query: { q: `repo:${repo}`, sort: 'relevance', limit: PACKAGES_PAGE, cursor: pageParam },
        signal,
      }),
    getNextPageParam: nextCursor,
  });
}

export const flatItems = <T>(data: InfiniteData<{ items: T[] }> | undefined): T[] =>
  data ? data.pages.flatMap((p) => p.items) : [];

/* ---------------------------------------------------------------- repositories */

export function useRepository(name: string | undefined) {
  return useQuery({
    queryKey: catalogKeys.repository(name ?? ''),
    enabled: !!name,
    queryFn: ({ signal }) => apiFetch<Repository>(`/repositories/${encodeURIComponent(name!)}`, { signal }),
  });
}

/** Every repository the caller can read, bounded (pickers and the blob store detail). */
export function useAllRepositories(enabled = true) {
  return useQuery({
    queryKey: catalogKeys.allRepositories,
    enabled,
    queryFn: async ({ signal }) => {
      const items: Repository[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < 10; i++) {
        const page = await apiFetch<RepositoryList>('/repositories', { query: { limit: 1000, cursor }, signal });
        items.push(...page.items);
        if (!page.next_cursor) break;
        cursor = page.next_cursor;
      }
      return items;
    },
  });
}

export function useUpdateRepository(name: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: RepositoryInput) =>
      apiFetch<Repository>(`/repositories/${encodeURIComponent(name)}`, { method: 'PUT', body }),
    onSuccess: (repo) => {
      qc.setQueryData(catalogKeys.repository(name), repo);
      void qc.invalidateQueries({ queryKey: queryKeys.repositories, predicate: (q) => q.queryKey[2] !== name });
      void qc.invalidateQueries({ queryKey: queryKeys.upstreams });
      void qc.invalidateQueries({ queryKey: queryKeys.clientEndpoints });
    },
  });
}

export function useDeleteRepository(name: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiFetch<void>(`/repositories/${encodeURIComponent(name)}`, { method: 'DELETE' }),
    onSuccess: () => {
      qc.removeQueries({ queryKey: catalogKeys.repository(name) });
      void qc.invalidateQueries({ queryKey: queryKeys.repositories });
      void qc.invalidateQueries({ queryKey: queryKeys.blobStores });
      void qc.invalidateQueries({ queryKey: queryKeys.upstreams });
      void qc.invalidateQueries({ queryKey: queryKeys.clientEndpoints });
      void qc.invalidateQueries({ queryKey: catalogKeys.search });
      void qc.invalidateQueries({ queryKey: catalogKeys.components });
    },
  });
}

/* ---------------------------------------------------------------- signing keys */

/** The OpenPGP key that signs an apt or rpm hosted or group repository (generated on first use). */
export function useSigningKey(name: string, key: string, enabled = true) {
  return useQuery({
    queryKey: catalogKeys.signingKey(name, key),
    enabled,
    queryFn: ({ signal }) => apiFetch<SigningKey>(`/repositories/${encodeURIComponent(name)}/signing-key`, { signal }),
  });
}

/** Replaces the key: `rotate` generates a new one, `import` stores the given private key (write-only). */
export function useChangeSigningKey(name: string, key: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (change: { kind: 'rotate' } | { kind: 'import'; private_key: string; passphrase?: string }) => {
      const path = `/repositories/${encodeURIComponent(name)}/signing-key`;
      if (change.kind === 'rotate') return apiFetch<SigningKey>(`${path}:rotate`, { method: 'POST' });
      const body = { private_key: change.private_key, ...(change.passphrase ? { passphrase: change.passphrase } : {}) };
      return apiFetch<SigningKey>(path, { method: 'PUT', body });
    },
    onSuccess: (info) => {
      qc.setQueryData(catalogKeys.signingKey(name, key), info);
      // Other repositories on the shared key show the new one too.
      if (key === 'shared') void qc.invalidateQueries({ queryKey: ['signing-key'], predicate: (q) => q.queryKey[2] === 'shared' && q.queryKey[1] !== name });
    },
  });
}

/* ---------------------------------------------------------------- routing rules */

const routingPath = (name: string) => `/repositories/${encodeURIComponent(name)}/routing`;

const emptyRules = (r: RoutingRules) => !r.allow?.length && !r.block?.length && !(r.members ?? []).some((m) => m.exclusive?.length || m.block?.length);

/** Replaces a proxy's or group's routing rules (empty rules clear them); the repository is refetched. */
export function useSaveRouting(name: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (rules: RoutingRules): Promise<void> => {
      if (emptyRules(rules)) await apiFetch<void>(routingPath(name), { method: 'DELETE' });
      else await apiFetch<RoutingRules>(routingPath(name), { method: 'PUT', body: rules });
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: catalogKeys.repository(name) });
      void qc.invalidateQueries({ queryKey: queryKeys.repositories });
    },
  });
}

/** Evaluates the stored rules for one package name. */
export function useTestRouting(name: string) {
  return useMutation({
    mutationFn: (pkg: string) => apiFetch<RoutingTest>(`${routingPath(name)}:test`, { method: 'POST', body: { name: pkg } }),
  });
}

/** Patterns derived from the names the hosted repositories publish. */
export function useSuggestRouting(name: string) {
  return useMutation({
    mutationFn: () => apiFetch<RoutingSuggestions>(`${routingPath(name)}:suggest`),
  });
}

/* ---------------------------------------------------------------- blob stores */

export function useBlobStore(name: string | undefined) {
  return useQuery({
    queryKey: catalogKeys.blobStore(name ?? ''),
    enabled: !!name,
    queryFn: ({ signal }) => apiFetch<BlobStore>(`/blob-stores/${encodeURIComponent(name!)}`, { signal }),
  });
}

/**
 * PUT /blob-stores/{name}. Sections left out keep their stored values; a
 * missing soft_quota_bytes removes the quota.
 */
export function useUpdateBlobStore(name: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: BlobStoreInput) =>
      apiFetch<BlobStore>(`/blob-stores/${encodeURIComponent(name)}`, { method: 'PUT', body }),
    onSuccess: (store) => {
      qc.setQueryData(catalogKeys.blobStore(name), store);
      void qc.invalidateQueries({ queryKey: queryKeys.blobStores, predicate: (q) => q.queryKey[2] !== name });
    },
  });
}

/** DELETE /blob-stores/{name}; the server refuses (409) while repositories or blobs remain. */
export function useDeleteBlobStore(name: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiFetch<void>(`/blob-stores/${encodeURIComponent(name)}`, { method: 'DELETE' }),
    onSuccess: () => {
      qc.removeQueries({ queryKey: catalogKeys.blobStore(name) });
      void qc.invalidateQueries({ queryKey: queryKeys.blobStores });
    },
  });
}

export function useCreateBlobStore() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: BlobStoreInput) => apiFetch<BlobStore>('/blob-stores', { method: 'POST', body }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.blobStores });
    },
  });
}
