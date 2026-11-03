import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { API_BASE } from './client';
import { queryKeys, RECENT_LIMIT } from './hooks';
import type { LiveEvent, Page, PublishedComponent } from './types';

export type LiveStatus = 'connecting' | 'open' | 'closed' | 'unsupported';

const EVENT_TYPES: LiveEvent['type'][] = [
  'component.published',
  'component.quarantined',
  'task.progress',
];

/** Applies one live event to the query cache. Exported for tests. */
export function applyLiveEvent(
  qc: ReturnType<typeof useQueryClient>,
  event: LiveEvent,
): void {
  switch (event.type) {
    case 'component.published':
      qc.setQueryData<Page<PublishedComponent>>(queryKeys.recentComponents, (prev) => {
        if (!prev) return prev;
        const rest = prev.items.filter((c) => c.id !== event.data.id);
        return { ...prev, items: [event.data, ...rest].slice(0, RECENT_LIMIT) };
      });
      void qc.invalidateQueries({ queryKey: ['metrics', 'overview'] });
      break;
    case 'component.quarantined':
      void qc.invalidateQueries({ queryKey: queryKeys.recentComponents });
      void qc.invalidateQueries({ queryKey: ['metrics', 'overview'] });
      break;
    case 'task.progress':
      void qc.invalidateQueries({ queryKey: ['tasks'] });
      break;
  }
}

/**
 * Subscribes to the server-sent event stream at /api/v1/events and folds events into
 * the TanStack Query cache. EventSource reconnects on its own after network errors.
 */
export function useLiveEvents(enabled = true): LiveStatus {
  const qc = useQueryClient();
  const supported = typeof window !== 'undefined' && 'EventSource' in window;
  const [status, setStatus] = useState<LiveStatus>(supported ? 'connecting' : 'unsupported');

  useEffect(() => {
    if (!enabled || !supported) return;
    const source = new EventSource(`${API_BASE}/events`, { withCredentials: true });
    const onOpen = () => setStatus('open');
    const onError = () =>
      setStatus(source.readyState === EventSource.CLOSED ? 'closed' : 'connecting');
    const handlers = EVENT_TYPES.map((type) => {
      const handler = (e: MessageEvent<string>) => {
        try {
          applyLiveEvent(qc, { type, data: JSON.parse(e.data) } as LiveEvent);
        } catch {
          // Ignore malformed frames; the next refetch reconciles state.
        }
      };
      source.addEventListener(type, handler);
      return [type, handler] as const;
    });
    source.addEventListener('open', onOpen);
    source.addEventListener('error', onError);
    return () => {
      handlers.forEach(([type, h]) => source.removeEventListener(type, h));
      source.removeEventListener('open', onOpen);
      source.removeEventListener('error', onError);
      source.close();
    };
  }, [enabled, supported, qc]);

  return status;
}
