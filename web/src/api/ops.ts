/**
 * Data hooks for the operations screens: cleanup policies, scheduled tasks,
 * the audit log and system status.
 */
import { useEffect } from 'react';
import { keepPreviousData, useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { ApiRequestError, apiFetch, buildUrl, csrfToken, CSRF_HEADER } from './client';
import { subscribeLiveEvents } from './events';
import { queryKeys, useSession } from './hooks';
import type {
  AuditEventPage,
  CleanupPolicy,
  CleanupPolicyInput,
  CleanupPreview,
  CleanupRun,
  ClusterStatus,
  ListAuditQuery,
  SystemConfig,
  Task,
  TaskList,
  TaskLogPage,
  TaskRun,
  TaskRunPage,
  TaskInput,
  LockfileResult,
} from './opsTypes';
import { allows, type PrivilegeAction } from '../lib/can';
import { applyTaskFinished, applyTaskProgress } from '../lib/tasks';
import type { Page } from './types';

export const opsKeys = {
  cleanupAll: [...queryKeys.cleanupPolicies, 'all'] as const,
  cleanupRuns: (name: string) => [...queryKeys.cleanupPolicies, 'runs', name] as const,
  tasks: ['tasks'] as const,
  taskList: ['tasks', 'list'] as const,
  task: (id: string) => ['tasks', 'detail', id] as const,
  taskRuns: (id: string, cursor?: string) => ['tasks', 'runs', id, cursor ?? ''] as const,
  taskLog: (id: string, runId: string) => ['tasks', 'log', id, runId] as const,
  audit: (q: ListAuditQuery) => ['audit', q] as const,
  cluster: ['nodes'] as const,
  systemConfig: ['system', 'config'] as const,
};

/** True when the server has no such endpoint yet (older build). */
export function isMissingEndpoint(err: unknown): boolean {
  return err instanceof ApiRequestError && (err.status === 404 || err.status === 501);
}

/** Whether the signed-in user holds `system:<area>:<action>` (UI hints only; the server decides). */
export function useCanSystem(area: string, action: PrivilegeAction): boolean {
  const session = useSession();
  return allows(session.data?.privileges, 'system', area, action);
}

/* ---------------------------------------------------------------- cleanup */

/** Every policy (bounded at 5000: more than that needs search, not a list). */
export function useAllCleanupPolicies() {
  return useQuery({
    queryKey: opsKeys.cleanupAll,
    queryFn: async ({ signal }) => {
      const items: CleanupPolicy[] = [];
      let cursor: string | undefined;
      for (let i = 0; i < 5; i++) {
        const page = await apiFetch<Page<CleanupPolicy>>('/cleanup-policies', { query: { limit: 1000, cursor }, signal });
        items.push(...page.items);
        if (!page.next_cursor) break;
        cursor = page.next_cursor;
      }
      return items;
    },
  });
}

export function usePreviewCleanup() {
  return useMutation({
    mutationFn: (body: CleanupPolicyInput) =>
      apiFetch<CleanupPreview>('/cleanup-policies/preview', { method: 'POST', body }),
  });
}

export function useSaveCleanupPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ original, body }: { original?: string; body: CleanupPolicyInput }) =>
      original
        ? apiFetch<CleanupPolicy>(`/cleanup-policies/${encodeURIComponent(original)}`, { method: 'PUT', body })
        : apiFetch<CleanupPolicy>('/cleanup-policies', { method: 'POST', body }),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: queryKeys.cleanupPolicies });
      void qc.invalidateQueries({ queryKey: queryKeys.repositories });
    },
  });
}

export function useDeleteCleanupPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => apiFetch<void>(`/cleanup-policies/${encodeURIComponent(name)}`, { method: 'DELETE' }),
    onSuccess: () => void qc.invalidateQueries({ queryKey: queryKeys.cleanupPolicies }),
  });
}

export function useRunCleanupPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) =>
      apiFetch<CleanupRun>(`/cleanup-policies/${encodeURIComponent(name)}/run`, { method: 'POST' }),
    onSuccess: (_run, name) => {
      void qc.invalidateQueries({ queryKey: opsKeys.cleanupRuns(name) });
      void qc.invalidateQueries({ queryKey: opsKeys.tasks });
    },
  });
}

export const CLEANUP_RUNS_LIMIT = 10;

