/**
 * The summary row of the Vulnerabilities screen: open findings by severity,
 * the quarantine policy of the repositories and the last 30 days.
 */
import { Link } from '@tanstack/react-router';
import { useAllRepositories } from '../../api/catalog';
import { useFindingTrend, useQuarantinedPackages } from '../../api/findings';
import type { FindingList, FindingTrendPoint } from '../../api/types';
import { plural } from '../../lib/overview';
import { SEVERITY_META } from '../../lib/vulnerabilities';

const CARDS = ['critical', 'high', 'medium', 'low'] as const;

function SeverityCards({ list }: { list?: FindingList }) {
  return (
    <ul className="vsum__severities" aria-label="Open findings by severity" aria-busy={!list || undefined}>
      {CARDS.map((s) => {
        // Unknown severities count as low, as the trend does.
        const n = list ? list.totals[s] + (s === 'low' ? (list.totals.unknown ?? 0) : 0) : null;
        const fix = list ? list.fixable[s] + (s === 'low' ? (list.fixable.unknown ?? 0) : 0) : null;
        return (
          <li key={s} className="vsum__card glass" aria-label={`${SEVERITY_META[s].label}: ${n ?? 'loading'}`}>
            <span className="vsum__label">
              <span className="vsum__dot" style={{ background: SEVERITY_META[s].color }} aria-hidden="true" />
              {SEVERITY_META[s].label}
            </span>
            <strong className="vsum__number">{n ?? '–'}</strong>
            <span className="vsum__note">{n === null ? ' ' : n === 0 ? 'None open' : fix ? `${fix} ${fix === 1 ? 'has' : 'have'} a fix` : 'No fix yet'}</span>
          </li>
        );
      })}
    </ul>
  );
}

function PolicyCard() {
  const repos = useAllRepositories();
  const quarantined = useQuarantinedPackages();
  const on = (repos.data ?? []).filter((r) => r.config.quarantine_critical).map((r) => r.name);
  return (
    <section className="vsum__card vsum__policy glass" aria-labelledby="vsum-policy-title">
      <h2 id="vsum-policy-title" className="vsum__label">
        Policy · quarantine critical
      </h2>
      {repos.isError ? (
        <span className="vsum__note">Repositories unavailable</span>
      ) : !repos.data ? (
        <span className="vsum__note">Loading…</span>
      ) : (
        <>
          <span className="vsum__value">
            {on.length ? `On in ${on.length} of ${plural(repos.data.length, 'repository', 'repositories')}` : 'Off in every repository'}
          </span>
          <span className="vsum__note">
            {on.length ? (
              <>
                {on.slice(0, 2).map((name, i) => (
                  <span key={name}>
                    {i > 0 && ', '}
                    <Link to="/repositories/$name/settings" params={{ name }}>
                      {name}
                    </Link>
                  </span>
                ))}
                {on.length > 2 && ` +${on.length - 2}`}
              </>
            ) : (
              'Turn it on in a repository’s settings'
            )}
            {quarantined.data !== undefined && ` · ${plural(quarantined.data, 'package')} quarantined`}
          </span>
        </>
      )}
    </section>
  );
}

const W = 150;
const H = 34;

/** Open critical + high findings at the end of each day (UTC). */
function TrendCard() {
  const trend = useFindingTrend(30);
  const points: FindingTrendPoint[] = trend.data?.items ?? [];
  const values = points.map((p) => p.critical + p.high);
  const max = Math.max(1, ...values);
  const bar = points.length ? W / points.length : 0;
  const first = values[0] ?? 0;
  const last = values.at(-1) ?? 0;
  return (
    <section className="vsum__card vsum__trend glass" aria-labelledby="vsum-trend-title">
      <h2 id="vsum-trend-title" className="vsum__label">
        Last 30 days · critical + high
      </h2>
      {trend.isError ? (
        <span className="vsum__note">Trend unavailable</span>
      ) : (
        <>
          <svg
            className="vsum__chart"
            viewBox={`0 0 ${W} ${H}`}
            preserveAspectRatio="none"
            role="img"
            aria-label={points.length ? `Open critical and high findings: ${first} on ${points[0]!.date}, ${last} on ${points.at(-1)!.date}` : 'Loading'}
          >
            <line x1="0" y1={H - 0.5} x2={W} y2={H - 0.5} className="vsum__axis" />
            {points.map((p, i) => {
              const v = values[i]!;
              const h = v ? Math.max(2, (v / max) * (H - 2)) : 0;
              return (
                <g key={p.date}>
                  <title>{`${p.date}: ${v} open (${p.critical} critical, ${p.high} high)`}</title>
                  {/* Full-height hit area; the bar itself is thin. */}
                  <rect x={i * bar} y="0" width={bar} height={H} fill="transparent" />
                  {h > 0 && <rect className="vsum__bar" x={i * bar + 0.5} y={H - h} width={Math.max(1, bar - 1)} height={h} rx="1" />}
                </g>
              );
            })}
          </svg>
          <span className="vsum__note">{points.length ? (first === last ? `${last} open, unchanged` : `${first} → ${last} open`) : ' '}</span>
        </>
      )}
    </section>
  );
}

export function VulnSummary({ list }: { list?: FindingList }) {
  return (
    <div className="vsum">
      <SeverityCards list={list} />
      <TrendCard />
      <PolicyCard />
    </div>
  );
}
