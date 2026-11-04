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
import { NotAvailable } from '../../components/NotAvailable';
import { coverageNote, plural, summarySentence } from '../../lib/overview';
import { RecentlyPublished } from './RecentlyPublished';
import { ConnectClient, UpstreamSources } from './SidePanels';
import { TrafficChart } from './TrafficChart';

const route = getRouteApi('/app/');

const KPI_LABELS = ['Storage used', 'Requests', 'Served from cache', 'Critical issues'];

const join = (...parts: (string | null | false | undefined)[]) => parts.filter(Boolean).join(' · ');

function StorageKpi({ m }: { m: OverviewMetrics }) {
  const { used_bytes: usedBytes, capacity_bytes: cap, added_bytes_24h: added } = m.storage;
  const used = splitBytes(usedBytes);
  const addedNote = `${formatBytes(added)} added since yesterday`;
  if (cap === null || cap <= 0) {
    return (
      <KpiCard
        label="Storage used"
        value={used.value}
        unit={`${used.unit} · no capacity limit set`}
        note={addedNote}
      />
    );
  }
  const ratio = usedBytes / cap;
  const tone: Tone = ratio >= 0.9 ? 'bad' : ratio >= 0.75 ? 'warn' : 'info';
  return (
    <KpiCard
      label="Storage used"
      tag={{ text: `${Math.round(ratio * 100)}%`, tone }}
      value={used.value}
      unit={`${used.unit} of ${formatBytes(cap)}`}
      meter={{ value: ratio, label: 'Storage used', valueText: `${formatBytes(usedBytes)} of ${formatBytes(cap)}` }}
      note={addedNote}
    />
  );
}

function RequestsKpi({ m, range, now }: { m: OverviewMetrics; range: OverviewRange; now: Date }) {
  const label = RANGES.find((r) => r.id === range)?.requests ?? 'Requests';
  const { total, trend_pct: trend, peak_total: peak, p95_ms: p95 } = m.requests;
  if (total === null) {
    return (
      <KpiCard
        label={label}
        value={<NotAvailable reason="Not reported" />}
        note="This server does not report request statistics."
      />
    );
  }
  return (
    <KpiCard
      label={label}
      tag={trend === null ? undefined : { text: formatSignedPercent(trend), tone: trend >= 0 ? 'ok' : 'neutral' }}
      value={formatCompact(total)}
      meter={
        peak
          ? { value: total / peak, label: 'Requests compared with the busiest period in 30 days', color: 'var(--chart-3)' }
          : undefined
      }
      note={join(p95 !== null && `95% of responses within ${Math.round(p95)} ms`, coverageNote(m, now)) || 'No latency data yet'}
    />
  );
}

function CacheKpi({ m }: { m: OverviewMetrics }) {
  const { hit_ratio: hit, bytes_saved: saved, hits, misses } = m.cache;
  if (hits === null || misses === null) {
    return (
      <KpiCard
        label="Served from cache"
        value={<NotAvailable reason="Not reported" />}
        note="This server does not report proxy cache statistics."
      />
    );
  }
  if (hit === null) {
    return (
      <KpiCard
        label="Served from cache"
        value={<NotAvailable reason="No proxy requests in this range" />}
        note="No proxy repository served content in this range."
      />
    );
  }
  const tag: { text: string; tone: Tone } =
    hit >= 0.8 ? { text: 'Healthy', tone: 'ok' } : hit >= 0.5 ? { text: 'Low', tone: 'warn' } : { text: 'Poor', tone: 'bad' };
  return (
    <KpiCard
      label="Served from cache"
      tag={tag}
      value={formatPercent(hit)}
      unit="%"
      meter={{ value: hit, label: 'Cache hit ratio', color: 'var(--ok-bar)' }}
      note={join(`${formatCompact(hits)} of ${formatCompact(hits + misses)} requests`, saved !== null && `${formatBytes(saved)} saved`)}
    />
  );
}

function IssuesKpi({ m }: { m: OverviewMetrics }) {
  const crit = m.issues.critical;
  return (
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
  );
}

function Kpis({ m, range, now }: { m: OverviewMetrics; range: OverviewRange; now: Date }) {
  return (
    <>
      <StorageKpi m={m} />
      <RequestsKpi m={m} range={range} now={now} />
      <CacheKpi m={m} />
      <IssuesKpi m={m} />
    </>
  );
}

function StorageByFormat({ m }: { m: OverviewMetrics }) {
  const rows = m.storage_by_format.filter((f) => f.bytes > 0);
  if (rows.length === 0) return <p className="meta">Nothing stored yet. Sizes per format appear after the first upload.</p>;
  const max = Math.max(1, ...rows.map((f) => f.bytes));
  return (
    <ul className="formats">
      {rows.map((f) => {
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
        {m ? <Kpis m={m} range={range} now={now} /> : KPI_LABELS.map((l) => <KpiCardSkeleton key={l} label={l} />)}
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
                {m && !m.traffic.some((b) => b.downloads !== null) ? 'Downloads (not recorded)' : 'Downloads'}
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
          <UpstreamSources range={range} />
          <ConnectClient />
        </div>
      </div>
    </div>
  );
}
