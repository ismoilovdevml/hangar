import { createContext, useContext, useEffect, useRef, useState } from 'react';
import { useQueryClient, type QueryClient } from '@tanstack/react-query';
import { API_BASE } from './client';
import { queryKeys, RECENT_LIMIT } from './hooks';
import { openEventStream, type StreamStatus } from './sse';
import type { LiveEvent, LiveEventType, Page, PublishedComponent, UpstreamHealth } from './types';

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

/** Applies one live event to the query cache. Exported for tests. */
export function applyLiveEvent(qc: QueryClient, event: LiveEvent, invalidate: Invalidate): void {
  const recent = queryKeys.recentComponents;
  switch (event.type) {
    case 'component.published':
      qc.setQueryData<Page<PublishedComponent>>(recent, (prev) => {
        if (!prev) return prev;
        const rest = prev.items.filter((c) => c.id !== event.data.id);
        return { ...prev, items: [event.data, ...rest].slice(0, RECENT_LIMIT) };
      });
      invalidate(['metrics', 'overview']);
      invalidate(queryKeys.repositories);
      break;
    case 'component.quarantined':
      qc.setQueryData<Page<PublishedComponent>>(recent, (prev) =>
        prev && {
          ...prev,
          items: prev.items.map((c) => (c.id === event.data.id ? { ...c, scan: { ...c.scan, state: 'quarantined' } } : c)),
        },
      );
      invalidate(['metrics', 'overview']);
      break;
    case 'component.deleted':
      qc.setQueryData<Page<PublishedComponent>>(recent, (prev) =>
        prev && { ...prev, items: prev.items.filter((c) => c.id !== event.data.id) },
      );
      invalidate(recent);
      invalidate(['metrics', 'overview']);
      invalidate(queryKeys.repositories);
      break;
    case 'task.progress':
    case 'task.finished':
      invalidate(['tasks']);
      break;
    case 'upstream.status':
      qc.setQueryData<Page<UpstreamHealth>>(queryKeys.upstreams, (prev) => {
        if (!prev) return prev;
        const known = prev.items.some((u) => u.name === event.data.name);
        return {
          ...prev,
          items: known ? prev.items.map((u) => (u.name === event.data.name ? event.data : u)) : [...prev.items, event.data],
        };
      });
      invalidate(queryKeys.repositories);
      break;
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
