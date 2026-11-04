/**
 * Queries and mutations of the Vulnerabilities screen: findings, waivers,
 * the daily trend, the scanner status and on-demand scans.
 */
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiFetch } from './client';
import type {
  Finding,
  FindingList,
  FindingTrend,
  RepositoryList,
  RepositoryScanQueued,
  ScanningStatus,
  ScanResult,
  Severity,
  WaiverInput,
} from './types';

export const findingKeys = {
  all: ['findings'] as const,
  list: (q: FindingQuery) => ['findings', 'list', q] as const,
  detail: (id: string) => ['findings', 'detail', id] as const,
  trend: (repository: string) => ['findings', 'trend', repository] as const,
  status: ['findings', 'scanning-status'] as const,
};

/** Query parameters of GET /findings, as the screen sends them. */
export interface FindingQuery {
  severity?: Severity;
  /** Comma-separated statuses; absent = everything except fixed. */
  status?: string;
  fix_available?: boolean;
  repository?: string;
  q?: string;
  cursor?: string;
  limit?: number;
}

export function useFindings(q: FindingQuery) {
  return useQuery({
    queryKey: findingKeys.list(q),
    queryFn: ({ signal }) => apiFetch<FindingList>('/findings', { query: { ...q }, signal }),
    placeholderData: keepPreviousData,
  });
}

export function useFinding(id: string | undefined) {
  return useQuery({
    queryKey: findingKeys.detail(id ?? ''),
    enabled: !!id,
    queryFn: ({ signal }) => apiFetch<Finding>(`/findings/${encodeURIComponent(id!)}`, { signal }),
  });
}

export function useFindingTrend(repository: string | undefined) {
  return useQuery({
    queryKey: findingKeys.trend(repository ?? ''),
    queryFn: ({ signal }) => apiFetch<FindingTrend>('/findings/trend', { query: { days: 30, repository }, signal }),
    placeholderData: keepPreviousData,
  });
}

export function useScanningStatus() {
  return useQuery({
    queryKey: findingKeys.status,
    queryFn: ({ signal }) => apiFetch<ScanningStatus>('/scanning/status', { signal }),
  });
}

/** Everything a waiver, a revoke or a scan can change. */
function invalidateAll(qc: ReturnType<typeof useQueryClient>) {
  void qc.invalidateQueries({ queryKey: findingKeys.all });
  void qc.invalidateQueries({ queryKey: ['components'] });
  void qc.invalidateQueries({ queryKey: ['metrics', 'overview'] });
  void qc.invalidateQueries({ queryKey: ['search'] });
}

export function useWaiveFinding() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: WaiverInput }) =>
      apiFetch<Finding>(`/findings/${encodeURIComponent(id)}/waiver`, { method: 'POST', body: input }),
    onSuccess: (f) => {
      qc.setQueryData(findingKeys.detail(f.id), f);
      invalidateAll(qc);
    },
  });
}

export function useRevokeWaiver() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiFetch<Finding>(`/findings/${encodeURIComponent(id)}/waiver`, { method: 'DELETE' }),
    onSuccess: (f) => {
      qc.setQueryData(findingKeys.detail(f.id), f);
      invalidateAll(qc);
    },
  });
}

export function useScanComponent() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (componentId: string) => apiFetch<ScanResult>(`/components/${encodeURIComponent(componentId)}:scan`, { method: 'POST' }),
    onSuccess: () => invalidateAll(qc),
  });
}

export function useScanRepository() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (repository: string) =>
      apiFetch<RepositoryScanQueued>(`/repositories/${encodeURIComponent(repository)}:scan`, { method: 'POST' }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: findingKeys.status }),
  });
}

/** Hosted and proxy repositories the caller can read (the repository filter; groups hold no components). */
export function useFindingRepositories() {
  return useQuery({
    queryKey: ['findings', 'repositories'] as const,
    queryFn: async ({ signal }) => {
      const list = await apiFetch<RepositoryList>('/repositories', { query: { limit: 1000 }, signal });
      return list.items.filter((r) => r.type !== 'group').sort((a, b) => a.name.localeCompare(b.name));
    },
    staleTime: 60_000,
  });
}
