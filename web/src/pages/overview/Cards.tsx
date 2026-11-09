import type { CSSProperties } from 'react';
import { Link } from '@tanstack/react-router';
import type { OverviewMetrics, UpstreamHealth } from '../../api/types';
import { useGate } from '../../api/permissions';
import { GATES } from '../../lib/gates';
import { ErrorNote } from '../../components/ErrorNote';
import { FORMAT_META, formatBytes, formatCompact, formatPercent } from '../../lib/format';
import { upstreamAuthProblem } from '../../lib/repositories';
import { headlineUpstream, plural, ratioPercent, storageNote, successRatio, urlHost } from '../../lib/overview';

function CardSkeleton() {
  return (
    <span className="ov-card__loading" aria-busy="true">
      <span className="skeleton" style={{ width: '55%', height: 24 }} />
      <span className="skeleton" style={{ width: '80%', height: 12 }} />
    </span>
  );
}

/* ---------------------------------------------------------------- outcome ring */

/** Share of requests answered without a server error or a refusal, with latency underneath. */
export function OutcomeCard({ m, error }: { m?: OverviewMetrics; error: boolean }) {
  if (!m) {
    return (
      <section className="ov-card outcome glass" aria-label="Request outcomes">
        {error ? <p className="meta">Request outcomes are unavailable.</p> : <span className="outcome__ring outcome__ring--loading" aria-busy="true" />}
      </section>
    );
  }
  const r = m.requests;
  const ratio = successRatio(r);
  const outcomes: string[] = [];
  if (r.denied) outcomes.push(`${formatCompact(r.denied)} denied`);
  if (r.server_errors) outcomes.push(`${formatCompact(r.server_errors)} server ${r.server_errors === 1 ? 'error' : 'errors'}`);
  if (ratio !== null && outcomes.length === 0) outcomes.push('no errors');
  const p95 = r.p95_ms === null ? null : Math.round(r.p95_ms);

  let center: { value: string; caption: string; label: string };
  if (r.total === null) center = { value: '—', caption: 'not reported', label: 'Request outcomes are not reported by this server' };
  else if (ratio === null) center = { value: '—', caption: 'no requests', label: 'No requests in this range yet' };
  else {
    const pct = ratioPercent(ratio);
    center = { value: `${pct}%`, caption: 'succeeded', label: `${pct} percent of requests succeeded` };
  }
  const style = { '--ok-share': `${ratio === null ? 0 : ratio * 100}%` } as CSSProperties;

  return (
    <section className="ov-card outcome glass" aria-label="Request outcomes">
      <div className={ratio === null ? 'outcome__ring outcome__ring--empty' : 'outcome__ring'} style={style} role="img" aria-label={center.label}>
        <div className="outcome__hole">
          <span className="outcome__value">{center.value}</span>
          <span className="outcome__caption">{center.caption}</span>
        </div>
      </div>
      <p className="outcome__note">
        {p95 !== null && (
          <>
            p95 <b>{p95} ms</b>
            {outcomes.length > 0 && ' · '}
          </>
        )}
        {outcomes.join(' · ')}
        {p95 === null && outcomes.length === 0 && (r.total === null ? 'No request statistics' : 'Nothing to measure yet')}
      </p>
    </section>
  );
}

/* ---------------------------------------------------------------- storage */

export function StorageCard({ m, error }: { m?: OverviewMetrics; error: boolean }) {
  if (!m) {
    return (
      <div className="ov-card ov-stat glass">
        <span className="ov-card__label">Storage</span>
        {error ? <span className="meta">Unavailable</span> : <CardSkeleton />}
      </div>
    );
  }
  const rows = m.storage_by_format.filter((f) => f.bytes > 0);
  const total = rows.reduce((n, f) => n + f.bytes, 0);
  const cap = m.storage.capacity_bytes;
  // With a quota the bar fills up to it; without one it shows how formats share what is stored.
  const scale = cap !== null && cap > 0 ? Math.min(1, m.storage.used_bytes / cap) : total > 0 ? 1 : 0;
  const label = (f: keyof typeof FORMAT_META) => FORMAT_META[f]?.label ?? f;
  return (
    <Link to="/blob-stores" className="ov-card ov-stat ov-link glass">
      <span className="ov-card__label">Storage</span>
      <span className="ov-stat__value">{formatBytes(m.storage.used_bytes)}</span>
      <span className="ov-meter" aria-hidden="true">
        {rows.map((f) => (
          <span
            key={f.format}
            className="ov-meter__part"
            style={{ width: `${(f.bytes / total) * scale * 100}%`, background: FORMAT_META[f.format]?.bar ?? 'var(--chart-1)' }}
          />
        ))}
      </span>
      <span className="ov-stat__note">{storageNote(m, label)}</span>
    </Link>
  );
}

