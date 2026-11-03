import type { OverviewRange, TrafficBucket } from '../../api/types';
import { formatCompact } from '../../lib/format';
import { axisLabels, bucketLabel } from '../../lib/overview';

/** Share of the plot height each series may use (uploads are an order of magnitude smaller). */
const DOWNLOAD_SHARE = 0.79;
const UPLOAD_SHARE = 0.13;

export function TrafficChart({ buckets, range }: { buckets: TrafficBucket[]; range: OverviewRange }) {
  const maxDown = Math.max(1, ...buckets.map((b) => b.downloads));
  const maxUp = Math.max(1, ...buckets.map((b) => b.uploads));
  const peak = buckets.reduce<TrafficBucket | null>((p, b) => (!p || b.downloads > p.downloads ? b : p), null);
  const summary = peak
    ? `Downloads and uploads per interval; busiest at ${bucketLabel(peak.start, range)} with ${formatCompact(peak.downloads)} downloads.`
    : 'No traffic recorded in this range.';

  return (
    <>
      <div className="traffic" role="img" aria-label={summary}>
        {buckets.map((b) => {
          const label = `${bucketLabel(b.start, range)} · ${formatCompact(b.downloads)} downloads · ${formatCompact(b.uploads)} uploads`;
          return (
            <div className="traffic__bar" key={b.start} title={label}>
              <div className="traffic__up" style={{ height: `${(b.uploads / maxUp) * UPLOAD_SHARE * 100}%` }} />
              <div className="traffic__down" style={{ height: `${(b.downloads / maxDown) * DOWNLOAD_SHARE * 100}%` }} />
            </div>
          );
        })}
      </div>
      <div className="traffic__axis" aria-hidden="true">
        {axisLabels(buckets, range).map((l, i) => (
          <span key={`${l}-${i}`}>{l}</span>
        ))}
      </div>
      <table className="visually-hidden">
        <caption>Traffic per interval</caption>
        <thead>
          <tr>
            <th scope="col">Interval start</th>
            <th scope="col">Downloads</th>
            <th scope="col">Uploads</th>
          </tr>
        </thead>
        <tbody>
          {buckets.map((b) => (
            <tr key={b.start}>
              <th scope="row">{bucketLabel(b.start, range)}</th>
              <td>{b.downloads}</td>
              <td>{b.uploads}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
