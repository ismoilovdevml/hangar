export interface MeterProps {
  /** 0..1 */
  value: number;
  label: string;
  /** Fixed fill colour; when omitted the fill turns warm above `warnAbove`. */
  color?: string;
  warnAbove?: number;
  height?: number;
  /** Human text for assistive tech, e.g. "4.82 TB of 12 TB". */
  valueText?: string;
}

export function Meter({ value, label, color, warnAbove = 0.75, height = 6, valueText }: MeterProps) {
  const v = Math.min(1, Math.max(0, Number.isFinite(value) ? value : 0));
  const fill = color ?? (v > warnAbove ? 'var(--chart-2)' : 'var(--chart-3)');
  return (
    <div
      className="meter"
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(v * 100)}
      aria-valuetext={valueText}
      style={{ height, borderRadius: height / 2 }}
    >
      <div className="meter__fill" style={{ width: `${v * 100}%`, background: fill, borderRadius: height / 2 }} />
    </div>
  );
}
