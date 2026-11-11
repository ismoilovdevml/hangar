import { getRouteApi, Link } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useAllRepositories, useBlobStore } from '../../api/catalog';
import { usePermissions } from '../../api/permissions';
import type { BlobStore, Repository } from '../../api/types';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { FormatBadge } from '../../components/FormatBadge';
import { Meter } from '../../components/Meter';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { dedupShare, quotaRatio, storeBadge, storeProblem } from '../../lib/blobStores';
import { formatBytes, formatCompact } from '../../lib/format';
import { isFormat, repoStatusMeta, TYPE_META } from '../../lib/repositories';
import { formatUtc } from '../../lib/tasks';
import { BlobStoreManage } from './BlobStoreManage';
import '../../styles/catalog.css';
import './blobstores.css';

const route = getRouteApi('/app/blob-stores/$name');

export function BlobStoreDetailPage() {
  const { name } = route.useParams();
  const store = useBlobStore(name);
  const canAdmin = usePermissions().can('admin', 'system', 'blobstores');

  if (store.isError) {
    const notFound = store.error instanceof ApiRequestError && store.error.status === 404;
    return (
      <div className="screen">
        <PageHeader title={name} description={notFound ? 'This blob store does not exist.' : undefined} eyebrow={<Link to="/blob-stores">Blob stores</Link>} />
        {!notFound && <ErrorNote what="the blob store" error={store.error} privilege="system:blobstores:read" onRetry={() => void store.refetch()} />}
      </div>
    );
  }
  const s = store.data;
  if (!s) {
    return (
      <div className="screen" aria-busy="true">
        <PageHeader title={name} description="Loading…" eyebrow={<Link to="/blob-stores">Blob stores</Link>} />
      </div>
    );
  }
  const badge = storeBadge(s);
  return (
    <div className="screen">
      <PageHeader
        eyebrow={
          <Link to="/blob-stores" className="link-strong">
            Blob stores
          </Link>
        }
        title={
          <span className="store-detail__title">
            {s.name} <Pill tone={badge.tone}>{badge.label}</Pill>
          </span>
        }
        description={`${s.kind === 's3' ? 'S3 bucket' : 'Filesystem'} · ${s.location}`}
      />
      <div className="store-detail">
        <Usage s={s} />
        <Configuration s={s} />
      </div>
      <UsedBy s={s} />
      {canAdmin && <BlobStoreManage key={s.id} s={s} />}
    </div>
  );
}

function Usage({ s }: { s: BlobStore }) {
  const capacity = s.capacity_bytes ?? s.soft_quota_bytes ?? null;
  const ratio = quotaRatio(s);
  const share = dedupShare(s.used_bytes, s.dedup_saved_bytes ?? 0);
  return (
    <Panel title="Usage" titleId="usage-title">
      <div className="store-card__usage">
        <div className="store-card__usage-row">
          <strong className="store-detail__used">{formatBytes(s.used_bytes)}</strong>
          <span className="meta">{capacity ? `of ${formatBytes(capacity)}` : 'no quota'}</span>
        </div>
        <Meter
          value={capacity ? s.used_bytes / capacity : 0}
          label="Usage of the soft quota"
          height={10}
          valueText={capacity ? `${formatBytes(s.used_bytes)} of ${formatBytes(capacity)}` : `${formatBytes(s.used_bytes)}, no quota`}
        />
      </div>
      <dl className="facts">
        <div>
          <dt>Soft quota</dt>
          <dd>{s.soft_quota_bytes ? `${formatBytes(s.soft_quota_bytes)}${ratio !== null ? ` · ${Math.round(ratio * 100)}% used` : ''}` : 'None'}</dd>
        </div>
        <div>
          <dt>Blobs</dt>
          <dd>{formatCompact(s.blob_count)}</dd>
        </div>
        <div>
          <dt>Saved by dedup</dt>
          <dd>
            {formatBytes(s.dedup_saved_bytes ?? 0)}
            {share !== null && <span className="meta"> · {Math.round(share * 100)}% of raw size</span>}
          </dd>
        </div>
        <div>
          <dt>Reclaimable</dt>
          <dd>{formatBytes(s.reclaimable_bytes ?? 0)}</dd>
        </div>
        <div>
          <dt>Health check</dt>
          <dd className={storeProblem(s) ? 'store-detail__problem' : undefined}>
            {s.checked_at ? (
              <>
                {s.state_detail ?? 'Checked'}
                <span className="meta"> · {formatUtc(s.checked_at, { seconds: true })}</span>
              </>
            ) : (
              'Not checked yet'
            )}
          </dd>
        </div>
        <div>
          <dt>Write latency</dt>
          <dd>{s.write_latency_ms != null ? `${Math.round(s.write_latency_ms)} ms` : s.kind === 's3' ? 'Not measured for S3' : 'Not measured'}</dd>
        </div>
        {s.encryption && (
          <div>
            <dt>Encryption</dt>
            <dd>{s.encryption}</dd>
          </div>
        )}
      </dl>
    </Panel>
  );
}

