import { useEffect, useId, useRef, useState } from 'react';
import { getRouteApi, Link } from '@tanstack/react-router';
import { isMissingEndpoint, useCanSystem, useTaskLiveUpdates, useTasks } from '../../api/ops';
import type { Task, TaskRun, TaskType } from '../../api/opsTypes';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { IconSearch } from '../../components/icons';
import { IconRefresh } from '../../components/opsIcons';
import { Meter } from '../../components/Meter';
import { NotAvailable } from '../../components/NotAvailable';
import { PageHeader } from '../../components/PageHeader';
import { Select } from '../../components/Select';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import {
  ALL_TASK_TYPES,
  estimateRemaining,
  filterTasks,
  formatDuration,
  formatNextRun,
  formatUtc,
  isActive,
  remainingText,
  RUN_STATUS,
  runDuration,
  TASK_STATUS_FILTERS,
  TASK_TYPES,
  taskTypeLabel,
  type TaskStatusFilter,
} from '../../lib/tasks';
import { useNow } from '../../lib/useNow';
import { TaskActions } from './TaskActions';

const route = getRouteApi('/app/tasks');

export interface TasksSearch {
  type?: TaskType;
  status?: TaskStatusFilter;
  q?: string;
}

function RunningBanner({ run, task, canRun, onMessage }: { run: TaskRun; task?: Task; canRun: boolean; onMessage: (m: string) => void }) {
  const now = useNow(5_000);
  const pct = Math.round(run.progress * 100);
  const left = remainingText(estimateRemaining(run, now));
  const name = run.task_name ?? task?.name ?? run.task_id;
  return (
    <section className="running glass" aria-label={`Running now: ${name}`}>
      <span className="running__icon" aria-hidden="true">
        <IconRefresh size={20} />
      </span>
      <div className="running__body">
        <div className="running__head">
          <p className="running__title">
            {run.status === 'queued' ? 'Queued' : 'Running now'} ·{' '}
            <Link to="/tasks/$taskId" params={{ taskId: run.task_id }} search={{ run: run.id }}>
              {name}
            </Link>
          </p>
          <p className="running__meta" aria-live="polite">
            {pct}%{left ? ` · ${left}` : ''}
          </p>
        </div>
        <Meter value={run.progress} label={`Progress of ${name}`} valueText={`${pct}%`} height={8} color="var(--chart-3)" />
        {run.message && <p className="running__message">{run.message}</p>}
      </div>
      {canRun && task && <TaskActions task={task} active={run} size="md" onMessage={onMessage} />}
    </section>
  );
}

/** Name filter: typed locally, written to the URL after a short pause. */
function NameFilter({ value, onChange }: { value: string; onChange: (q: string | undefined) => void }) {
  const [draft, setDraft] = useState(value);
  const [synced, setSynced] = useState(value);
  if (value !== synced) {
    setSynced(value);
    setDraft(value);
  }
  const commit = useRef(onChange);
  useEffect(() => {
    commit.current = onChange;
  }, [onChange]);
  useEffect(() => {
    if (draft.trim() === value) return;
    const t = setTimeout(() => commit.current(draft.trim() || undefined), 300);
    return () => clearTimeout(t);
  }, [draft, value]);
  const id = useId();
  return (
    <div className="filter-search">
      <label htmlFor={id} className="visually-hidden">
        Filter tasks by name
      </label>
      <IconSearch size={16} />
      <input
        id={id}
        className="filter-search__input"
        type="search"
        placeholder="Filter by name"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  );
}

