import { useBrowseFolder } from '../../api/browse';
import { ApiRequestError } from '../../api/client';
import type { BrowseEntry } from '../../api/types';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { formatBytes, relativeTime } from '../../lib/format';
import { useNow } from '../../lib/useNow';

const KIND_LABEL: Record<BrowseEntry['kind'], string> = { folder: 'Folder', package: 'Package', version: 'Version', asset: 'File' };

interface FolderDetailProps {
  repo: string;
  path: string;
  onOpen: (e: BrowseEntry) => void;
}

/** Right pane for a plain folder: what it holds. */
export function FolderDetail({ repo, path, onOpen }: FolderDetailProps) {
  const q = useBrowseFolder(repo, path);
  const now = useNow();
  const name = path.replace(/\/$/, '').split('/').pop() ?? path;

  if (q.isError) {
    const notFound = q.error instanceof ApiRequestError && q.error.status === 404;
    return (
      <Panel className="browse-empty" role="status">
        {notFound ? <p>Repository {repo} does not exist, or you cannot see it.</p> : <ErrorNote what="the folder" onRetry={() => void q.refetch()} />}
      </Panel>
    );
  }
  const items = q.data?.items ?? [];
  const group = items.some((e) => e.repositories);
  const columns: Column<BrowseEntry>[] = [
    {
      key: 'name',
      header: 'Name',
      className: 'col-first',
      cell: (e) => (
        <button type="button" className="link-button link-strong" onClick={() => onOpen(e)}>
          {e.name}
          {e.kind !== 'asset' ? '/' : ''}
        </button>
      ),
    },
    { key: 'kind', header: 'Kind', cell: (e) => <Pill tone={e.kind === 'asset' ? 'neutral' : 'info'}>{KIND_LABEL[e.kind]}</Pill> },
    { key: 'size', header: 'Size', align: 'right', className: 'muted nowrap', cell: (e) => (e.asset ? formatBytes(e.asset.size_bytes) : '') },
    ...(group ? [{ key: 'repo', header: 'Repository', className: 'muted', cell: (e: BrowseEntry) => e.repository }] : []),
    {
      key: 'updated',
      header: 'Updated',
      align: 'right',
      className: 'col-last meta',
      cell: (e) => (e.asset ? relativeTime(e.asset.updated_at, now) : ''),
    },
  ];

  return (
    <Panel className="pkg-head" aria-label={`Folder ${path}`}>
      <div className="folder-head">
        <h1 className="pkg-head__title">{name}/</h1>
        <p className="pkg-head__meta">
          <span className="facts__mono">{path}</span> in {repo}
        </p>
      </div>
      <div className="pkg-table">
        <DataTable
          caption={`Contents of ${path}`}
          columns={columns}
          rows={items}
          rowKey={(e) => e.path}
          busy={q.isFetching}
          minWidth={560}
          empty={q.isPending ? 'Loading…' : 'This folder is empty.'}
        />
        {q.data?.next_cursor && <p className="meta pkg-more">Showing the first {items.length} entries; expand the folder in the tree to page through all of them.</p>}
      </div>
    </Panel>
  );
}
