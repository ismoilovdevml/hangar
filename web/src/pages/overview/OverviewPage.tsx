import { getRouteApi } from '@tanstack/react-router';
import { useOverview, useUpstreamHealth } from '../../api/hooks';
import { useNow } from '../../lib/useNow';
import { OutcomeCard, SecurityCard, StorageCard, UpstreamCard } from './Cards';
import { Hero } from './Hero';
import { MostDownloaded } from './MostDownloaded';
import { TodayCard } from './TodayCard';
import { TodoCard } from './TodoCard';
import { useTodo } from './useTodo';

const route = getRouteApi('/app/');

export function OverviewPage() {
  const { range = 'today' } = route.useSearch();
  const navigate = route.useNavigate();
  const overview = useOverview(range);
  const upstreams = useUpstreamHealth(range);
  const todo = useTodo(range);
  const now = useNow(60_000);
  const m = overview.data;
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
        <TodoCard items={todo.items} error={todo.error} onRetry={todo.retry} onDismiss={todo.dismiss} />
        <StorageCard m={m} error={overview.isError} />
        <SecurityCard m={m} error={overview.isError} />
        <UpstreamCard items={upstreams.data?.items} error={upstreams.isError} onRetry={() => void upstreams.refetch()} m={m} />
        <MostDownloaded />
      </div>
    </div>
  );
}