export function TasksPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const tasks = useTasks();
  useTaskLiveUpdates();
  const canRun = useCanSystem('tasks', 'write');
  const now = useNow(30_000);
  const [message, setMessage] = useState('');
  const typeId = useId();
  const statusId = useId();

  const data = tasks.data;
  const rows = data ? filterTasks(data, search) : [];
  const runningByTask = new Map((data?.running ?? []).map((r) => [r.task_id, r]));
  const filtered = !!(search.type || search.status || search.q);

  const setFilter = (patch: Partial<TasksSearch>) => void navigate({ search: (s) => ({ ...s, ...patch }), replace: 'q' in patch });

  const columns: Column<Task>[] = [
    {
      key: 'task',
      header: 'Task',
      className: 'col-first',
      cell: (t) => (
        <div className="task-name">
          <Link to="/tasks/$taskId" params={{ taskId: t.id }} className="task-name__link">
            {t.name}
          </Link>
          {t.description && <span className="task-name__sub">{t.description}</span>}
        </div>
      ),
    },
    { key: 'type', priority: 3, header: 'Type', cell: (t) => <Pill tone="neutral">{taskTypeLabel(t.type)}</Pill> },
    {
      key: 'schedule',
      priority: 2,
      header: 'Schedule',
      cell: (t) => (
        <div className="task-schedule">
          <span>{t.enabled === false ? 'Disabled' : t.schedule_text}</span>
          {t.schedule && <code className="task-schedule__cron">{t.schedule}</code>}
        </div>
      ),
    },
    {
      key: 'last',
      header: 'Last run',
      cell: (t) => {
        const active = runningByTask.get(t.id);
        if (active) return <StatusDot tone="warn" label={`Running · ${Math.round(active.progress * 100)}%`} />;
        const r = t.last_run;
        if (!r) return <span className="muted">Never run</span>;
        const meta = RUN_STATUS[r.status];
        const d = runDuration(r, now);
        const text = [r.status === 'succeeded' ? r.message : meta.label + (r.message ? `: ${r.message}` : ''), d !== null ? formatDuration(d) : null]
          .filter(Boolean)
          .join(' · ');
        return (
          <span title={r.finished_at ? `Finished ${formatUtc(r.finished_at)}` : undefined}>
            <StatusDot tone={meta.tone} label={text || meta.label} />
          </span>
        );
      },
    },
    {
      key: 'next',
      priority: 2,
      header: 'Next run',
      className: 'muted nowrap',
      cell: (t) =>
        t.next_run_at && t.enabled !== false ? (
          <time dateTime={t.next_run_at} title={formatUtc(t.next_run_at)}>
            {formatNextRun(t.next_run_at, now)}
          </time>
        ) : (
          <NotAvailable reason={t.schedule ? 'not scheduled while disabled' : 'manual only'} />
        ),
    },
    ...(canRun
      ? [
          {
            key: 'actions',
            header: <span className="visually-hidden">Actions</span>,
            align: 'right' as const,
            className: 'col-last nowrap',
            cell: (t: Task) => <TaskActions task={t} active={runningByTask.get(t.id) ?? (isActive(t.last_run) ? t.last_run! : undefined)} onMessage={setMessage} />,
          },
        ]
      : []),
  ];

  return (
    <div className="screen">
      <PageHeader
        title="Scheduled tasks"
        description="Maintenance the registry does on its own, mostly at night. Times are in UTC."
        actions={
          canRun ? (
            <Link to="/tasks/new" search={{}} className="button button--primary">
              New task
            </Link>
          ) : undefined
        }
      />

      <p className="visually-hidden" role="status">
        {message}
      </p>

      {data?.running.map((r) => (
        <RunningBanner key={r.id} run={r} task={data.items.find((t) => t.id === r.task_id)} canRun={canRun} onMessage={setMessage} />
      ))}

      <Panel flush aria-label="Task list" className="tasks-panel">
        <div className="repo-toolbar">
          <div className="repo-toolbar__filters repo-toolbar__filters--start">
            <div className="filter-select">
              <label htmlFor={typeId} className="filter-select__label">
                Type
              </label>
              <Select<TaskType | ''>
                id={typeId}
                variant="inline"
                value={search.type ?? ''}
                options={[{ value: '', label: 'All types' }, ...ALL_TASK_TYPES.map((t) => ({ value: t, label: TASK_TYPES[t].label }))]}
                onChange={(v) => setFilter({ type: v || undefined })}
              />
            </div>
            <div className="filter-select">
              <label htmlFor={statusId} className="filter-select__label">
                Status
              </label>
              <Select
                id={statusId}
                variant="inline"
                value={search.status ?? ''}
                options={[{ value: '', label: 'Any status' }, ...TASK_STATUS_FILTERS.map((f) => ({ value: f.id, label: f.label }))]}
                onChange={(v) => setFilter({ status: TASK_STATUS_FILTERS.find((f) => f.id === v)?.id })}
              />
            </div>
            <NameFilter value={search.q ?? ''} onChange={(q) => setFilter({ q })} />
          </div>
        </div>
        {tasks.isError && isMissingEndpoint(tasks.error) ? (
          <p className="meta panel-note">This server does not report scheduled tasks yet. Upgrade Hangar to see and run them here.</p>
        ) : tasks.isError ? (
          <ErrorNote what="scheduled tasks" error={tasks.error} privilege="system:tasks:read" onRetry={() => void tasks.refetch()} />
        ) : (
          <DataTable
            caption="Scheduled tasks"
            columns={columns}
            rows={rows}
            rowKey={(t) => t.id}
            minWidth={canRun ? 980 : 860}
            busy={tasks.isFetching}
            empty={
              tasks.isPending ? (
                'Loading…'
              ) : filtered ? (
                <>
                  No tasks match these filters.{' '}
                  <Link to="/tasks" search={{}}>
                    Show all
                  </Link>
                </>
              ) : (
                'No scheduled tasks are configured.'
              )
            }
          />
        )}
        <div className="repo-footer">
          <p className="meta" aria-live="polite">
            {data ? `Showing ${rows.length} of ${data.items.length}` : ' '}
          </p>
          {!canRun && data && <p className="meta">Running and stopping tasks needs the tasks write privilege.</p>}
        </div>
      </Panel>
    </div>
  );
}
