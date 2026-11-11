/**
 * The one health model of the app. Every indicator (the sidebar card, the
 * Overview pill, the bell and To do, System status, Settings › System and the
 * Settings hub) reads `useSystemHealth()`, which is GET /system/status: the
 * server's rollup of the same checks the System status screen lists. So they
 * cannot disagree, and none of them is green before the answer arrived.
 */
import type { components } from './schema';
import type { SystemStatus } from './types';
import { useSystemStatus } from './hooks';

export type SystemCheckState = components['schemas']['SystemCheckState'];
export type HealthTone = 'ok' | 'warn' | 'bad' | 'neutral';

export interface SystemHealth {
  /** unknown while the status loads or when it could not be read. */
  state: SystemStatus['state'] | 'unknown';
  /** Green only for a loaded "ok"; grey while loading or failed. */
  tone: HealthTone;
  /** "All systems normal", "Blob storage down", "Checking status…", "Status unavailable". */
  text: string;
  /** Checks that are not healthy, worst first (signed-in users). */
  problems: SystemCheckState[];
  loading: boolean;
  error: boolean;
  /** The last answer, also while a refresh fails. */
  status?: SystemStatus;
}

const TONE: Record<SystemStatus['state'], HealthTone> = { ok: 'ok', degraded: 'warn', down: 'bad' };

/** The health of the installation from the /system/status query. */
export function systemHealth(q: { data?: SystemStatus; isError: boolean }): SystemHealth {
  const s = q.data;
  if (q.isError) return { state: 'unknown', tone: 'neutral', text: 'Status unavailable', problems: [], loading: false, error: true, status: s };
  if (!s) return { state: 'unknown', tone: 'neutral', text: 'Checking status…', problems: [], loading: true, error: false };
  return { state: s.state, tone: TONE[s.state], text: s.message, problems: s.problems ?? [], loading: false, error: false, status: s };
}

export function useSystemHealth(): SystemHealth {
  return systemHealth(useSystemStatus());
}

/** The tone of one check's level. */
export const LEVEL_TONE: Record<SystemCheckState['status'], HealthTone> = { healthy: 'ok', degraded: 'warn', down: 'bad' };
