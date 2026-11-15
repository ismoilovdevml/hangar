import { useState } from 'react';
import { useQueries } from '@tanstack/react-query';
import { browseFolderQuery } from '../../api/browse';
import { NotFound } from '../../components/States';
import { ApiRequestError } from '../../api/client';
import type { BrowseEntry } from '../../api/types';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { formatBytes } from '../../lib/format';
import { Time } from '../../components/Time';
import { useNow } from '../../lib/useNow';

const KIND_LABEL: Record<BrowseEntry['kind'], string> = { folder: 'Folder', package: 'Package', version: 'Version', asset: 'File' };

interface FolderDetailProps {
  repo: string;
  /** "" for the root of the repository's stored files. */
  path: string;
  onOpen: (e: BrowseEntry) => void;
  /** Opens the parent folder (the stored files view). */
  onUp?: () => void;
}

/** Right pane for a folder of stored files: what it holds. */
export function FolderDetail({ repo, path, onOpen, onUp }: FolderDetailProps) {
  // Pages after the first that were asked for.
  const [more, setMore] = useState<string[]>([]);
  const pages = useQueries({ queries: [undefined, ...more].map((cursor) => browseFolderQuery(repo, path, cursor)) });
  const q = pages[0]!;
  const last = pages[pages.length - 1]!;
  const now = useNow();
  const name = path ? (path.replace(/\/$/, '').split('/').pop() ?? path) : `Files in ${repo}`;

  if (q.isError) {
    if (q.error instanceof ApiRequestError && q.error.status === 404) {
      return <NotFound pane kind="Repository" name={repo} back={{ to: '/browse' }} backLabel="Packages" />;
    }
    return (
      <Panel className="browse-empty" role="status">
        <ErrorNote what="the folder" error={q.error} onRetry={() => void q.refetch()} />
      </Panel>
    );
  }
  const items = pages.flatMap((p) => p.data?.items ?? []);
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
    { key: 'kind', priority: 3, header: 'Kind', cell: (e) => <Pill tone={e.kind === 'asset' ? 'neutral' : 'info'}>{KIND_LABEL[e.kind]}</Pill> },
    { key: 'size', priority: 2, header: 'Size', align: 'right', className: 'muted nowrap', cell: (e) => (e.asset ? formatBytes(e.asset.size_bytes) : '') },
    ...(group ? [{ key: 'repo', priority: 3 as const, header: 'Repository', className: 'muted', cell: (e: BrowseEntry) => e.repository }] : []),
    {
      key: 'updated',
      priority: 2,
      header: 'Updated',
      align: 'right',
      className: 'col-last meta',
      cell: (e) => (e.asset ? <Time value={e.asset.updated_at} mode="relative" now={now} /> : ''),
    },
  ];

  return (
    <Panel className="pkg-head" aria-label={path ? `Folder ${path}` : `Files in ${repo}`}>
      <div className="folder-head">
        <h1 className="pkg-head__title">{path ? `${name}/` : name}</h1>
        <p className="pkg-head__meta">
          {path ? (
            <>
              <span className="facts__mono">{path}</span> in {repo}
            </>
          ) : (
            'Every file the repository stores, as clients address it.'
          )}
          {onUp && (
            <>
              {' · '}
              <button type="button" className="link-button link-strong" onClick={onUp}>
                Up one folder
              </button>
            </>
          )}
        </p>
      </div>
      <div className="pkg-table">
        <DataTable
          caption={path ? `Contents of ${path}` : `Files in ${repo}`}
          columns={columns}
          rows={items}
          rowKey={(e) => e.path}
          busy={pages.some((p) => p.isFetching)}
          minWidth={560}
          loading={q.isPending} empty={'This folder is empty.'}
        />
        {last.data?.next_cursor && (
          <div className="pkg-more">
            <button type="button" className="button button--secondary button--sm" onClick={() => setMore((m) => [...m, last.data!.next_cursor!])}>
              Load more
            </button>
          </div>
        )}
      </div>
    </Panel>
  );
}