export function useCleanupRuns(name: string | undefined) {
  return useQuery({
    queryKey: opsKeys.cleanupRuns(name ?? ''),
    enabled: !!name,
    queryFn: ({ signal }) =>
      apiFetch<Page<CleanupRun>>(`/cleanup-policies/${encodeURIComponent(name!)}/runs`, {
        query: { limit: CLEANUP_RUNS_LIMIT },
        signal,
      }),
    retry: false,
    refetchInterval: (q) => (q.state.data?.items.some((r) => r.status === 'queued' || r.status === 'running') ? 5_000 : false),
  });
}

/** Triggers a browser download of `blob` as `filename`. */
export function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

/** Full preview as CSV (every matching version, not just the sample). */
export async function fetchPreviewCsv(body: CleanupPolicyInput): Promise<Blob> {
  const headers: Record<string, string> = { Accept: 'text/csv', 'Content-Type': 'application/json' };
  const token = csrfToken();
  if (token) headers[CSRF_HEADER] = token;
  const res = await fetch(buildUrl('/cleanup-policies/preview'), {
    method: 'POST',
    credentials: 'same-origin',
    headers,
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    let err: { code: string; message: string } | null = null;
    try {
      err = (await res.json()) as { code: string; message: string };
    } catch {
      err = null;
    }
    throw new ApiRequestError(res.status, err);
  }
  if (!(res.headers.get('Content-Type') ?? '').includes('text/csv')) {
    throw new ApiRequestError(406, { code: 'not_acceptable', message: 'The server did not return CSV for this preview' });
  }
  return res.blob();
}

/* ---------------------------------------------------------------- tasks */

export function useTasks() {
  return useQuery({
    queryKey: opsKeys.taskList,
    queryFn: ({ signal }) => apiFetch<TaskList>('/tasks', { signal }),
    // Live events keep progress current; polling is the fallback without them.
    refetchInterval: (q) => (q.state.data?.running.length ? 10_000 : 60_000),
  });
}

export function useTask(id: string) {
  return useQuery({
    queryKey: opsKeys.task(id),
    queryFn: ({ signal }) => apiFetch<Task>(`/tasks/${encodeURIComponent(id)}`, { signal }),
  });
}

export const TASK_RUNS_LIMIT = 20;

export function useTaskRuns(id: string, cursor?: string) {
  return useQuery({
    queryKey: opsKeys.taskRuns(id, cursor),
    queryFn: ({ signal }) =>
      apiFetch<TaskRunPage>(`/tasks/${encodeURIComponent(id)}/runs`, { query: { cursor, limit: TASK_RUNS_LIMIT }, signal }),
    placeholderData: keepPreviousData,
    refetchInterval: (q) => (q.state.data?.items.some((r) => r.status === 'queued' || r.status === 'running') ? 10_000 : false),
  });
}

/** A run's log; re-read every 2 s while the run is active. */
export function useTaskRunLog(id: string, runId: string | undefined, active: boolean) {
  return useQuery({
    queryKey: opsKeys.taskLog(id, runId ?? ''),
    enabled: !!runId,
    queryFn: ({ signal }) =>
      apiFetch<TaskLogPage>(`/tasks/${encodeURIComponent(id)}/runs/${encodeURIComponent(runId!)}/log`, {
        query: { limit: 1000 },
        signal,
      }),
    retry: false,
    refetchInterval: active ? 2_000 : false,
  });
}

function invalidateTask(qc: QueryClient, id: string) {
  void qc.invalidateQueries({ queryKey: opsKeys.taskList });
  void qc.invalidateQueries({ queryKey: opsKeys.task(id) });
  void qc.invalidateQueries({ queryKey: ['tasks', 'runs', id] });
}

export function useRunTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiFetch<TaskRun>(`/tasks/${encodeURIComponent(id)}/run`, { method: 'POST' }),
    onSuccess: (run, id) => {
      qc.setQueryData<TaskList>(opsKeys.taskList, (prev) =>
        prev && !prev.running.some((r) => r.id === run.id) ? { ...prev, running: [...prev.running, run] } : prev,
      );
      invalidateTask(qc, id);
    },
  });
}

