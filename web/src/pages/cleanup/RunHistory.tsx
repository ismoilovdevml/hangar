import { CLEANUP_RUNS_LIMIT, isMissingEndpoint, useCleanupRuns } from '../../api/ops';
import type { CleanupRun } from '../../api/opsTypes';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { NotAvailable } from '../../components/NotAvailable';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { formatBytes } from '../../lib/format';
import { formatDuration, formatUtc, RUN_STATUS, runDuration } from '../../lib/tasks';
import { useNow } from '../../lib/useNow';

/** The latest runs of one policy, newest first. */
export function RunHistory({ policy }: { policy: string }) {
  const runs = useCleanupRuns(policy);
  const now = useNow(10_000);

  const columns: Column<CleanupRun>[] = [
    { key: 'started', header: 'Started', className: 'col-first nowrap', cell: (r) => formatUtc(r.started_at ?? r.created_at) },
    { key: 'status', header: 'Status', cell: (r) => <Pill tone={RUN_STATUS[r.status].pill}>{RUN_STATUS[r.status].label}</Pill> },
    {
      key: 'deleted',
      priority: 2,
      header: 'Deleted',
      align: 'right',
      className: 'nowrap',
      cell: (r) =>
        typeof r.components_deleted === 'number' ? new Intl.NumberFormat('en-US').format(r.components_deleted) : <NotAvailable reason="not reported" />,
    },
    {
      key: 'freed',
      priority: 2,
      header: 'Freed',
      align: 'right',
      className: 'nowrap',
      cell: (r) => (typeof r.bytes_freed === 'number' ? formatBytes(r.bytes_freed) : <NotAvailable reason="not reported" />),
    },
    {
      key: 'duration',
      priority: 3,
      header: 'Duration',
      className: 'muted nowrap',
      cell: (r) => {
        const d = runDuration(r, now);
        return d === null ? <NotAvailable reason="not started" /> : formatDuration(d);
      },
    },
    { key: 'by', priority: 3, header: 'Triggered by', className: 'muted', cell: (r) => r.triggered_by ?? <NotAvailable /> },
    { key: 'message', priority: 2, header: 'Result', className: 'muted col-last', cell: (r) => r.message ?? '' },
  ];

  return (
    <Panel flush title="Run history" titleId="cleanup-runs-title" className="runs-panel">
      {runs.isError ? (
        isMissingEndpoint(runs.error) ? (
          <p className="meta panel-note">This server does not report cleanup run history yet. Runs also appear under Scheduled tasks.</p>
        ) : (
          <ErrorNote what="run history" error={runs.error} privilege="system:cleanup:read" onRetry={() => void runs.refetch()} />
        )
      ) : (
        <DataTable
          caption={`Latest runs of ${policy}`}
          columns={columns}
          rows={runs.data?.items ?? []}
          rowKey={(r) => r.id}
          minWidth={760}
          busy={runs.isFetching}
          empty={runs.isPending ? 'Loading…' : 'This policy has not run yet.'}
        />
      )}
      {runs.data && runs.data.items.length >= CLEANUP_RUNS_LIMIT && (
        <p className="meta panel-note">Showing the latest {CLEANUP_RUNS_LIMIT} runs.</p>
      )}
    </Panel>
  );
}
