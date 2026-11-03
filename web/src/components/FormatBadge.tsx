import type { Format } from '../api/types';
import { FORMAT_META } from '../lib/format';

export function FormatBadge({ format, size = 32 }: { format: Format; size?: number }) {
  const meta = FORMAT_META[format];
  return (
    <span
      className={`format-badge tone-${meta.tone}`}
      style={{ width: size, height: size, fontSize: size >= 40 ? 11 : 10 }}
      title={meta.label}
    >
      <span aria-hidden="true">{meta.code}</span>
      <span className="visually-hidden">{meta.label}</span>
    </span>
  );
}