/* ---------------------------------------------------------------- security */

export function SecurityCard({ m, error }: { m?: OverviewMetrics; error: boolean }) {
  if (!m) {
    return (
      <div className="ov-card ov-stat glass">
        <span className="ov-card__label">Security</span>
        {error ? <span className="meta">Unavailable</span> : <CardSkeleton />}
      </div>
    );
  }
  const { critical, open_findings: open, scanning } = m.issues;
  let value: { text: string; tone: 'ok' | 'bad' | 'neutral' };
  let note: string;
  if (scanning && !scanning.enabled) {
    value = { text: 'Scanning off', tone: 'neutral' };
    note = 'Packages are not checked for vulnerabilities';
  } else if (scanning && scanning.scanned_components === 0) {
    value = { text: 'Not scanned yet', tone: 'neutral' };
    note = 'No package has been scanned yet';
  } else {
    value = critical > 0 ? { text: `${critical} critical`, tone: 'bad' } : { text: open > 0 ? 'No critical' : 'Clean', tone: 'ok' };
    const scanned = scanning ? ` · ${scanning.scanned_components.toLocaleString('en-GB')} scanned` : '';
    note = critical > 0 ? `In ${plural(m.issues.packages_affected, 'package')} · ${m.issues.quarantined} quarantined` : `${plural(open, 'open finding')}${scanned}`;
  }
  return (
    <Link to="/vulnerabilities" className="ov-card ov-stat ov-link glass">
      <span className="ov-card__label">Security</span>
      <span className={`ov-stat__value ov-stat__value--${value.tone}`}>{value.text}</span>
      <span className="ov-stat__note">{note}</span>
    </Link>
  );
}

/* ---------------------------------------------------------------- upstream */

const UPSTREAM_TONE: Record<UpstreamHealth['state'], { tone: 'ok' | 'warn' | 'bad' | 'neutral'; label: string }> = {
  healthy: { tone: 'ok', label: 'healthy' },
  slow: { tone: 'warn', label: 'slow' },
  blocked: { tone: 'bad', label: 'blocked' },
  offline: { tone: 'bad', label: 'offline' },
  unknown: { tone: 'neutral', label: 'not checked yet' },
};

interface UpstreamCardProps {
  items?: UpstreamHealth[];
  error: boolean;
  onRetry: () => void;
  m?: OverviewMetrics;
}

export function UpstreamCard({ items, error, onRetry, m }: UpstreamCardProps) {
  const canCreateRepo = useGate(GATES.createRepository);
  if (!items) {
    return (
      <div className="ov-card ov-stat glass">
        <span className="ov-card__label">Upstream</span>
        {error ? <ErrorNote what="upstream health" onRetry={onRetry} /> : <CardSkeleton />}
      </div>
    );
  }
  const u = headlineUpstream(items);
  if (!u) {
    if (!canCreateRepo) {
      return (
        <div className="ov-card ov-stat glass">
          <span className="ov-card__label">Upstream</span>
          <span className="ov-stat__value ov-stat__value--sm">No proxy yet</span>
          <span className="ov-stat__note">No proxy repository you can read caches a public registry</span>
        </div>
      );
    }
    return (
      <Link to="/repositories/new" search={{ type: 'proxy' }} className="ov-card ov-stat ov-link glass">
        <span className="ov-card__label">Upstream</span>
        <span className="ov-stat__value ov-stat__value--sm">No proxy yet</span>
        <span className="ov-stat__note">Add a proxy repository to cache a public registry</span>
      </Link>
    );
  }
  const auth = u.state === 'blocked' ? upstreamAuthProblem(u.reason) : null;
  const s = auth ? { ...UPSTREAM_TONE.blocked, label: auth.label } : UPSTREAM_TONE[u.state];
  const hit = m?.cache.hit_ratio;
  const note = [
    u.latency_ms !== null ? `${u.latency_ms} ms` : s.label,
    hit !== null && hit !== undefined ? `cache hit ${formatPercent(hit, 0)}%` : null,
    items.length > 1 ? `${items.filter((x) => x.state === 'healthy').length} of ${items.length} healthy` : null,
  ]
    .filter(Boolean)
    .join(' · ');
  return (
    <Link to="/repositories" search={{ type: 'proxy' }} className="ov-card ov-stat ov-link glass">
      <span className="ov-card__label">Upstream</span>
      <span className="ov-stat__value ov-stat__value--sm ov-stat__host" title={`${u.name} · ${u.url}`}>
        <span className={`status__dot dot-${s.tone}`} aria-hidden="true" />
        <span className="ov-stat__host-text">{urlHost(u.url)}</span>
        <span className="visually-hidden"> ({s.label})</span>
      </span>
      <span className="ov-stat__note">{note}</span>
    </Link>
  );
}