/** Creates a task (POST /tasks), optionally starting a run right away. */
export function useCreateTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ input, runNow }: { input: TaskInput; runNow: boolean }) => {
      const task = await apiFetch<Task>('/tasks', { method: 'POST', body: input });
      const run = runNow ? await apiFetch<TaskRun>(`/tasks/${encodeURIComponent(task.id)}/run`, { method: 'POST' }) : undefined;
      return { task, run };
    },
    onSuccess: ({ task }) => invalidateTask(qc, task.id),
  });
}

/** Gzip-compresses a file in the browser when it can (large lockfiles fit the API body limit). */
async function gzipped(file: Blob): Promise<{ body: BodyInit; headers?: Record<string, string> }> {
  if (typeof CompressionStream === 'undefined' || typeof file.stream !== 'function') return { body: file };
  const stream = file.stream().pipeThrough(new CompressionStream('gzip'));
  return { body: await new Response(stream).blob(), headers: { 'Content-Encoding': 'gzip' } };
}

/** Reads the coordinates a lockfile pins for a repository (POST /lockfiles). */
export function useParseLockfile() {
  return useMutation({
    mutationFn: async ({ repository, file }: { repository: string; file: File }) =>
      apiFetch<LockfileResult>('/lockfiles', { method: 'POST', query: { repository, filename: file.name }, raw: await gzipped(file) }),
  });
}

export function useStopTask() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => apiFetch<TaskRun>(`/tasks/${encodeURIComponent(id)}/stop`, { method: 'POST' }),
    onSuccess: (_run, id) => invalidateTask(qc, id),
  });
}

/**
 * Folds task.progress and task.finished events straight into the cached task
 * list and run pages, so progress bars move without waiting for a refetch.
 */
export function useTaskLiveUpdates() {
  const qc = useQueryClient();
  useEffect(
    () =>
      subscribeLiveEvents((e) => {
        if (e.type === 'task.progress') {
          qc.setQueryData<TaskList>(opsKeys.taskList, (prev) => prev && applyTaskProgress(prev, e.data));
          qc.setQueriesData<TaskRunPage>({ queryKey: ['tasks', 'runs', e.data.task_id] }, (prev) =>
            prev && { ...prev, items: prev.items.map((r) => (r.id === e.data.run_id ? { ...r, ...progressPatch(r, e.data) } : r)) },
          );
        } else if (e.type === 'task.finished') {
          qc.setQueryData<TaskList>(opsKeys.taskList, (prev) => prev && applyTaskFinished(prev, e.data));
          qc.setQueriesData<TaskRunPage>({ queryKey: ['tasks', 'runs', e.data.task_id] }, (prev) =>
            prev && { ...prev, items: prev.items.map((r) => (r.id === e.data.id ? e.data : r)) },
          );
        }
      }),
    [qc],
  );
}

function progressPatch(run: TaskRun, p: { progress: number; message?: string }): Partial<TaskRun> {
  return {
    progress: Math.max(run.progress, p.progress),
    message: p.message ?? run.message,
    status: run.status === 'queued' ? 'running' : run.status,
  };
}

/* ---------------------------------------------------------------- audit */

export const AUDIT_PAGE_SIZE = 50;

export function useAuditEvents(query: ListAuditQuery) {
  return useQuery({
    queryKey: opsKeys.audit(query),
    queryFn: ({ signal }) => apiFetch<AuditEventPage>('/audit', { query: { ...query, limit: AUDIT_PAGE_SIZE }, signal }),
    placeholderData: keepPreviousData,
  });
}

/* ---------------------------------------------------------------- system */

export function useClusterStatus() {
  return useQuery({
    queryKey: opsKeys.cluster,
    queryFn: ({ signal }) => apiFetch<ClusterStatus>('/nodes', { signal }),
    retry: false,
    refetchInterval: 30_000,
  });
}

export function useSystemConfig(enabled: boolean) {
  return useQuery({
    queryKey: opsKeys.systemConfig,
    enabled,
    queryFn: ({ signal }) => apiFetch<SystemConfig>('/system/config', { signal }),
    retry: false,
    staleTime: 5 * 60_000,
  });
}

/** A sentence for an API failure: the server's message, or `fallback` for network errors. */
export function errorText(err: unknown, fallback = 'Something went wrong. Try again.'): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return 'You do not have permission to do this.';
    const msg = err.body?.message;
    return msg ? msg.charAt(0).toUpperCase() + msg.slice(1) : fallback;
  }
  return fallback;
}
