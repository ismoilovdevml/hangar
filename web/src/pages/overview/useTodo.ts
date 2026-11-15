import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useSystemHealth } from '../../api/health';
import { useBlobStores, useOverview, useSession, useUpstreamHealth } from '../../api/hooks';
import { fetchTaskList, opsKeys, useCanSystem } from '../../api/ops';
import type { OverviewRange } from '../../api/types';
import { useDismissed } from '../../lib/dismissed';
import { todoItems, type TodoItem } from '../../lib/overview';
import { toast } from '../../lib/toast';
import { useNow } from '../../lib/useNow';

/** Scheduled tasks for the backup reminder, only for users who may read them. */
function useTasksIfAllowed() {
  const canRead = useCanSystem('tasks', 'read');
  return useQuery({
    queryKey: opsKeys.taskList,
    queryFn: ({ signal }) => fetchTaskList(signal),
    enabled: canRead,
  });
}

export interface Todo {
  /** Undefined until the inputs loaded (or when they failed: see error). */
  items?: TodoItem[];
  error: boolean;
  retry: () => void;
  /** Hides a reminder in this browser, with an Undo toast. */
  dismiss: (id: string) => void;
  /** Dismissed reminders that would show otherwise. */
  hidden: string[];
  /** Shows the hidden reminders again. */
  showHidden: () => void;
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
  // With the stores known, the quota reminder names the ones without a quota.
  const stores = useBlobStores(useCanSystem('blobstores', 'read')).data?.items;
  const now = useNow(60_000);
  const [dismissed, dismissId, restore] = useDismissed();
  const m = overview.data;
  const sys = health.status;

  const all = useMemo(
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
        blobStores: stores,
        now,
      }),
    [m, upstreams.data, sys, session.data, tasks.data, canAdminBlobStores, stores, now],
  );
  const items = all && all.filter((t) => !t.dismissible || !dismissed.has(t.id));
  const hidden = all ? all.filter((t) => t.dismissible && dismissed.has(t.id)).map((t) => t.id) : [];
  const dismiss = (id: string) => {
    dismissId(id);
    toast('Reminder hidden in this browser.', { label: 'Undo', run: () => restore([id]) });
  };
  return { items: items || undefined, error: overview.isError, retry: () => void overview.refetch(), dismiss, hidden, showHidden: () => restore(hidden) };
}
