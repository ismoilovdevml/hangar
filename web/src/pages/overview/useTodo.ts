import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { apiFetch } from '../../api/client';
import { useSystemHealth } from '../../api/health';
import { useOverview, useSession, useUpstreamHealth } from '../../api/hooks';
import { opsKeys, useCanSystem } from '../../api/ops';
import type { TaskList } from '../../api/opsTypes';
import type { OverviewRange } from '../../api/types';
import { useDismissed } from '../../lib/dismissed';
import { todoItems, type TodoItem } from '../../lib/overview';
import { useNow } from '../../lib/useNow';

/** Scheduled tasks for the backup reminder, only for users who may read them. */
function useTasksIfAllowed() {
  const canRead = useCanSystem('tasks', 'read');
  return useQuery({
    queryKey: opsKeys.taskList,
    queryFn: ({ signal }) => apiFetch<TaskList>('/tasks', { signal }),
    enabled: canRead,
  });
}

export interface Todo {
  /** Undefined until the inputs loaded (or when they failed: see error). */
  items?: TodoItem[];
  error: boolean;
  retry: () => void;
  dismiss: (id: string) => void;
}

/**
 * What needs someone's attention: the Overview's To do card and the bell
 * both show this list, so they always agree. The queries are shared with the
 * Overview's cards.
 */
export function useTodo(range: OverviewRange): Todo {
  const session = useSession();
  const overview = useOverview(range);
  const upstreams = useUpstreamHealth(range);
  const health = useSystemHealth();
  const tasks = useTasksIfAllowed();
  const canAdminBlobStores = useCanSystem('blobstores', 'admin');
  const now = useNow(60_000);
  const [dismissed, dismiss] = useDismissed();
  const m = overview.data;
  const sys = health.status;

  const items = useMemo(
    () =>
      m &&
      todoItems({
        metrics: m,
        upstreams: upstreams.data?.items,
        system: sys,
        tokenWarnings: session.data?.token_warnings,
        username: session.data?.user.username,
        tasks: tasks.data?.items,
        canAdminBlobStores,
        now,
      }).filter((t) => !t.dismissible || !dismissed.has(t.id)),
    [m, upstreams.data, sys, session.data, tasks.data, canAdminBlobStores, now, dismissed],
  );
  return { items: items || undefined, error: overview.isError, retry: () => void overview.refetch(), dismiss };
}
