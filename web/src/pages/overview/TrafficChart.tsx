import type { OverviewRange, TrafficBucket } from '../../api/types';
import { formatCompact } from '../../lib/format';
import { axisLabels, bucketLabel } from '../../lib/overview';

/** Share of the plot height each series may use (uploads are an order of magnitude smaller). */
const DOWNLOAD_SHARE = 0.79;
const UPLOAD_SHARE = 0.13;
/** Uploads alone get the full height when downloads are not recorded. */
const UPLOAD_ONLY_SHARE = 0.9;

const count = (n: number | null) => (n === null ? 'not recorded' : formatCompact(n));

export function TrafficChart({ buckets, range }: { buckets: TrafficBucket[]; range: OverviewRange }) {
  if (buckets.length === 0) {
    return <p className="traffic traffic--empty meta">No traffic recorded in this range.</p>;
  }
  const hasDownloads = buckets.some((b) => b.downloads !== null);
  const maxDown = Math.max(1, ...buckets.map((b) => b.downloads ?? 0));
  const maxUp = Math.max(1, ...buckets.map((b) => b.uploads));
  const upShare = hasDownloads ? UPLOAD_SHARE : UPLOAD_ONLY_SHARE;
  const peak = buckets.reduce<TrafficBucket | null>(
    (p, b) => (b.downloads !== null && (!p || b.downloads > (p.downloads ?? 0)) ? b : p),
    null,
  );
  const uploads = buckets.reduce((n, b) => n + b.uploads, 0);
  const summary = hasDownloads
    ? peak && peak.downloads
      ? `Downloads and uploads per interval; busiest at ${bucketLabel(peak.start, range)} with ${formatCompact(peak.downloads)} downloads.`
      : `No downloads in this range; ${formatCompact(uploads)} uploads.`
    : `Uploads per interval: ${formatCompact(uploads)} in total. Download counts are not recorded for this range.`;

  return (
    <>
      <div className="traffic" role="img" aria-label={summary}>
        {buckets.map((b) => {
          const label = `${bucketLabel(b.start, range)} · ${count(b.downloads)} downloads · ${formatCompact(b.uploads)} uploads`;
          return (
            <div className="traffic__bar" key={b.start} title={label}>
              <div className="traffic__up" style={{ height: `${(b.uploads / maxUp) * upShare * 100}%` }} />
              {b.downloads !== null && (
                <div className="traffic__down" style={{ height: `${(b.downloads / maxDown) * DOWNLOAD_SHARE * 100}%` }} />
              )}
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
              <td>{b.downloads ?? 'not recorded'}</td>
              <td>{b.uploads}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
