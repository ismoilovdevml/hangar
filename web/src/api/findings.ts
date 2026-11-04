/**
 * Queries and mutations of the Vulnerabilities screen: the fix plan,
 * findings, waivers, assignees, who pulls a package, the scanner status and
 * on-demand scans.
 */
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiRequestError, apiFetch, buildUrl } from './client';
import type {
  Assignee,
  Finding,
  FindingList,
  FixPlan,
  PackageChange,
  PackageWaiverInput,
  Pullers,
  RepositoryList,
  RescanStarted,
  Rescan,
  ScanningStatus,
  Severity,
  WaiverInput,
} from './types';

export const findingKeys = {
  all: ['findings'] as const,
  list: (q: FindingQuery) => ['findings', 'list', q] as const,
  detail: (id: string) => ['findings', 'detail', id] as const,
  status: ['findings', 'scanning-status'] as const,
  plan: (q: FixPlanQuery) => ['findings', 'fix-plan', q] as const,
  pullers: (id: string, window: string) => ['findings', 'pullers', id, window] as const,
  assignees: (q: string) => ['findings', 'assignees', q] as const,
  rescan: ['findings', 'rescan'] as const,
};

/** Query parameters of GET /vulnerabilities/fix-plan. */
export interface FixPlanQuery {
  tab?: 'in_use' | 'all' | 'waived';
  window?: string;
  assignee?: string;
  repository?: string;
  q?: string;
}

export function useFixPlan(q: FixPlanQuery) {
  return useQuery({
    queryKey: findingKeys.plan(q),
    queryFn: ({ signal }) => apiFetch<FixPlan>('/vulnerabilities/fix-plan', { query: { ...q }, signal }),
    placeholderData: keepPreviousData,
  });
}

/** Downloads the fix plan as CSV through the browser (same session and filters). */
export async function downloadFixPlanCsv(q: FixPlanQuery): Promise<void> {
  const res = await fetch(buildUrl('/vulnerabilities/fix-plan', { ...q, format: 'csv' }), {
    credentials: 'same-origin',
    headers: { Accept: 'text/csv' },
  });
  if (!res.ok) {
    let body = null;
    try {
      body = await res.json();
    } catch {
      body = null;
    }
    throw new ApiRequestError(res.status, body);
  }
  const blob = await res.blob();
  const name = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') ?? '')?.[1] ?? 'hangar-fix-plan.csv';
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function usePullers(id: string | undefined, window: string, enabled: boolean) {
  return useQuery({
    queryKey: findingKeys.pullers(id ?? '', window),
    enabled: !!id && enabled,
    queryFn: ({ signal }) => apiFetch<Pullers>(`/findings/${encodeURIComponent(id!)}/pullers`, { query: { window }, signal }),
  });
}

export function useAssignees(q: string, enabled: boolean) {
  return useQuery({
    queryKey: findingKeys.assignees(q),
    enabled,
    queryFn: ({ signal }) => apiFetch<{ items: Assignee[] }>('/vulnerabilities/assignees', { query: { q, limit: 50 }, signal }),
    placeholderData: keepPreviousData,
    staleTime: 30_000,
  });
}

/** The last rescan of every repository; polled while it runs. */
export function useRescan(enabled: boolean) {
  return useQuery({
    queryKey: findingKeys.rescan,
    enabled,
    queryFn: ({ signal }) => apiFetch<{ run: Rescan | null }>('/vulnerabilities/rescan', { signal }),
    refetchInterval: (query) => (query.state.data?.run?.state === 'running' ? 3_000 : false),
  });
}

export function useStartRescan() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => apiFetch<RescanStarted>('/vulnerabilities/rescan', { method: 'POST' }),
    onSuccess: (r) => {
      qc.setQueryData(findingKeys.rescan, { run: r.run });
      void qc.invalidateQueries({ queryKey: findingKeys.status });
    },
  });
}

/** Sets (assignee) or clears (null) the assignee of one finding. */
export function useAssignFinding() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, assignee }: { id: string; assignee: string | null }) =>
      assignee
        ? apiFetch<Finding>(`/findings/${encodeURIComponent(id)}/assignee`, { method: 'PUT', body: { assignee } })
        : apiFetch<Finding>(`/findings/${encodeURIComponent(id)}/assignee`, { method: 'DELETE' }),
    onSuccess: (f) => {
      qc.setQueryData(findingKeys.detail(f.id), f);
      void qc.invalidateQueries({ queryKey: findingKeys.all });
    },
  });
}

/** Sets (assignee) or clears (null) the assignee of every finding of a package. */
export function useAssignPackage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ key, assignee }: { key: string; assignee: string | null }) =>
      apiFetch<PackageChange>('/vulnerabilities/fix-plan/assign', { method: 'POST', body: { package: key, assignee } }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: findingKeys.all }),
  });
}

/** Waives every finding of a package, or the ones listed. */
export function useWaivePackage() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: PackageWaiverInput) => apiFetch<PackageChange>('/vulnerabilities/fix-plan/waive', { method: 'POST', body: input }),
    onSuccess: () => invalidateAll(qc),
  });
}

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
