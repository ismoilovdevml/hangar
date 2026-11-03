import { getRouteApi, Link } from '@tanstack/react-router';
import { useOverview, useSession } from '../../api/hooks';
import type { OverviewMetrics, OverviewRange } from '../../api/types';
import { KpiCard, KpiCardSkeleton } from '../../components/KpiCard';
import { Panel } from '../../components/Panel';
import { Segmented } from '../../components/Segmented';
import {
  FORMAT_META,
  firstName,
  formatBytes,
  formatCompact,
  formatPercent,
  formatSignedPercent,
  greeting,
  longDate,
  RANGES,
  splitBytes,
  type Tone,
} from '../../lib/format';
import { useNow } from '../../lib/useNow';
import { ErrorNote } from '../../components/ErrorNote';
import { plural, summarySentence } from '../../lib/overview';
import { RecentlyPublished } from './RecentlyPublished';
import { ConnectClient, UpstreamSources } from './SidePanels';
import { TrafficChart } from './TrafficChart';

const route = getRouteApi('/app/');

const KPI_LABELS = ['Storage used', 'Requests', 'Served from cache', 'Critical issues'];

function Kpis({ m, range }: { m: OverviewMetrics; range: OverviewRange }) {
  const storageRatio = m.storage.capacity_bytes ? m.storage.used_bytes / m.storage.capacity_bytes : 0;
  const used = splitBytes(m.storage.used_bytes);
  const storageTone: Tone = storageRatio >= 0.9 ? 'bad' : storageRatio >= 0.75 ? 'warn' : 'info';
  const hit = m.cache.hit_ratio;
  const cacheTag: { text: string; tone: Tone } =
    hit >= 0.8 ? { text: 'Healthy', tone: 'ok' } : hit >= 0.5 ? { text: 'Low', tone: 'warn' } : { text: 'Poor', tone: 'bad' };
  const reqLabel = RANGES.find((r) => r.id === range)?.requests ?? 'Requests';
  const crit = m.issues.critical;

  return (
    <>
      <KpiCard
        label="Storage used"
        tag={{ text: `${Math.round(storageRatio * 100)}%`, tone: storageTone }}
        value={used.value}
        unit={`${used.unit} of ${formatBytes(m.storage.capacity_bytes)}`}
        meter={{ value: storageRatio, label: 'Storage used', valueText: `${formatBytes(m.storage.used_bytes)} of ${formatBytes(m.storage.capacity_bytes)}` }}
        note={`${formatBytes(m.storage.added_bytes_24h)} added since yesterday`}
      />
      <KpiCard
        label={reqLabel}
        tag={{ text: formatSignedPercent(m.requests.trend_pct), tone: m.requests.trend_pct >= 0 ? 'ok' : 'neutral' }}
        value={formatCompact(m.requests.total)}
        meter={{
          value: m.requests.peak_total ? m.requests.total / m.requests.peak_total : 0,
          label: 'Requests compared with the busiest period in 30 days',
          color: 'var(--chart-3)',
        }}
        note={`95% of responses within ${m.requests.p95_ms} ms`}
      />
      <KpiCard
        label="Served from cache"
        tag={cacheTag}
        value={formatPercent(hit)}
        unit="%"
        meter={{ value: hit, label: 'Cache hit ratio', color: 'var(--ok-bar)' }}
        note={`${formatBytes(m.cache.bytes_saved)} of downloads saved`}
      />
      <KpiCard
        label="Critical issues"
        tag={crit > 0 ? { text: 'Review', tone: 'peach' } : { text: 'Clear', tone: 'ok' }}
        value={crit}
        unit={`in ${plural(m.issues.packages_affected, 'package')}`}
        meter={{
          value: m.issues.open_findings ? crit / m.issues.open_findings : 0,
          label: 'Critical share of open findings',
          color: 'var(--chart-2)',
        }}
        note={`${m.issues.quarantined} already quarantined`}
      />
    </>
  );
}

function StorageByFormat({ m }: { m: OverviewMetrics }) {
  const max = Math.max(1, ...m.storage_by_format.map((f) => f.bytes));
  return (
    <ul className="formats">
      {m.storage_by_format.map((f) => {
        const meta = FORMAT_META[f.format];
        return (
          <li key={f.format} className="formats__row">
            <span className="formats__name">{meta.label}</span>
            <span className="formats__track" aria-hidden="true">
              <span className="formats__fill" style={{ width: `${(f.bytes / max) * 100}%`, background: meta.bar }} />
            </span>
            <span className="formats__size">{formatBytes(f.bytes)}</span>
          </li>
        );
      })}
    </ul>
  );
}

export function OverviewPage() {
  const { range = 'today' } = route.useSearch();
  const navigate = route.useNavigate();
  const session = useSession();
  const overview = useOverview(range);
  const now = useNow(60_000);
  const m = overview.data;
  const name = session.data ? firstName(session.data.user.display_name) : null;
  const rangeMeta = RANGES.find((r) => r.id === range) ?? RANGES[1]!;

  return (
    <div className="overview">
      <div className="page-header page-header--overview">
        <div className="page-header__text">
          <p className="page-header__eyebrow">{longDate(now)}</p>
          <h1 className="page-header__title page-header__title--greeting">
            {greeting(now)}
            {name ? `, ${name}` : ''}
          </h1>
          <p className="page-header__desc" aria-live="polite">
            {m ? summarySentence(m) : ' '}
          </p>
        </div>
        <Segmented
          label="Time range"
          options={RANGES.map((r) => ({ value: r.id, label: r.label }))}
          value={range}
          onChange={(r) => void navigate({ search: (s) => ({ ...s, range: r === 'today' ? undefined : r }), replace: true })}
        />
      </div>

      <section aria-label="Key metrics" className="kpis">
        {m ? <Kpis m={m} range={range} /> : KPI_LABELS.map((l) => <KpiCardSkeleton key={l} label={l} />)}
      </section>
      {overview.isError && <ErrorNote what="overview metrics" onRetry={() => void overview.refetch()} />}

      <div className="overview__charts">
        <Panel
          title={rangeMeta.title}
          titleId="traffic-title"
          className="chart-panel"
          action={
            <div className="legend">
              <span className="legend__item">
                <span className="legend__swatch" style={{ background: 'var(--chart-1)' }} />
                Downloads
              </span>
              <span className="legend__item">
                <span className="legend__swatch" style={{ background: 'var(--chart-2)' }} />
                Uploads
              </span>
            </div>
          }
        >
          {m ? <TrafficChart buckets={m.traffic} range={range} /> : <div className="traffic traffic--loading" aria-busy="true" />}
        </Panel>

        <Panel
          title="Storage by format"
          titleId="formats-title"
          className="chart-panel chart-panel--formats"
          action={
            <Link to="/blob-stores" className="panel__link">
              Blob stores
            </Link>
          }
        >
          {m ? <StorageByFormat m={m} /> : <p className="meta">Loading…</p>}
        </Panel>
      </div>

      <div className="overview__bottom">
        <RecentlyPublished />
        <div className="overview__side">
          <UpstreamSources />
          <ConnectClient />
        </div>
      </div>
    </div>
  );
}
