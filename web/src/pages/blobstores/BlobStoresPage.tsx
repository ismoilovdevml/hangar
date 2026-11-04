import { getRouteApi, Link } from '@tanstack/react-router';
import { useBlobStores } from '../../api/hooks';
import { usePermissions } from '../../api/permissions';
import type { BlobStore, BlobStoreList } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { IconPlus } from '../../components/icons';
import { KpiCard, KpiCardSkeleton } from '../../components/KpiCard';
import { Meter } from '../../components/Meter';
import { PageHeader } from '../../components/PageHeader';
import { Pill } from '../../components/Pill';
import { dedupShare, humanDays, quotaRatio, storeBadge } from '../../lib/blobStores';
import { formatBytes, formatCompact, splitBytes } from '../../lib/format';
import { BlobStoreCreate } from './BlobStoreCreate';
import './blobstores.css';

const route = getRouteApi('/app/blob-stores');

export function BlobStoresPage() {
  const { add } = route.useSearch();
  const navigate = route.useNavigate();
  const list = useBlobStores();
  const perms = usePermissions();
  const canCreate = perms.can('admin', 'system', 'blobstores');
  const setAdd = (v: boolean) => void navigate({ search: v ? { add: true } : {}, replace: !v });

  return (
    <div className="screen">
      <PageHeader
        title="Blob stores"
        description="The disks and buckets behind your repositories. Identical files are stored once."
        actions={
          (!add || !canCreate) && (
            <button
              type="button"
              className="button button--primary"
              disabled={!canCreate}
              title={canCreate ? undefined : 'Needs the blob store admin privilege'}
              onClick={() => setAdd(true)}
            >
              <IconPlus size={16} /> Add blob store
            </button>
          )
        }
      />

      {add && canCreate && <BlobStoreCreate onClose={() => setAdd(false)} />}

      {list.isError ? (
        <ErrorNote what="blob stores" onRetry={() => void list.refetch()} />
      ) : (
        <>
          <Totals data={list.data} />
          <section className="store-grid" aria-label="Blob stores">
            {list.data?.items.map((s) => <StoreCard key={s.id} store={s} />)}
            {list.data && list.data.items.length === 0 && <p className="meta">No blob stores yet.</p>}
          </section>
        </>
      )}
    </div>
  );
}

function Totals({ data }: { data: BlobStoreList | undefined }) {
  const labels = ['Total used', 'Saved by deduplication', 'Growth', 'Reclaimable now'];
  if (!data) {
    return (
      <section className="kpis" aria-label="Storage totals" aria-busy="true">
        {labels.map((l) => (
          <KpiCardSkeleton key={l} label={l} />
        ))}
      </section>
    );
  }
  const t = data.totals;
  const used = splitBytes(t.used_bytes);
  const saved = splitBytes(t.dedup_saved_bytes);
  const growth = splitBytes(t.growth_bytes_per_day);
  const reclaim = splitBytes(t.reclaimable_bytes);
  const share = dedupShare(t.used_bytes, t.dedup_saved_bytes);
  const n = data.items.length;
  const capacity = t.capacity_bytes;
  return (
    <section className="kpis" aria-label="Storage totals">
      <KpiCard
        label="Total used"
        value={used.value}
        unit={used.unit}
        meter={
          capacity
            ? { value: t.used_bytes / capacity, label: 'Used of total quota', valueText: `${formatBytes(t.used_bytes)} of ${formatBytes(capacity)}` }
            : undefined
        }
        note={capacity ? `of ${formatBytes(capacity)} across ${n} ${n === 1 ? 'store' : 'stores'}` : `across ${n} ${n === 1 ? 'store' : 'stores'} · not every store has a quota`}
      />
      <KpiCard
        label="Saved by deduplication"
        value={saved.value}
        unit={saved.unit}
        note={share !== null ? `${Math.round(share * 100)}% of the raw size` : 'No duplicate content yet'}
      />
      <KpiCard
        label="Growth"
        value={`+${growth.value}`}
        unit={`${growth.unit} / day`}
        note={t.days_to_full != null ? `Quota reached in ${humanDays(t.days_to_full)}` : capacity ? 'Not growing toward the quota' : 'Set quotas to see time to full'}
      />
      <KpiCard
        label="Reclaimable now"
        value={reclaim.value}
        unit={reclaim.unit}
        note={t.reclaimable_bytes > 0 ? 'Freed by the next blob compaction' : 'Nothing waiting for compaction'}
      />
    </section>
  );
}

function StoreCard({ store: s }: { store: BlobStore }) {
  const badge = storeBadge(s);
  const capacity = s.capacity_bytes ?? s.soft_quota_bytes ?? null;
  const ratio = quotaRatio(s);
  return (
    <article className="store-card glass" aria-labelledby={`store-${s.id}`}>
      <header className="store-card__head">
        <span className={`store-card__kind tone-${s.kind === 's3' ? 'peach' : 'info'}`} aria-hidden="true">
          {s.kind === 's3' ? 'S3' : 'FS'}
        </span>
        <div className="store-card__title">
          <h2 id={`store-${s.id}`} className="store-card__name">
            <Link to="/blob-stores/$name" params={{ name: s.name }}>
              {s.name}
            </Link>
          </h2>
          <p className="store-card__loc">
            <span className="visually-hidden">{s.kind === 's3' ? 'S3 bucket' : 'File system'}: </span>
            {s.location}
          </p>
        </div>
        <Pill tone={badge.tone}>{badge.label}</Pill>
      </header>

      <div className="store-card__usage">
        <div className="store-card__usage-row">
          <strong>{formatBytes(s.used_bytes)}</strong>
          <span className="meta">{capacity ? `of ${formatBytes(capacity)}` : 'no quota'}</span>
        </div>
        <Meter
          value={capacity ? s.used_bytes / capacity : 0}
          label={`${s.name} usage`}
          height={8}
          valueText={capacity ? `${formatBytes(s.used_bytes)} of ${formatBytes(capacity)}` : `${formatBytes(s.used_bytes)}, no quota`}
        />
        <p className="meta">
          {s.soft_quota_bytes
            ? `Soft quota ${formatBytes(s.soft_quota_bytes)}${ratio !== null ? ` · ${Math.round(ratio * 100)}% used` : ''}`
            : 'No soft quota set'}
        </p>
      </div>

      <dl className="store-card__facts">
        <div>
          <dt>Repositories</dt>
          <dd>{s.repository_count}</dd>
        </div>
        <div>
          <dt>Blobs</dt>
          <dd>{formatCompact(s.blob_count)}</dd>
        </div>
        <div>
          <dt>Saved by dedup</dt>
          <dd>{formatBytes(s.dedup_saved_bytes ?? 0)}</dd>
        </div>
        <div>
          <dt>Reclaimable</dt>
          <dd>{formatBytes(s.reclaimable_bytes ?? 0)}</dd>
        </div>
        {s.write_latency_ms != null && (
          <div>
            <dt>Write latency</dt>
            <dd>{Math.round(s.write_latency_ms)} ms</dd>
          </div>
        )}
        {s.encryption && (
          <div>
            <dt>Encryption</dt>
            <dd>{s.encryption}</dd>
          </div>
        )}
      </dl>

      <div className="store-card__actions">
        <Link to="/blob-stores/$name" params={{ name: s.name }} className="button button--secondary button--sm">
          Details and repositories
        </Link>
      </div>
    </article>
  );
}
