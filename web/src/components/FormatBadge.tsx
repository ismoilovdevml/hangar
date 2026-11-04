import type { Format } from '../api/types';
import { FORMAT_META } from '../lib/format';
import { FormatIcon } from './FormatIcon';

/** A rounded tile with the format's mark, named for screen readers. */
export function FormatBadge({ format, size = 32 }: { format: Format; size?: number }) {
  const meta = FORMAT_META[format];
  return (
    <span className="format-badge" style={{ width: size, height: size }} title={meta?.label ?? format}>
      <FormatIcon format={format} size={Math.round(size * 0.56)} label={null} />
      <span className="visually-hidden">{meta?.label ?? format}</span>
    </span>
  );
}
