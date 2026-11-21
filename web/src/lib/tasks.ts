import type { Task, TaskList, TaskProgress, TaskRun, TaskRunStatus, TaskType } from '../api/opsTypes';
import { formatDuration, type Tone } from './format';

export const TASK_TYPES: Record<TaskType, { label: string; hint: string }> = {
  cleanup: { label: 'Cleanup', hint: 'Applies a cleanup policy' },
  blob_compaction: { label: 'Blob compaction', hint: 'Frees space held by soft-deleted blobs' },
  docker_gc: { label: 'Docker GC', hint: 'Removes unreferenced layers' },
  docker_upload_cleanup: { label: 'Docker upload cleanup', hint: 'Ends abandoned docker uploads and frees their chunks' },
  auth_purge: { label: 'Expired sessions purge', hint: 'Deletes expired sessions and session tokens' },
  audit_partitions: { label: 'Audit partitions', hint: 'Creates the next monthly audit log partitions and drops months older than audit.retention' },
  rescan: { label: 'Rescan', hint: 'Scans packages against the latest advisories' },
  reindex: { label: 'Reindex search', hint: 'Rebuilds the search index' },
  directory_sync: { label: 'Directory sync', hint: 'Syncs users and groups from the directory' },
  db_backup: { label: 'Database backup', hint: 'Backs up PostgreSQL' },
  replication: { label: 'Replication', hint: 'Copies repositories to other sites' },
  proxy_metadata_refresh: { label: 'Proxy metadata refresh', hint: 'Refreshes cached upstream metadata' },
  blob_store_move: { label: 'Blob store move', hint: 'Moves repositories between blob stores' },
  repository_import: { label: 'Import', hint: 'Copies packages from another registry into a hosted repository' },
  proxy_prewarm: { label: 'Proxy pre-warm', hint: 'Fetches packages through a proxy so its cache holds them' },
};

export const ALL_TASK_TYPES = Object.keys(TASK_TYPES) as TaskType[];

export function isTaskType(v: unknown): v is TaskType {
  return typeof v === 'string' && v in TASK_TYPES;
}

export function taskTypeLabel(t: string): string {
  return isTaskType(t) ? TASK_TYPES[t].label : t;
}

export const RUN_STATUS: Record<TaskRunStatus, { label: string; tone: 'ok' | 'warn' | 'bad' | 'neutral'; pill: Tone }> = {
  queued: { label: 'Queued', tone: 'neutral', pill: 'neutral' },
  running: { label: 'Running', tone: 'warn', pill: 'info' },
  succeeded: { label: 'Succeeded', tone: 'ok', pill: 'ok' },
  failed: { label: 'Failed', tone: 'bad', pill: 'bad' },
  cancelled: { label: 'Cancelled', tone: 'neutral', pill: 'neutral' },
};

export const isActive = (r: Pick<TaskRun, 'status'> | null | undefined) => !!r && (r.status === 'queued' || r.status === 'running');

/* ---------------------------------------------------------------- filters */

export type TaskStatusFilter = 'running' | 'failed' | 'succeeded' | 'never';

export const TASK_STATUS_FILTERS: { id: TaskStatusFilter; label: string }[] = [
  { id: 'running', label: 'Running now' },
  { id: 'failed', label: 'Last run failed' },
  { id: 'succeeded', label: 'Last run succeeded' },
  { id: 'never', label: 'Never run' },
];

export function isTaskStatusFilter(v: unknown): v is TaskStatusFilter {
  return TASK_STATUS_FILTERS.some((f) => f.id === v);
}

export interface TaskFilter {
  type?: TaskType;
  status?: TaskStatusFilter;
  q?: string;
}

export function filterTasks(list: TaskList, f: TaskFilter): Task[] {
  const running = new Set(list.running.map((r) => r.task_id));
  const q = f.q?.trim().toLowerCase();
  return list.items.filter((t) => {
    if (f.type && t.type !== f.type) return false;
    if (q && !`${t.name} ${t.description ?? ''}`.toLowerCase().includes(q)) return false;
    switch (f.status) {
      case 'running':
        return running.has(t.id) || isActive(t.last_run);
      case 'failed':
        return t.last_run?.status === 'failed';
      case 'succeeded':
        return t.last_run?.status === 'succeeded';
      case 'never':
        return !t.last_run;
      default:
        return true;
    }
  });
}

/* ---------------------------------------------------------------- live updates */

/** Folds a task.progress event into the task list (pure; returns `list` when nothing changed). */
export function applyTaskProgress(list: TaskList, p: TaskProgress): TaskList {
  let hit = false;
  const running = list.running.map((r) => {
    if (r.task_id !== p.task_id || (p.run_id && r.id !== p.run_id)) return r;
    hit = true;
    // Events can arrive out of order; progress never goes backwards.
    return {
      ...r,
      status: r.status === 'queued' ? ('running' as const) : r.status,
      progress: Math.max(r.progress, p.progress),
      message: p.message ?? r.message,
    };
  });
  return hit ? { ...list, running } : list;
}

/** Folds a task.finished event: the run leaves the banner and becomes the task's last run. */
export function applyTaskFinished(list: TaskList, run: TaskRun): TaskList {
  return {
    ...list,
    running: list.running.filter((r) => r.id !== run.id),
    items: list.items.map((t) => (t.id === run.task_id ? { ...t, last_run: run } : t)),
  };
}

export function runDuration(r: Pick<TaskRun, 'started_at' | 'finished_at'>, now: Date): number | null {
  if (!r.started_at) return null;
  const end = r.finished_at ? new Date(r.finished_at) : now;
  return Math.max(0, end.getTime() - new Date(r.started_at).getTime());
}

/**
 * Rough time left from the rate so far; null until there is enough signal
 * (at least 5 % done and 5 s elapsed) to avoid wild guesses.
 */
export function estimateRemaining(r: Pick<TaskRun, 'started_at' | 'progress'>, now: Date): number | null {
  if (!r.started_at || r.progress < 0.05 || r.progress >= 1) return null;
  const elapsed = now.getTime() - new Date(r.started_at).getTime();
  if (elapsed < 5_000) return null;
  return (elapsed / r.progress) * (1 - r.progress);
}

export function remainingText(ms: number | null): string | null {
  if (ms === null) return null;
  if (ms < 60_000) return 'less than a minute left';
  return `about ${formatDuration(Math.round(ms / 60_000) * 60_000)} left`;
}

/** The cleanup policy a task belongs to ("cleanup/<policy>"); null for other tasks. */
export function taskPolicy(t: Pick<Task, 'type' | 'name' | 'policy_id'>): string | null {
  if (!t.policy_id && t.type !== 'cleanup') return null;
  return t.name.startsWith('cleanup/') ? t.name.slice('cleanup/'.length) : t.name;
}

/** Only tasks an administrator added can be deleted; built-in and policy tasks cannot. */
export const taskDeletable = (t: Pick<Task, 'builtin' | 'policy_id' | 'type'>) => !t.builtin && !t.policy_id && t.type !== 'cleanup';