function Configuration({ s }: { s: BlobStore }) {
  const rows: [string, string][] =
    s.kind === 'fs'
      ? [['Directory', s.fs?.path ?? s.location]]
      : [
          ['Bucket', s.s3?.bucket ?? '—'],
          ['Prefix', s.s3?.prefix || '—'],
          ['Endpoint', s.s3?.endpoint || 'AWS S3'],
          ['Region', s.s3?.region || '—'],
          ['Addressing', s.s3?.use_path_style ? 'Path-style' : 'Virtual-hosted'],
          ['Access key id', s.s3?.access_key_id || 'From the environment'],
          ['Secret access key', s.s3?.access_key_id ? 'Stored encrypted, write-only' : '—'],
        ];
  return (
    <Panel title="Configuration" titleId="config-title">
      <dl className="summary__list">
        <dt>Kind</dt>
        <dd>{s.kind === 's3' ? 'S3 / MinIO' : 'Filesystem'}</dd>
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt>{k}</dt>
            <dd className="summary__url">{v}</dd>
          </div>
        ))}
        {s.created_at && (
          <>
            <dt>Created</dt>
            <dd>{new Date(s.created_at).toISOString().slice(0, 10)}</dd>
          </>
        )}
      </dl>
    </Panel>
  );
}

function UsedBy({ s }: { s: BlobStore }) {
  const repos = useAllRepositories();
  const rows = (repos.data ?? []).filter((r) => r.blob_store === s.name).sort((a, b) => b.size_bytes - a.size_bytes);
  const columns: Column<Repository>[] = [
    {
      key: 'name',
      header: 'Repository',
      className: 'col-first',
      cell: (r) => (
        <span className="store-repo">
          {isFormat(r.format) && <FormatBadge format={r.format} size={28} />}
          <Link to="/repositories/$name/settings" params={{ name: r.name }} className="repo-name__link">
            {r.name}
          </Link>
        </span>
      ),
    },
    { key: 'type', priority: 2, header: 'Type', cell: (r) => <Pill tone={TYPE_META[r.type].tone}>{TYPE_META[r.type].label}</Pill> },
    { key: 'size', header: 'Size', align: 'right', className: 'muted nowrap', cell: (r) => formatBytes(r.size_bytes) },
    { key: 'count', priority: 3, header: 'Versions', align: 'right', className: 'muted', cell: (r) => r.component_count },
    {
      key: 'status',
      header: 'Status',
      className: 'col-last nowrap',
      cell: (r) => {
        const s = repoStatusMeta(r);
        return <StatusDot tone={s.tone} label={s.label} title={s.hint} />;
      },
    },
  ];
  return (
    <Panel title={`Repositories using ${s.name}`} titleId="used-by-title" flush>
      {repos.isError ? (
        <ErrorNote what="repositories" error={repos.error} onRetry={() => void repos.refetch()} />
      ) : (
        <DataTable
          caption={`Repositories using ${s.name}`}
          columns={columns}
          rows={rows}
          rowKey={(r) => r.id}
          busy={repos.isFetching}
          minWidth={620}
          empty={repos.isPending ? 'Loading…' : 'No repository keeps its files here yet.'}
        />
      )}
      {repos.data && rows.length < s.repository_count && (
        <p className="meta store-detail__note">
          The store holds files of {s.repository_count} {s.repository_count === 1 ? 'repository' : 'repositories'}; you can see {rows.length} of them.
        </p>
      )}
    </Panel>
  );
}
