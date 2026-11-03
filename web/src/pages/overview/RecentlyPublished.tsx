import { Link } from '@tanstack/react-router';
import { useRecentComponents } from '../../api/hooks';
import type { PublishedComponent } from '../../api/types';
import { DataTable, type Column } from '../../components/DataTable';
import { FormatBadge } from '../../components/FormatBadge';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { ErrorNote } from '../../components/ErrorNote';
import { relativeTime } from '../../lib/format';
import { scanPill } from '../../lib/overview';
import { useNow } from '../../lib/useNow';

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
            <span className="pkg-link__name">{c.name}</span>
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
        <Link to="/browse" className="panel__link">
          View all
        </Link>
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
