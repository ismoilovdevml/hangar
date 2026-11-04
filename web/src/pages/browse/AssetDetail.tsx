import { useState, type ReactNode } from 'react';
import { Link } from '@tanstack/react-router';
import { useAsset, useDeleteAsset } from '../../api/browse';
import { ApiRequestError } from '../../api/client';
import { usePermissions } from '../../api/permissions';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { CopyButton } from '../../components/CopyButton';
import { ErrorNote } from '../../components/ErrorNote';
import { IconDownload } from '../../components/opsIcons';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { FORMAT_META, formatBytes, formatCompact, relativeTime } from '../../lib/format';
import { componentName } from '../../lib/overview';
import { downloadHref } from '../../lib/packages';
import { isFormat } from '../../lib/repositories';
import { useNow } from '../../lib/useNow';
import { dateTime, deleteMessage } from './detailUtil';
import { FormatIcon } from './formatIcon';

interface AssetDetailProps {
  id: string;
  /** Repository the tree is showing (a group when browsing one). */
  treeRepo: string;
  onDeleted: () => void;
}

const baseName = (p: string) => p.slice(p.lastIndexOf('/') + 1);

export function AssetDetail({ id, treeRepo, onDeleted }: AssetDetailProps) {
  const q = useAsset(id);
  const perms = usePermissions();
  const remove = useDeleteAsset();
  const [confirm, setConfirm] = useState(false);
  const now = useNow();

  if (q.isError) {
    const notFound = q.error instanceof ApiRequestError && q.error.status === 404;
    return (
      <Panel className="browse-empty" role="status">
        {notFound ? <p>This file no longer exists, or you cannot see it.</p> : <ErrorNote what="the file" onRetry={() => void q.refetch()} />}
      </Panel>
    );
  }
  const a = q.data;
  if (!a) {
    return (
      <Panel aria-busy="true" className="browse-empty">
        <p>Loading file…</p>
      </Panel>
    );
  }

  const canDelete = perms.can('delete', 'repo', a.repository);
  const name = baseName(a.path);
  const sha256 = a.checksums.sha256 ?? a.blob_digest.replace(/^sha256:/, '');
  const otherSums = Object.entries(a.checksums)
    .filter(([k]) => k !== 'sha256')
    .sort(([x], [y]) => x.localeCompare(y));
  const viaGroup = treeRepo !== a.repository;
  const c = a.component;

  return (
    <>
      <Panel className="pkg-head" aria-label={`File ${name}`}>
        <div className="pkg-head__row">
          <FormatIcon format={a.format} size={48} />
          <div className="pkg-head__text">
            <div className="pkg-head__titles">
              <h1 className="pkg-head__title">{name}</h1>
              <Pill tone={a.kind === 'content' ? 'info' : 'neutral'}>{a.kind === 'content' ? 'Content' : 'Metadata'}</Pill>
              {a.cached_from_remote && <Pill tone="neutral">Cached from upstream</Pill>}
            </div>
            <p className="pkg-head__meta facts__mono">{a.path}</p>
          </div>
          <div className="pkg-head__actions">
            {a.download_url && (
              <a href={downloadHref(a.download_url)} className="button button--primary" download>
                <IconDownload size={16} /> Download
              </a>
            )}
            <button
              type="button"
              className="button button--danger-outline"
              disabled={!canDelete}
              title={canDelete ? undefined : 'Needs delete rights on this repository'}
              onClick={() => {
                remove.reset();
                setConfirm(true);
              }}
            >
              Delete file
            </button>
          </div>
        </div>

        <dl className="facts">
          <Fact label="Repository">
            <Link to="/browse" search={{ repo: a.repository }} className="link-strong">
              {a.repository}
            </Link>
            {viaGroup && <span className="meta"> · served through {treeRepo}</span>}
          </Fact>
          <Fact label="Format">{isFormat(a.format) ? FORMAT_META[a.format].label : a.format}</Fact>
          {c && (
            <Fact label="Package">
              <Link
                to="/browse"
                search={{ repo: a.repository, ns: c.namespace || undefined, name: c.name, version: c.version || undefined }}
                className="link-strong"
              >
                {componentName({ format: a.format, namespace: c.namespace, name: c.name })}
                {c.version ? ` ${c.version}` : ''}
              </Link>
            </Fact>
          )}
          <Fact label="Size">{formatBytes(a.size_bytes)}</Fact>
          <Fact label="Content type">
            <span className="facts__mono">{a.content_type}</span>
          </Fact>
          <Fact label={a.cached_from_remote ? 'Cached' : 'Stored'}>
            <time dateTime={a.created_at} title={dateTime(a.created_at)}>
              {relativeTime(a.created_at, now)}
            </time>
          </Fact>
          {a.updated_at !== a.created_at && (
            <Fact label="Updated">
              <time dateTime={a.updated_at} title={dateTime(a.updated_at)}>
                {relativeTime(a.updated_at, now)}
              </time>
            </Fact>
          )}
          <Fact label="Blob created">
            {a.blob_created_at ? (
              <time dateTime={a.blob_created_at} title={dateTime(a.blob_created_at)}>
                {relativeTime(a.blob_created_at, now)}
              </time>
            ) : (
              'Unknown'
            )}
          </Fact>
          <Fact label="Downloads">{formatCompact(a.download_count)}</Fact>
          <Fact label="Last downloaded">
            {a.last_downloaded_at ? (
              <time dateTime={a.last_downloaded_at} title={dateTime(a.last_downloaded_at)}>
                {relativeTime(a.last_downloaded_at, now)}
              </time>
            ) : (
              'Never'
            )}
          </Fact>
          {a.repository_type === 'proxy' && (
            <Fact label="Upstream" wide>
              {a.remote_url ? <span className="facts__mono">{a.remote_url}</span> : 'Cached copy of remote content'}
            </Fact>
          )}
          <Fact label="Path" wide>
            <span className="facts__mono facts__copy">
              <span>{a.path}</span>
              <CopyButton text={a.path} label="Copy path" />
            </span>
          </Fact>
          {a.download_url && (
            <Fact label="URL" wide>
              <span className="facts__mono facts__copy">
                <span>{a.download_url}</span>
                <CopyButton text={a.download_url} label="Copy download URL" />
              </span>
            </Fact>
          )}
        </dl>

        <div className="checksums">
          <h2 className="checksums__title">Checksums</h2>
          <dl className="checksums__list">
            {[['sha256', sha256] as const, ...otherSums].map(([algo, value]) => (
              <div key={algo} className="checksums__row">
                <dt>{algo}</dt>
                <dd className="facts__mono">
                  <span className="checksums__value">{value}</span>
                  <CopyButton text={value} label={`Copy ${algo}`} />
                </dd>
              </div>
            ))}
          </dl>
        </div>
        {!a.download_url && <p className="meta">Container images are pulled with the registry protocol; their files have no download link.</p>}
      </Panel>

      {confirm && (
        <ConfirmDialog
          title={`Delete ${name}?`}
          confirmLabel="Delete file"
          tone="danger"
          busy={remove.isPending}
          error={remove.isError ? deleteMessage(remove.error, 'files') : null}
          onCancel={() => setConfirm(false)}
          onConfirm={() =>
            remove.mutate(a.id, {
              onSuccess: () => {
                setConfirm(false);
                onDeleted();
              },
            })
          }
        >
          <p>
            Clients will no longer be able to download <span className="facts__mono">{a.path}</span> from <strong>{a.repository}</strong>
            {c ? '. If it is the last file of its version, the version is deleted too' : ''}. Its blob is released for compaction. This cannot be undone.
          </p>
        </ConfirmDialog>
      )}
    </>
  );
}

export function Fact({ label, children, wide }: { label: string; children: ReactNode; wide?: boolean }) {
  return (
    <div className={wide ? 'facts__wide' : undefined}>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}
