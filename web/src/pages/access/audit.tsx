import { useInfiniteQuery, useQueries } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { buildUrl, apiFetch } from '../../api/client';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { Segmented } from '../../components/Segmented';
import { relativeTime } from '../../lib/format';
import { useNow } from '../../lib/useNow';
import { accessKeys, type AuditEvent, type AuditEventPage } from './api';
import { AUDIT_CATEGORIES, describeAudit, formatDateTime } from './model';

const OUTCOME_TONE: Record<string, 'ok' | 'warn' | 'bad'> = { success: 'ok', denied: 'warn', error: 'bad' };

export function AuditTab({ category, onCategory }: { category: string; onCategory: (c: string) => void }) {
  const cat = AUDIT_CATEGORIES.find((c) => c.id === category) ?? AUDIT_CATEGORIES[0]!;
  const now = useNow();
  const pages = useInfiniteQuery({
    queryKey: [...accessKeys.audit(cat.action, ''), 'pages'],
    queryFn: ({ pageParam, signal }) =>
      apiFetch<AuditEventPage>('/audit', { query: { action: cat.action, limit: 50, cursor: pageParam || undefined }, signal }),
    initialPageParam: '',
    getNextPageParam: (last) => last.next_cursor ?? undefined,
  });
  const rows = pages.data?.pages.flatMap((p) => p.items) ?? [];

  const columns: Column<AuditEvent>[] = [
    {
      key: 'at',
      header: 'When',
      className: 'col-first nowrap',
      cell: (e) => (
        <span className="cell-stack" title={e.at}>
          <span>{relativeTime(e.at, now)}</span>
          <span className="meta">{formatDateTime(e.at)}</span>
        </span>
      ),
    },
    { key: 'actor', header: 'Who', cell: (e) => <span className="cell-strong">{e.actor}</span> },
    {
      key: 'what',
      header: 'What',
      cell: (e) => (
        <span className="cell-stack">
          <span>{describeAudit(e.action, e.target, e.details)}</span>
          <code className="meta mono">{e.action}</code>
        </span>
      ),
    },
    {
      key: 'outcome',
      header: 'Outcome',
      className: 'nowrap',
      cell: (e) => <Pill tone={OUTCOME_TONE[e.outcome] ?? 'neutral'}>{e.outcome === 'success' ? 'Success' : e.outcome === 'denied' ? 'Denied' : 'Error'}</Pill>,
    },
    { key: 'ip', priority: 3, header: 'Address', className: 'col-last meta mono', cell: (e) => e.ip ?? '—' },
  ];

  return (
    <Panel flush aria-label="Access audit log">
      <div className="repo-toolbar">
        <Segmented
          label="Event type"
          options={AUDIT_CATEGORIES.map((c) => ({ value: c.id, label: c.label }))}
          value={cat.id}
          onChange={onCategory}
        />
        <div className="repo-toolbar__filters">
          <Link to="/audit" className="button button--secondary button--sm">
            Full audit log
          </Link>
          <a className="button button--secondary button--sm" href={buildUrl('/audit/export', { format: 'csv', action: cat.action })} download>
            Export CSV
          </a>
          <a className="button button--secondary button--sm" href={buildUrl('/audit/export', { format: 'json', action: cat.action })} download>
            Export JSON
          </a>
        </div>
      </div>
      {pages.isError ? (
        <ErrorNote what="the audit log" error={pages.error} privilege="system:audit:read" onRetry={() => void pages.refetch()} />
      ) : (
        <DataTable
          caption="Access audit log"
          columns={columns}
          rows={rows}
          rowKey={(e) => String(e.id)}
          busy={pages.isFetching}
          minWidth={820}
          empty={pages.isPending ? 'Loading…' : 'No events of this type yet.'}
        />
      )}
      <div className="repo-footer">
        <p className="meta" aria-live="polite">
          {pages.data ? `${rows.length} events` : ' '}
        </p>
        <button type="button" className="button button--secondary button--sm" disabled={!pages.hasNextPage || pages.isFetchingNextPage} onClick={() => void pages.fetchNextPage()}>
          {pages.isFetchingNextPage ? 'Loading…' : 'Load more'}
        </button>
      </div>
    </Panel>
  );
}

const DOT_TONE: Record<string, 'ok' | 'warn' | 'bad'> = { success: 'ok', denied: 'warn', error: 'bad' };

/** The latest access events across categories, newest first (side panel). */
export function RecentAccessActivity({ onShowAll }: { onShowAll: () => void }) {
  const now = useNow();
  const results = useQueries({
    queries: AUDIT_CATEGORIES.map((c) => ({
      queryKey: [...accessKeys.audit(c.action, ''), 6],
      queryFn: ({ signal }: { signal: AbortSignal }) => apiFetch<AuditEventPage>('/audit', { query: { action: c.action, limit: 6 }, signal }),
    })),
  });
  const failed = results.every((r) => r.isError);
  const events = results
    .flatMap((r) => r.data?.items ?? [])
    .sort((a, b) => b.at.localeCompare(a.at) || b.id - a.id)
    .slice(0, 6);
  return (
    <Panel
      className="side-panel"
      title="Audit log"
      titleId="recent-access-title"
      action={
        <button type="button" className="link-button panel__link" onClick={onShowAll}>
          Show all
        </button>
      }
    >
      {failed ? (
        <p className="meta">Could not load the audit log.</p>
      ) : events.length === 0 ? (
        <p className="meta">{results.some((r) => r.isPending) ? 'Loading…' : 'No access events yet.'}</p>
      ) : (
        <ul className="activity">
          {events.map((e) => (
            <li key={e.id} className="activity__row">
              <StatusDot tone={DOT_TONE[e.outcome] ?? 'neutral'} />
              <span className="cell-stack">
                <span>
                  <strong>{e.actor}</strong> {describeAudit(e.action, e.target, e.details)}
                  {e.outcome !== 'success' ? ` (${e.outcome})` : ''}
                </span>
                <span className="meta">
                  {relativeTime(e.at, now)}
                  {e.ip ? ` · ${e.ip}` : ''}
                </span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}
