/**
 * Types for the operations screens (cleanup policies, scheduled tasks, audit
 * log, system). Most alias api/openapi.yaml; the ones marked "not in the
 * contract yet" follow docs/PRODUCT_SPEC.md and are served by the mocks until
 * the server implements them.
 */
import type { components, operations } from './schema';
import type { SystemStatus } from './types';

type Schemas = components['schemas'];

/* ---------- cleanup ---------- */

export type CleanupCriteria = Schemas['CleanupCriteria'];
export type CleanupPolicyInput = Schemas['CleanupPolicyInput'];
export type CleanupPolicy = Schemas['CleanupPolicy'];
export type CleanupPreview = Schemas['CleanupPreview'];
export type CleanupCandidate = Schemas['CleanupCandidate'];

/**
 * One execution of a policy (not in the contract yet:
 * `POST /cleanup-policies/{name}/run` answers it, `GET /cleanup-policies/{name}/runs`
 * lists them newest first). Counts are filled in when the run finishes.
 */
export type CleanupRun = TaskRun & {
  components_deleted?: number | null;
  bytes_freed?: number | null;
};

/* ---------- tasks ---------- */

export type TaskType = Schemas['TaskType'];
export type TaskRunStatus = Schemas['TaskRunStatus'];
export type TaskRun = Schemas['TaskRun'];
export type Task = Schemas['Task'];
export type TaskList = Schemas['TaskList'];
export type TaskRunPage = Schemas['TaskRunPage'];

type TaskProgressEvent = Extract<Schemas['LiveEvent'], { type: 'task.progress' }>;
export type TaskProgress = TaskProgressEvent['data'];

/** One line of a run's log (not in the contract yet: `GET /tasks/{id}/runs/{run_id}/log`). */
export interface TaskLogLine {
  /** Monotonic per run; pass the last one as `after` to fetch only newer lines. */
  seq: number;
  at: string;
  level: 'debug' | 'info' | 'warn' | 'error';
  message: string;
}

export interface TaskLogPage {
  items: TaskLogLine[];
  /** Older lines were dropped from the server's buffer. */
  truncated?: boolean;
}

/* ---------- audit ---------- */

export type AuditEvent = Schemas['AuditEvent'];
export type AuditEventPage = Schemas['AuditEventPage'];
export type AuditOutcome = AuditEvent['outcome'];
/**
 * `outcome` is not a documented filter yet; the UI sends it and also checks
 * rows itself, so an older server cannot show the wrong outcome.
 */
export type ListAuditQuery = NonNullable<operations['listAuditEvents']['parameters']['query']> & {
  outcome?: AuditOutcome;
};

/* ---------- system ---------- */

export type BuildInfo = Schemas['BuildInfo'];
export type DependencyStatus = Schemas['DependencyStatus'];
export type ClusterNode = Schemas['Node'];
export type ClusterStatus = Schemas['ClusterStatus'];

/** System status plus the public endpoints (not in the contract yet). */
export type SystemStatusDetail = SystemStatus & {
  base_url?: string;
  registry_host?: string;
};

/**
 * Effective configuration with secrets already masked by the server (not in
 * the contract yet: `GET /system/config`, admin only). Nested objects use the
 * same snake_case keys as hangar.yaml.
 */
export interface SystemConfig {
  config: Record<string, unknown>;
  /** Where values came from, keyed by dotted path ("server.base_url": "env"). */
  sources?: Record<string, 'default' | 'file' | 'env'>;
}
