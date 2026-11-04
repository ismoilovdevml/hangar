import type { Task, TaskList, TaskProgress, TaskRun, TaskRunStatus, TaskType } from '../api/opsTypes';
import type { Tone } from './format';

export const TASK_TYPES: Record<TaskType, { label: string; hint: string }> = {
  cleanup: { label: 'Cleanup', hint: 'Applies a cleanup policy' },
  blob_compaction: { label: 'Blob compaction', hint: 'Frees space held by soft-deleted blobs' },
  docker_gc: { label: 'Docker GC', hint: 'Removes unreferenced layers' },
  auth_purge: { label: 'Expired sessions purge', hint: 'Deletes expired sessions and session tokens' },
  audit_partitions: { label: 'Audit partitions', hint: 'Creates the next monthly audit log partitions' },
  rescan: { label: 'Rescan', hint: 'Scans packages against the latest advisories' },
  reindex: { label: 'Reindex search', hint: 'Rebuilds the search index' },
  directory_sync: { label: 'Directory sync', hint: 'Syncs users and groups from the directory' },
  db_backup: { label: 'Database backup', hint: 'Backs up PostgreSQL' },
  replication: { label: 'Replication', hint: 'Copies repositories to other sites' },
  proxy_metadata_refresh: { label: 'Proxy metadata refresh', hint: 'Refreshes cached upstream metadata' },
  blob_store_move: { label: 'Blob store move', hint: 'Moves repositories between blob stores' },
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

/* ---------------------------------------------------------------- time */

/** 75_000 → "1 min 15 s", 4_000 → "4 s", 3_900_000 → "1 h 5 min". */
export function formatDuration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 && m < 10 ? `${m} min ${s % 60} s` : `${m} min`;
  const h = Math.floor(m / 60);
  return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
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

const DAY = 86_400_000;
const utcDay = (d: Date) => Math.floor(d.getTime() / DAY);
const pad = (n: number) => String(n).padStart(2, '0');
const utcClock = (d: Date) => `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
const WEEKDAY = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MONTH = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

/** UTC wall time in a compact form: "14 Mar 2026, 09:52 UTC". */
export function formatUtc(iso: string, opts: { seconds?: boolean } = {}): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const time = opts.seconds ? `${utcClock(d)}:${pad(d.getUTCSeconds())}` : utcClock(d);
  return `${d.getUTCDate()} ${MONTH[d.getUTCMonth()]} ${d.getUTCFullYear()}, ${time} UTC`;
}

/** When a scheduled run happens, relative to now: "In 9 min", "Today 02:00", "Sun 03:00", "12 Nov 03:00" (UTC). */
export function formatNextRun(iso: string, now: Date): string {
  const d = new Date(iso);
  const diff = d.getTime() - now.getTime();
  if (Number.isNaN(diff)) return iso;
  if (diff <= 60_000) return 'Due now';
  if (diff < 60 * 60_000) return `In ${Math.round(diff / 60_000)} min`;
  const days = utcDay(d) - utcDay(now);
  if (days === 0) return `Today ${utcClock(d)}`;
  if (days === 1) return `Tomorrow ${utcClock(d)}`;
  if (days < 7) return `${WEEKDAY[d.getUTCDay()]} ${utcClock(d)}`;
  return `${d.getUTCDate()} ${MONTH[d.getUTCMonth()]} ${utcClock(d)}`;
}
