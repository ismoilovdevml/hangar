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
  FindingPage,
  PublishedComponentPage,
  Repository,
  RepositoryInput,
  RepositoryList,
  SearchResult,
  SearchSort,
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
};

/** Endpoints a server may not implement yet answer 501; those tabs are hidden, not faked. */
const isMissing = (err: unknown) => err instanceof ApiRequestError && (err.status === 501 || err.status === 404);

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
    queryFn: async ({ signal }) => {
      try {
        return await apiFetch<PublishedComponentPage>(`/components/${encodeURIComponent(id!)}/dependents`, {
          query: { limit: 100 },
          signal,
        });
      } catch (err) {
        if (isMissing(err)) return null;
        throw err;
      }
    },
  });
}

/** Security findings, or null when the server has no scanner data for components. */
export function useComponentFindings(id: string | undefined) {
  return useQuery({
    queryKey: catalogKeys.findings(id ?? ''),
    enabled: !!id,
    retry: false,
    queryFn: async ({ signal }) => {
      try {
        return await apiFetch<FindingPage>(`/components/${encodeURIComponent(id!)}/findings`, {
          query: { limit: 200 },
          signal,
        });
      } catch (err) {
        if (isMissing(err)) return null;
        throw err;
      }
    },
  });
}

export function useDeleteComponent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/components/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    onSuccess: (_void, id) => {
      qc.removeQueries({ queryKey: catalogKeys.component(id) });
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

/* ---------------------------------------------------------------- blob stores */

export function useBlobStore(name: string | undefined) {
  return useQuery({
    queryKey: catalogKeys.blobStore(name ?? ''),
    enabled: !!name,
    queryFn: ({ signal }) => apiFetch<BlobStore>(`/blob-stores/${encodeURIComponent(name!)}`, { signal }),
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
