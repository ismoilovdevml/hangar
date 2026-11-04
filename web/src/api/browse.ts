/**
 * Queries and mutations of the repository browser: tree folders, asset
 * detail and deletion, package facts (README, licences) and the
 * repositories that hold one package.
 */
import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { catalogKeys } from './catalog';
import { apiFetch, ApiRequestError } from './client';
import { queryKeys } from './hooks';
import type { AssetDetail, BrowsePage, ComponentInfo, PublishedComponentPage } from './types';

export const BROWSE_PAGE = 200;

export const browseKeys = {
  all: ['browse'] as const,
  folder: (repo: string, path: string, cursor: string) => ['browse', 'folder', repo, path, cursor] as const,
  asset: (id: string) => ['browse', 'asset', id] as const,
  info: (id: string) => ['components', 'info', id] as const,
  holders: (name: string) => ['components', 'holders', name] as const,
};

/** One page of a folder's children (`path` is "" for the root or ends in "/"). */
export const browseFolderQuery = (repo: string, path: string, cursor?: string) =>
  queryOptions({
    queryKey: browseKeys.folder(repo, path, cursor ?? ''),
    queryFn: ({ signal }) =>
      apiFetch<BrowsePage>(`/repositories/${encodeURIComponent(repo)}/browse`, {
        query: { path, cursor, limit: BROWSE_PAGE },
        signal,
      }),
    staleTime: 30_000,
  });

export function useBrowseFolder(repo: string | undefined, path: string, cursor?: string) {
  return useQuery({ ...browseFolderQuery(repo ?? '', path, cursor), enabled: !!repo });
}

export function useAsset(id: string | undefined) {
  return useQuery({
    queryKey: browseKeys.asset(id ?? ''),
    enabled: !!id,
    queryFn: ({ signal }) => apiFetch<AssetDetail>(`/assets/${encodeURIComponent(id!)}`, { signal }),
  });
}

/** Everything the browser shows may have changed after a delete. */
function invalidateAfterDelete(qc: ReturnType<typeof useQueryClient>) {
  void qc.invalidateQueries({ queryKey: browseKeys.all });
  void qc.invalidateQueries({ queryKey: catalogKeys.components });
  void qc.invalidateQueries({ queryKey: catalogKeys.search });
  void qc.invalidateQueries({ queryKey: queryKeys.repositories });
  void qc.invalidateQueries({ queryKey: queryKeys.blobStores });
}

export function useDeleteAsset() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiFetch<void>(`/assets/${encodeURIComponent(id)}`, { method: 'DELETE' }),
    onSuccess: (_void, id) => {
      qc.removeQueries({ queryKey: browseKeys.asset(id) });
      invalidateAfterDelete(qc);
    },
  });
}

/** Refreshes the tree after a version was deleted elsewhere on the page. */
export function useInvalidateBrowse() {
  const qc = useQueryClient();
  return () => void qc.invalidateQueries({ queryKey: browseKeys.all });
}

/** README, licences, keywords and home page; extracted by the server on first view. */
export function useComponentInfo(id: string | undefined) {
  return useQuery({
    queryKey: browseKeys.info(id ?? ''),
    enabled: !!id,
    // Extraction errors other than an unreachable server are not worth a retry.
    retry: (n, err) => !(err instanceof ApiRequestError && err.status < 600) && n < 1,
    staleTime: 5 * 60_000,
    queryFn: ({ signal }) => apiFetch<ComponentInfo>(`/components/${encodeURIComponent(id!)}/info`, { signal }),
  });
}

/** Every readable version named `name` in any repository (the "Repositories" tab filters it). */
export function usePackageHolders(name: string | undefined) {
  return useQuery({
    queryKey: browseKeys.holders(name ?? ''),
    enabled: !!name,
    queryFn: ({ signal }) =>
      apiFetch<PublishedComponentPage>('/components', { query: { name, sort: '-published_at', limit: 1000 }, signal }),
  });
}
