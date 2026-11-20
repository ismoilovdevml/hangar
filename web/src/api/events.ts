import { createContext, useContext, useEffect, useRef, useState } from 'react';
import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { API_BASE } from './client';
import { browseKeys } from './browse';
import { catalogKeys } from './catalog';
import { findingKeys } from './findings';
import { queryKeys } from './hooks';
import { openEventStream, type StreamStatus } from './sse';
import type { LiveEvent, LiveEventType, Page, UpstreamHealth } from './types';

export type LiveStatus = StreamStatus;

const EVENT_TYPES: readonly LiveEventType[] = [
  'component.published',
  'component.quarantined',
  'component.deleted',
  'task.progress',
  'task.finished',
  'upstream.status',
];

const isLiveType = (t: string): t is LiveEventType => (EVENT_TYPES as readonly string[]).includes(t);

/** Query keys refetched (at most once per `delay`) after events that change them. */
type Invalidate = (key: readonly unknown[]) => void;

/**
 * Applies one live event to the query cache. Exported for tests. Component
 * events refetch what lists or counts packages (browse, package pages, search,
 * findings); `invalidate` is batched, so a burst of publishes costs one refetch
 * per screen that is open, and closed screens only go stale.
 */
export function applyLiveEvent(qc: QueryClient, event: LiveEvent, invalidate: Invalidate): void {
  switch (event.type) {
    case 'component.published':
    case 'component.deleted':
      invalidate(['metrics', 'overview']);
      invalidate(queryKeys.repositories);
      invalidate(catalogKeys.components);
      invalidate(catalogKeys.search);
      invalidate(browseKeys.all);
      invalidate(findingKeys.all);
      break;
    case 'component.quarantined':
      invalidate(['metrics', 'overview']);
      invalidate(catalogKeys.components);
      invalidate(catalogKeys.search);
      invalidate(findingKeys.all);
      break;
    case 'task.progress':
    case 'task.finished':
      invalidate(['tasks']);
      break;
    case 'upstream.status': {
      // The event carries the new state only: keep each row's recorded latency.
      const { name, url, state } = event.data;
      qc.setQueriesData<Page<UpstreamHealth>>({ queryKey: queryKeys.upstreams }, (prev) =>
        prev && { ...prev, items: prev.items.map((u) => (u.name === name ? { ...u, url, state } : u)) },
      );
      invalidate(queryKeys.upstreams);
      invalidate(queryKeys.repositories);
      // The upstream check is part of the system health every indicator shows.
      invalidate(queryKeys.systemStatus);
      break;
    }
  }
}

/** Collapses bursts of invalidations (a CI pipeline publishing 50 packages) into one refetch per key. */
function batchedInvalidate(qc: QueryClient, delayMs: number) {
  const pending = new Map<string, readonly unknown[]>();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const flush = () => {
    timer = undefined;
    for (const key of pending.values()) void qc.invalidateQueries({ queryKey: key });
    pending.clear();
  };
  return {
    invalidate: (key: readonly unknown[]) => {
      pending.set(JSON.stringify(key), key);
      timer ??= setTimeout(flush, delayMs);
    },
    cancel: () => clearTimeout(timer),
  };
}

type LiveListener = (event: LiveEvent) => void;
const listeners = new Set<LiveListener>();

/**
 * Lets a screen react to live events directly (task progress bars, run logs)
 * instead of waiting for the batched refetch. Returns the unsubscribe function.
 */
export function subscribeLiveEvents(listener: LiveListener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export const LiveStatusContext = createContext<LiveStatus>('connecting');

export function useLiveStatus(): LiveStatus {
  return useContext(LiveStatusContext);
}

/**
 * Subscribes to /api/v1/events and folds events into the TanStack Query cache.
 * Reconnects with backoff and resumes with Last-Event-ID; a 401 hands over to
 * `onUnauthorized` (sign-in again).
 */
export function useLiveEvents(onUnauthorized: () => void, enabled = true): LiveStatus {
  const qc = useQueryClient();
  const [status, setStatus] = useState<LiveStatus>('connecting');
  const unauthorized = useRef(onUnauthorized);
  useEffect(() => {
    unauthorized.current = onUnauthorized;
  }, [onUnauthorized]);

  useEffect(() => {
    if (!enabled) return;
    const batch = batchedInvalidate(qc, 2_000);
    const close = openEventStream({
      url: `${API_BASE}/events?types=${EVENT_TYPES.join(',')}`,
      onStatus: setStatus,
      onUnauthorized: () => unauthorized.current(),
      onMessage: (m) => {
        if (!isLiveType(m.event)) return;
        let event: LiveEvent;
        try {
          event = { type: m.event, data: JSON.parse(m.data) } as LiveEvent;
          applyLiveEvent(qc, event, batch.invalidate);
        } catch {
          // A malformed frame is skipped; the next refetch reconciles state.
          return;
        }
        for (const l of listeners) {
          try {
            l(event);
          } catch {
            // One screen's bug must not stop the stream for the others.
          }
        }
      },
    });
    return () => {
      close();
      batch.cancel();
    };
  }, [enabled, qc]);

  return status;
}
