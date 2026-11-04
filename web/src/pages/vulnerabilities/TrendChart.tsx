import type { FindingTrendPoint } from '../../api/types';
import { SEVERITY_META, trendDayLabel, trendSummary } from '../../lib/vulnerabilities';

const STACK = ['critical', 'high', 'medium', 'low'] as const;

/**
 * Unresolved findings at the end of each day, stacked by severity, with the
 * same numbers in a table for screen readers.
 */
export function TrendChart({ points }: { points: FindingTrendPoint[] }) {
  const summary = trendSummary(points);
  const total = (p: FindingTrendPoint) => p.critical + p.high + p.medium + p.low;
  const max = Math.max(1, ...points.map(total));
  const empty = points.every((p) => total(p) === 0 && p.new === 0 && p.resolved === 0);
  if (points.length === 0 || empty) {
    return <p className="vtrend vtrend--empty meta">No findings in the last {points.length || 30} days.</p>;
  }
  const ticks = [0, Math.floor((points.length - 1) / 2), points.length - 1];
  return (
    <>
      <div className="vtrend" role="img" aria-label={summary}>
        {points.map((p) => (
          <div
            className="vtrend__day"
            key={p.date}
            title={`${trendDayLabel(p.date)}: ${p.critical} critical, ${p.high} high, ${p.medium} medium, ${p.low} low · ${p.new} new, ${p.resolved} fixed`}
          >
            {STACK.map((s) =>
              p[s] > 0 ? (
                <div key={s} className="vtrend__seg" style={{ height: `${(p[s] / max) * 100}%`, background: SEVERITY_META[s].color }} />
              ) : null,
            )}
          </div>
        ))}
      </div>
      <div className="vtrend__axis" aria-hidden="true">
        {ticks.map((i) => (
          <span key={i}>{trendDayLabel(points[i]!.date)}</span>
        ))}
      </div>
      <ul className="vtrend__legend" aria-hidden="true">
        {STACK.map((s) => (
          <li key={s}>
            <span className="vtrend__swatch" style={{ background: SEVERITY_META[s].color }} />
            {SEVERITY_META[s].label}
          </li>
        ))}
      </ul>
      <table className="visually-hidden">
        <caption>Unresolved findings per day</caption>
        <thead>
          <tr>
            <th scope="col">Day</th>
            <th scope="col">Critical</th>
            <th scope="col">High</th>
            <th scope="col">Medium</th>
            <th scope="col">Low</th>
            <th scope="col">New</th>
            <th scope="col">Fixed</th>
          </tr>
        </thead>
        <tbody>
          {points.map((p) => (
            <tr key={p.date}>
              <th scope="row">{trendDayLabel(p.date)}</th>
              <td>{p.critical}</td>
              <td>{p.high}</td>
              <td>{p.medium}</td>
              <td>{p.low}</td>
              <td>{p.new}</td>
              <td>{p.resolved}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
