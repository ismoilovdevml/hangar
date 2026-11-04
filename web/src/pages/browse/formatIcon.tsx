/**
 * Format icon used by the package browser. A local stand-in: when the shared
 * components/FormatIcon lands, re-export it from here and every browse view
 * picks it up.
 */
import { FormatBadge } from '../../components/FormatBadge';
import { FORMAT_META } from '../../lib/format';
import { isFormat } from '../../lib/repositories';

export function FormatIcon({ format, size = 20 }: { format: string; size?: number }) {
  if (isFormat(format) && size >= 32) return <FormatBadge format={format} size={size} />;
  const meta = isFormat(format) ? FORMAT_META[format] : { label: format, code: format.slice(0, 3).toUpperCase(), tone: 'neutral' };
  // Tree rows are small: long codes get a smaller type so they stay inside the badge.
  const fontSize = meta.code.length > 3 ? 6.5 : 8;
  return (
    <span className={`format-badge tone-${meta.tone}`} style={{ width: size, height: size, fontSize, letterSpacing: 0 }} title={meta.label}>
      <span aria-hidden="true">{meta.code}</span>
      <span className="visually-hidden">{meta.label}</span>
    </span>
  );
}
