/** Data access for the System status and System logs screens. */
import { useCallback, useEffect, useReducer, useRef } from 'react';
import { useQuery } from '@tanstack/react-query';
import { ApiRequestError, apiFetch } from '../../api/client';
import { appendRecords, type LogLevel, type LogPage, type LogRecord, type SystemNodeList } from './model';

export const STATUS_REFRESH_MS = 15_000;
export const LOGS_REFRESH_MS = 5_000;

export const systemKeys = {
  nodes: ['system', 'nodes'] as const,
};

/** Every node with its subsystems; polled every 15 s unless paused. */
export function useSystemNodes(paused: boolean, enabled = true) {
  return useQuery({
    queryKey: systemKeys.nodes,
    enabled,
    queryFn: ({ signal }) => apiFetch<SystemNodeList>('/system/nodes', { signal }),
    retry: false,
    refetchInterval: paused ? false : STATUS_REFRESH_MS,
    refetchIntervalInBackground: false,
  });
}

export interface LogFilters {
  node?: string;
  level?: LogLevel;
  q?: string;
}

function logQuery(f: LogFilters, extra: Record<string, string | number | undefined>) {
  return { node: f.node, level: f.level, q: f.q, ...extra };
}

/** First page size and follow page size. */
const TAIL_LIMIT = 1000;
const FOLLOW_LIMIT = 1000;
/** Follow pages fetched in one refresh before waiting for the next. */
const MAX_FOLLOW_PAGES = 5;

interface StreamState {
  records: LogRecord[];
  cursor?: string;
  status: 'loading' | 'ready' | 'error';
  error?: unknown;
  /** Records were overwritten before this screen read them. */
  gap: boolean;
  buffer?: LogPage['buffer'];
  node?: string;
  updatedAt?: number;
  fetching: boolean;
}

type Action =
  | { type: 'reset' }
  | { type: 'fetching' }
  | { type: 'page'; page: LogPage; replace: boolean }
  | { type: 'error'; error: unknown };

function reducer(s: StreamState, a: Action): StreamState {
  switch (a.type) {
    case 'reset':
      return { records: [], status: 'loading', gap: false, fetching: true };
    case 'fetching':
      return { ...s, fetching: true };
    case 'page':
      return {
        ...s,
        records: a.replace ? a.page.items : appendRecords(s.records, a.page.items),
        cursor: a.page.cursor,
        status: 'ready',
        error: undefined,
        gap: a.replace ? false : s.gap || a.page.gap,
        buffer: a.page.buffer,
        node: a.page.node,
        updatedAt: Date.now(),
        fetching: false,
      };
    case 'error':
      return { ...s, status: s.status === 'ready' ? 'ready' : 'error', error: a.error, fetching: false, updatedAt: Date.now() };
  }
}

/**
 * The records of one node's log buffer: the newest page first, then only
 * newer records every 5 s (follow) until paused. Changing a filter starts
 * over. `refresh` fetches at once.
 */
export function useLogStream(filters: LogFilters, paused: boolean) {
  const [state, dispatch] = useReducer(reducer, { records: [], status: 'loading', gap: false, fetching: true });
  const cursorRef = useRef<string | undefined>(undefined);
  const inflight = useRef<AbortController | null>(null);
  const key = JSON.stringify([filters.node ?? '', filters.level ?? '', filters.q ?? '']);

  const load = useCallback(
    async (initial: boolean) => {
      inflight.current?.abort();
      const ctrl = new AbortController();
      inflight.current = ctrl;
      dispatch(initial ? { type: 'reset' } : { type: 'fetching' });
      try {
        if (initial || !cursorRef.current) {
          const page = await apiFetch<LogPage>('/system/logs', { query: logQuery(filters, { limit: TAIL_LIMIT }), signal: ctrl.signal });
          cursorRef.current = page.cursor;
          dispatch({ type: 'page', page, replace: true });
          return;
        }
        for (let i = 0; i < MAX_FOLLOW_PAGES; i++) {
          const page: LogPage = await apiFetch<LogPage>('/system/logs', {
            query: logQuery(filters, { limit: FOLLOW_LIMIT, cursor: cursorRef.current }),
            signal: ctrl.signal,
          });
          cursorRef.current = page.cursor;
          dispatch({ type: 'page', page, replace: false });
          if (!page.more) break;
        }
      } catch (err) {
        if (ctrl.signal.aborted) return;
        dispatch({ type: 'error', error: err });
      }
    },
    // The filters are captured through `key`.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key],
  );

  // Start over whenever the filters change.
  useEffect(() => {
    cursorRef.current = undefined;
    void load(true);
    return () => inflight.current?.abort();
  }, [load]);

  // Follow: the next refresh is due 5 s after the last answer.
  useEffect(() => {
    if (paused || state.fetching) return;
    const id = setTimeout(() => void load(false), LOGS_REFRESH_MS);
    return () => clearTimeout(id);
  }, [paused, load, state.updatedAt, state.fetching]);

  const refresh = useCallback(() => void load(false), [load]);
  return { ...state, refresh };
}

/**
 * The whole buffer of a node, oldest first, with the current filters: pages
 * through it from the start ("cursor=0") until the server has no more.
 */
export async function fetchWholeBuffer(filters: LogFilters, signal?: AbortSignal): Promise<LogRecord[]> {
  const out: LogRecord[] = [];
  let cursor = '0';
  for (let i = 0; i < 50; i++) {
    const page: LogPage = await apiFetch<LogPage>('/system/logs', { query: logQuery(filters, { limit: 5000, cursor }), signal });
    out.push(...page.items);
    cursor = page.cursor;
    if (!page.more) break;
  }
  return out;
}

/** The server has no such endpoint (an older build), as opposed to an unknown node. */
export function isOlderServer(err: unknown): boolean {
  return err instanceof ApiRequestError && (err.status === 501 || (err.status === 404 && err.code !== 'not_found'));
}
