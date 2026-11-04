import type { OverviewMetrics, OverviewRange, TrafficBucket } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { Segmented } from '../../components/Segmented';
import { formatCompact, longDate, RANGES } from '../../lib/format';
import { bucketLabel, coverageNote } from '../../lib/overview';

const BAR_HEIGHT = 44;

const RANGE_EYEBROW: Record<OverviewRange, string> = {
  '1h': 'Last hour',
  today: 'Today',
  '7d': 'Last 7 days',
  '30d': 'Last 30 days',
};

const SHORT_RANGE: Record<OverviewRange, string> = { '1h': '1h', today: 'Today', '7d': '7d', '30d': '30d' };

const BUCKET_UNIT: Record<OverviewRange, string> = {
  '1h': 'per 5 minutes',
  today: 'per hour',
  '7d': 'per 6 hours',
  '30d': 'per day',
};

/** Counts read in full up to 9,999 and compactly above. */
function count(n: number): string {
  return n < 10_000 ? n.toLocaleString('en-GB') : formatCompact(n);
}

function Activity({ m }: { m: OverviewMetrics }) {
  const { published, cached, pushes_rejected: rejected } = m.activity;
  const total = m.requests.total;
  return (
    <p className="today__sentence" aria-live="polite">
      {total !== null && (
        <>
          <b>{count(total)}</b> {total === 1 ? 'request' : 'requests'},{' '}
        </>
      )}
      <b>{count(cached)}</b> {cached === 1 ? 'package' : 'packages'} cached, <b>{count(published)}</b> published,{' '}
      <b className={rejected > 0 ? 'today__bad' : undefined}>{count(rejected)}</b> {rejected === 1 ? 'push' : 'pushes'} rejected.
    </p>
  );
}

function Bars({ buckets, range }: { buckets: TrafficBucket[]; range: OverviewRange }) {
  const recorded = buckets.some((b) => b.requests !== null);
  if (buckets.length === 0 || !recorded) {
    return <p className="today__nobars meta">Requests per interval are not recorded for this range.</p>;
  }
  const max = Math.max(1, ...buckets.map((b) => b.requests ?? 0));
  const peak = buckets.reduce<TrafficBucket | null>((p, b) => ((b.requests ?? 0) > (p?.requests ?? 0) ? b : p), null);
  const summary = peak
    ? `Requests ${BUCKET_UNIT[range]}, busiest around ${bucketLabel(peak.start, range)} with ${formatCompact(peak.requests ?? 0)}`
    : `Requests ${BUCKET_UNIT[range]}: none yet`;
  const mid = buckets[Math.floor(buckets.length / 2)];
  return (
    <>
      <div className="today__bars" role="img" aria-label={summary}>
        {buckets.map((b) => (
          <span
            key={b.start}
            className={['today__bar', b === peak ? 'is-peak' : '', b.requests === null ? 'is-missing' : ''].filter(Boolean).join(' ')}
            style={{ height: `${Math.max(2, ((b.requests ?? 0) / max) * BAR_HEIGHT)}px` }}
            title={`${bucketLabel(b.start, range)} · ${b.requests === null ? 'not recorded' : `${formatCompact(b.requests)} requests`}`}
          />
        ))}
      </div>
      <div className="today__axis" aria-hidden="true">
        <span>{bucketLabel(buckets[0]!.start, range)}</span>
        {mid && buckets.length > 2 && <span>{bucketLabel(mid.start, range)}</span>}
        <span>Now</span>
      </div>
      <table className="visually-hidden">
        <caption>Requests {BUCKET_UNIT[range]}</caption>
        <thead>
          <tr>
            <th scope="col">Interval start</th>
            <th scope="col">Requests</th>
          </tr>
        </thead>
        <tbody>
          {buckets.map((b) => (
            <tr key={b.start}>
              <th scope="row">{bucketLabel(b.start, range)}</th>
              <td>{b.requests ?? 'not recorded'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}

interface TodayCardProps {
  m?: OverviewMetrics;
  range: OverviewRange;
  now: Date;
  error: boolean;
  onRetry: () => void;
  onRange: (r: OverviewRange) => void;
}

/** What happened in the range, in one sentence, with requests per interval underneath. */
export function TodayCard({ m, range, now, error, onRetry, onRange }: TodayCardProps) {
  const note = m ? coverageNote(m, now) : null;
  const eyebrow = [range === 'today' ? `Today · ${longDate(now)}` : RANGE_EYEBROW[range], note].filter(Boolean).join(' · ');
  return (
    <section className="ov-card today glass" aria-labelledby="today-title">
      <div className="today__head">
        <h2 className="ov-card__label" id="today-title">
          {eyebrow}
        </h2>
        <Segmented
          label="Time range"
          className="segmented--mini"
          options={RANGES.map((r) => ({ value: r.id, label: SHORT_RANGE[r.id] }))}
          value={range}
          onChange={onRange}
        />
      </div>
      {m ? (
        <>
          <Activity m={m} />
          {m.requests.total === null && <p className="meta">This server does not report request statistics.</p>}
          <div className="today__chart">
            <Bars buckets={m.traffic} range={range} />
          </div>
        </>
      ) : error ? (
        <ErrorNote what="today's activity" onRetry={onRetry} />
      ) : (
        <div className="today__loading" aria-busy="true">
          <span className="skeleton" style={{ width: '80%', height: 18 }} />
          <span className="skeleton" style={{ width: '45%', height: 18 }} />
          <span className="skeleton today__loading-bars" />
        </div>
      )}
    </section>
  );
}
