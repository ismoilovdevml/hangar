import { Link } from '@tanstack/react-router';
import { useLiveStatus, type LiveStatus } from '../../api/events';
import { useRecentComponents } from '../../api/hooks';
import type { PublishedComponent } from '../../api/types';
import { DataTable, type Column } from '../../components/DataTable';
import { FormatBadge } from '../../components/FormatBadge';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { ErrorNote } from '../../components/ErrorNote';
import { relativeTime } from '../../lib/format';
import { componentName, scanPill } from '../../lib/overview';
import { useNow } from '../../lib/useNow';

const LIVE: Record<LiveStatus, { text: string; tone: 'ok' | 'warn' | 'neutral' }> = {
  open: { text: 'Live', tone: 'ok' },
  connecting: { text: 'Connecting…', tone: 'neutral' },
  reconnecting: { text: 'Reconnecting…', tone: 'warn' },
  closed: { text: 'Not live', tone: 'neutral' },
  unavailable: { text: 'Live updates off', tone: 'neutral' },
};

function LiveBadge() {
  const s = LIVE[useLiveStatus()];
  return (
    <span className="live-badge" role="status" title="Updates from the server event stream">
      <span className={`status__dot dot-${s.tone}`} aria-hidden="true" />
      {s.text}
    </span>
  );
}

export function RecentlyPublished() {
  const q = useRecentComponents();
  const now = useNow();

  const columns: Column<PublishedComponent>[] = [
    {
      key: 'pkg',
      header: 'Package',
      className: 'col-first',
      cell: (c) => (
        <Link
          to="/browse"
          search={{ repo: c.repository, name: c.name, version: c.version }}
          className="pkg-link"
        >
          <FormatBadge format={c.format} />
          <span className="pkg-link__text">
            <span className="pkg-link__name">{componentName(c)}</span>
            <span className="pkg-link__ver">{c.version}</span>
          </span>
        </Link>
      ),
    },
    { key: 'repo', header: 'Repository', cell: (c) => c.repository, className: 'muted' },
    { key: 'by', header: 'Published by', cell: (c) => c.publisher.display_name, className: 'muted' },
    {
      key: 'scan',
      header: 'Security',
      cell: (c) => {
        const p = scanPill(c.scan);
        return <Pill tone={p.tone}>{p.text}</Pill>;
      },
    },
    {
      key: 'when',
      header: 'When',
      align: 'right',
      className: 'col-last meta',
      cell: (c) => <time dateTime={c.published_at}>{relativeTime(c.published_at, now)}</time>,
    },
  ];

  return (
    <Panel
      title="Recently published"
      titleId="recent-title"
      flush
      className="recent"
      action={
        <span className="panel__actions">
          <LiveBadge />
          <Link to="/browse" className="panel__link">
            View all
          </Link>
        </span>
      }
    >
      {q.isError ? (
        <ErrorNote onRetry={() => void q.refetch()} what="recent packages" />
      ) : (
        <DataTable
          caption="Recently published packages"
          columns={columns}
          rows={q.data?.items ?? []}
          rowKey={(c) => c.id}
          busy={q.isPending}
          empty={q.isPending ? 'Loading…' : 'Nothing published yet. Push a package and it shows up here.'}
        />
      )}
    </Panel>
  );
}
