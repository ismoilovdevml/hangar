import { useQueries } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { apiFetch } from '../../api/client';
import { Panel } from '../../components/Panel';
import { StatusDot } from '../../components/Pill';
import { Time } from '../../components/Time';
import { AUDIT_CATEGORIES, describeAudit } from '../../lib/audit';
import { useNow } from '../../lib/useNow';
import { accessKeys, type AuditEventPage } from './api';

const DOT_TONE: Record<string, 'ok' | 'warn' | 'bad'> = { success: 'ok', denied: 'warn', error: 'bad' };

/** The latest access events across categories, newest first (side panel); the whole log is /audit. */
export function RecentAccessActivity() {
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
        <Link to="/audit" className="panel__link">
          Show all
        </Link>
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
                  <Time value={e.at} mode="relative" now={now} />
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
