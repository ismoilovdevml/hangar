import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { getRouteApi } from '@tanstack/react-router';
import { apiFetch } from '../../api/client';
import { useOverview, useSession, useSystemStatus, useUpstreamHealth } from '../../api/hooks';
import { opsKeys, useCanSystem } from '../../api/ops';
import type { TaskList } from '../../api/opsTypes';
import { todoItems } from '../../lib/overview';
import { useNow } from '../../lib/useNow';
import { OutcomeCard, SecurityCard, StorageCard, UpstreamCard } from './Cards';
import { Hero } from './Hero';
import { MostDownloaded } from './MostDownloaded';
import { TodayCard } from './TodayCard';
import { TodoCard } from './TodoCard';

const route = getRouteApi('/app/');

/** Scheduled tasks for the backup reminder, only for users who may read them. */
function useTasksIfAllowed() {
  const canRead = useCanSystem('tasks', 'read');
  return useQuery({
    queryKey: opsKeys.taskList,
    queryFn: ({ signal }) => apiFetch<TaskList>('/tasks', { signal }),
    enabled: canRead,
  });
}

export function OverviewPage() {
  const { range = 'today' } = route.useSearch();
  const navigate = route.useNavigate();
  const session = useSession();
  const overview = useOverview(range);
  const upstreams = useUpstreamHealth(range);
  const status = useSystemStatus();
  const tasks = useTasksIfAllowed();
  const canAdminBlobStores = useCanSystem('blobstores', 'admin');
  const now = useNow(60_000);
  const m = overview.data;

  const todo = useMemo(
    () =>
      m &&
      todoItems({
        metrics: m,
        upstreams: upstreams.data?.items,
        system: status.data,
        tokenWarnings: session.data?.token_warnings,
        username: session.data?.user.username,
        tasks: tasks.data?.items,
        canAdminBlobStores,
        now,
      }),
    [m, upstreams.data, status.data, session.data, tasks.data, canAdminBlobStores, now],
  );
  const retry = () => void overview.refetch();

  return (
    <div className="overview">
      <Hero topFormat={m?.storage_by_format.find((f) => f.bytes > 0)?.format} />

      <div className="ov-grid">
        <TodayCard
          m={m}
          range={range}
          now={now}
          error={overview.isError}
          onRetry={retry}
          onRange={(r) => void navigate({ search: (s) => ({ ...s, range: r === 'today' ? undefined : r }), replace: true })}
        />
        <OutcomeCard m={m} error={overview.isError} />
        <TodoCard items={todo || undefined} error={overview.isError} onRetry={retry} />
        <StorageCard m={m} error={overview.isError} />
        <SecurityCard m={m} error={overview.isError} />
        <UpstreamCard items={upstreams.data?.items} error={upstreams.isError} onRetry={() => void upstreams.refetch()} m={m} />
        <MostDownloaded />
      </div>
    </div>
  );
}
