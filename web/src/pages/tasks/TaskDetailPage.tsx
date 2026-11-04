import { useEffect, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { getRouteApi, Link, useRouterState } from '@tanstack/react-router';
import { subscribeLiveEvents } from '../../api/events';
import { isMissingEndpoint, opsKeys, useCanSystem, useTask, useTaskLiveUpdates, useTaskRunLog, useTaskRuns, useTasks } from '../../api/ops';
import type { TaskRun } from '../../api/opsTypes';
import { ApiRequestError } from '../../api/client';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { Meter } from '../../components/Meter';
import { NotAvailable } from '../../components/NotAvailable';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import {
  estimateRemaining,
  formatDuration,
  formatNextRun,
  formatUtc,
  isActive,
  remainingText,
  RUN_STATUS,
  runDuration,
  taskTypeLabel,
} from '../../lib/tasks';
import { useNow } from '../../lib/useNow';
import { TaskActions } from './TaskActions';

const route = getRouteApi('/app/tasks/$taskId');

export interface TaskDetailSearch {
  run?: string;
  cursor?: string;
}

function RunLog({ taskId, run }: { taskId: string; run: TaskRun }) {
  const active = isActive(run);
  const log = useTaskRunLog(taskId, run.id, active);
  const listRef = useRef<HTMLOListElement>(null);
  const [follow, setFollow] = useState(true);
  const lines = log.data?.items ?? [];

  // Keep the newest line in view while following a live run.
  useEffect(() => {
    const el = listRef.current;
    if (el && follow) el.scrollTop = el.scrollHeight;
  }, [lines.length, follow]);

  if (log.isError) {
    return isMissingEndpoint(log.error) ? (
      <p className="meta">This server does not keep run logs yet. The progress message above is the latest the run reported.</p>
    ) : (
      <ErrorNote what="the run log" onRetry={() => void log.refetch()} />
    );
  }
  return (
    <div className="run-log">
      <div className="run-log__head">
        <h3 className="section-label">Log</h3>
        {active && (
          <label className="checkbox">
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow
          </label>
        )}
      </div>
      {log.data?.truncated && <p className="meta">Older lines were dropped; only the latest are kept.</p>}
      <ol
        ref={listRef}
        className="run-log__lines"
        tabIndex={0}
        aria-label={`Log of run ${run.id}`}
        aria-live={active ? 'polite' : undefined}
        aria-busy={log.isPending || undefined}
      >
        {log.isPending && <li className="meta">Loading…</li>}
        {!log.isPending && lines.length === 0 && <li className="meta">No log lines yet.</li>}
        {lines.map((l) => (
          <li key={l.seq} className={`run-log__line is-${l.level}`}>
            <time dateTime={l.at}>{l.at.slice(11, 19)}</time>
            <span className="run-log__level">{l.level}</span>
            <span className="run-log__msg">{l.message}</span>
          </li>
        ))}
      </ol>
    </div>
  );
}

function RunDetail({ taskId, run }: { taskId: string; run: TaskRun }) {
  const now = useNow(isActive(run) ? 1_000 : 30_000);
  const meta = RUN_STATUS[run.status];
  const pct = Math.round(run.progress * 100);
  const d = runDuration(run, now);
  const left = isActive(run) ? remainingText(estimateRemaining(run, now)) : null;
  return (
    <Panel title={`Run ${run.id}`} titleId="run-detail-title" action={<Pill tone={meta.pill}>{meta.label}</Pill>} className="run-detail">
      <div className="run-detail__progress">
        <Meter value={run.progress} label="Run progress" valueText={`${pct}%`} height={8} color={run.status === 'failed' ? 'var(--bad)' : 'var(--chart-3)'} />
        <p className="meta" aria-live="polite">
          {pct}%{left ? ` · ${left}` : ''}
          {run.message ? ` · ${run.message}` : ''}
        </p>
      </div>
      <dl className="facts">
        <div>
          <dt>Started</dt>
          <dd>{run.started_at ? formatUtc(run.started_at, { seconds: true }) : 'Not started'}</dd>
        </div>
        <div>
          <dt>Finished</dt>
          <dd>{run.finished_at ? formatUtc(run.finished_at, { seconds: true }) : isActive(run) ? 'Still running' : <NotAvailable />}</dd>
        </div>
        <div>
          <dt>Duration</dt>
          <dd>{d === null ? <NotAvailable reason="not started" /> : formatDuration(d)}</dd>
        </div>
        <div>
          <dt>Triggered by</dt>
          <dd>{run.triggered_by ?? <NotAvailable />}</dd>
        </div>
        <div>
          <dt>Node</dt>
          <dd>{run.node ?? <NotAvailable />}</dd>
        </div>
      </dl>
      <RunLog taskId={taskId} run={run} />
    </Panel>
  );
}

export function TaskDetailPage() {
  const { taskId } = route.useParams();
  const { run: runId, cursor } = route.useSearch();
  const navigate = route.useNavigate();
  const pageCursors = useRouterState({ select: (s) => s.location.state.pageCursors });
  const qc = useQueryClient();
  const task = useTask(taskId);
  const runs = useTaskRuns(taskId, cursor);
  const list = useTasks();
  useTaskLiveUpdates();
  const canRun = useCanSystem('tasks', 'write');
  const now = useNow(30_000);
  const [message, setMessage] = useState('');

  const items = runs.data?.items ?? [];
  const selected = items.find((r) => r.id === runId) ?? (runId ? undefined : items[0]);
  const running = list.data?.running.find((r) => r.task_id === taskId);

  // A progress event for the shown run: fetch its new log lines right away.
  useEffect(
    () =>
      subscribeLiveEvents((e) => {
        if ((e.type === 'task.progress' && e.data.task_id === taskId) || (e.type === 'task.finished' && e.data.task_id === taskId)) {
          void qc.invalidateQueries({ queryKey: ['tasks', 'log', taskId] });
          if (e.type === 'task.finished') void qc.invalidateQueries({ queryKey: opsKeys.task(taskId) });
        }
      }),
    [qc, taskId],
  );

  // A run started from this page appears at the top of the first page.
  // (Once per run, so a server that lists runs late cannot cause a refetch loop.)
  const firstPage = runs.data;
  const refreshedFor = useRef<string | null>(null);
  useEffect(() => {
    if (!running || cursor || !firstPage || refreshedFor.current === running.id) return;
    if (firstPage.items.some((r) => r.id === running.id)) return;
    refreshedFor.current = running.id;
    void qc.invalidateQueries({ queryKey: ['tasks', 'runs', taskId] });
  }, [running, cursor, firstPage, qc, taskId]);

  if (task.isError) {
    const notFound = task.error instanceof ApiRequestError && task.error.status === 404;
    return (
      <div className="screen">
        <PageHeader title={notFound ? 'Task not found' : 'Task'} description={<Link to="/tasks">All scheduled tasks</Link>} />
        {notFound ? (
          <Panel>
            <p>There is no task with id {taskId}. It may have been deleted.</p>
          </Panel>
        ) : (
          <ErrorNote what="this task" onRetry={() => void task.refetch()} />
        )}
      </div>
    );
  }

  const t = task.data;
  const stack = cursor ? (pageCursors ?? []) : [];
  const goNext = () => {
    if (!runs.data?.next_cursor) return;
    void navigate({ search: (s) => ({ ...s, cursor: runs.data?.next_cursor ?? undefined, run: undefined }), state: { pageCursors: [...stack, cursor ?? ''] } });
  };
  const goPrev = () => {
    const prev = stack.length ? stack[stack.length - 1] : '';
    void navigate({ search: (s) => ({ ...s, cursor: prev || undefined, run: undefined }), state: { pageCursors: stack.slice(0, -1) } });
  };

  const columns: Column<TaskRun>[] = [
    {
      key: 'started',
      header: 'Started',
      className: 'col-first nowrap',
      cell: (r) => (
        <Link to="/tasks/$taskId" params={{ taskId }} search={(s) => ({ ...s, run: r.id })} aria-current={selected?.id === r.id ? 'true' : undefined} className="run-link">
          {formatUtc(r.started_at ?? r.created_at)}
        </Link>
      ),
    },
    { key: 'status', header: 'Status', cell: (r) => <Pill tone={RUN_STATUS[r.status].pill}>{RUN_STATUS[r.status].label}</Pill> },
    {
      key: 'duration',
      header: 'Duration',
      className: 'muted nowrap',
      cell: (r) => {
        const d = runDuration(r, now);
        return d === null ? <NotAvailable reason="not started" /> : formatDuration(d);
      },
    },
    { key: 'by', header: 'Triggered by', className: 'muted', cell: (r) => r.triggered_by ?? <NotAvailable /> },
    { key: 'message', header: 'Result', className: 'muted col-last', cell: (r) => r.message ?? '' },
  ];

  return (
    <div className="screen">
      <PageHeader
        eyebrow={
          <>
            <Link to="/tasks">Scheduled tasks</Link> · {t ? taskTypeLabel(t.type) : '…'}
          </>
        }
        title={t?.name ?? 'Loading task…'}
        description={t?.description}
        actions={canRun && t ? <TaskActions task={t} active={running} size="md" onMessage={setMessage} /> : undefined}
      />
      <p className="visually-hidden" role="status">
        {message}
      </p>

      {t && (
        <Panel aria-label="Task settings">
          <dl className="facts">
            <div>
              <dt>Schedule</dt>
              <dd>
                {t.schedule_text}
                {t.schedule && <code className="task-schedule__cron">{t.schedule}</code>}
              </dd>
            </div>
            <div>
              <dt>Next run</dt>
              <dd>{t.next_run_at && t.enabled !== false ? `${formatNextRun(t.next_run_at, now)} (${formatUtc(t.next_run_at)})` : 'Not scheduled'}</dd>
            </div>
            <div>
              <dt>State</dt>
              <dd>{t.enabled === false ? 'Disabled' : 'Enabled'}</dd>
            </div>
            <div>
              <dt>Type</dt>
              <dd>{taskTypeLabel(t.type)}</dd>
            </div>
          </dl>
        </Panel>
      )}

      <div className="task-detail">
        <Panel flush title="Runs" titleId="task-runs-title" className="task-detail__runs">
          {runs.isError ? (
            <ErrorNote what="runs" onRetry={() => void runs.refetch()} />
          ) : (
            <DataTable
              caption="Runs, newest first"
              columns={columns}
              rows={items}
              rowKey={(r) => r.id}
              minWidth={620}
              busy={runs.isFetching}
              empty={runs.isPending ? 'Loading…' : 'This task has not run yet.'}
            />
          )}
          <div className="repo-footer">
            <p className="meta">Newest first</p>
            <div className="repo-footer__pager">
              <button type="button" className="button button--secondary button--sm" onClick={goPrev} disabled={!cursor}>
                Previous
              </button>
              <button type="button" className="button button--secondary button--sm" onClick={goNext} disabled={!runs.data?.next_cursor}>
                Next
              </button>
            </div>
          </div>
        </Panel>
        {selected ? (
          <RunDetail key={selected.id} taskId={taskId} run={selected} />
        ) : (
          runs.isSuccess &&
          runId && (
            <Panel>
              <p className="meta">Run {runId} is not on this page.</p>
            </Panel>
          )
        )}
      </div>
    </div>
  );
}
