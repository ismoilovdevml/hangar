import { formatDate, formatShortTime, formatTime, formatUtc, relativeFuture, relativeTime } from '../lib/format';

type Mode = 'datetime' | 'date' | 'short' | 'relative' | 'future';

interface TimeProps {
  /** An RFC 3339 timestamp from the API. */
  value: string;
  /**
   * datetime: "11 Nov 2026, 12:23 (UTC+5)"; date: "11 Nov 2026"; short:
   * "12:23" today, "3 Nov 12:23" before; relative: "4 min ago"; future:
   * "In 14 min", "Tomorrow 02:00".
   */
  mode?: Mode;
  seconds?: boolean;
  /** datetime only: leave the zone out where the page already names it. */
  zone?: boolean;
  now?: Date;
  className?: string;
  /** Text before the hover's times, e.g. "Finished". */
  label?: string;
  /** Lower-case the first letter of a relative time inside a sentence. */
  inline?: boolean;
}

/**
 * An instant as the browser's local time (see lib/format). The hover text
 * gives it in full: the local time with its zone, and the same instant in UTC.
 */
export function Time({ value, mode = 'datetime', seconds, zone, now, className, label, inline }: TimeProps) {
  let text: string;
  switch (mode) {
    case 'date':
      text = formatDate(value);
      break;
    case 'short':
      text = formatShortTime(value, now);
      break;
    case 'relative':
      text = relativeTime(value, now);
      break;
    case 'future':
      text = relativeFuture(value, now);
      break;
    default:
      text = formatTime(value, { seconds, zone });
  }
  if (inline) text = text.charAt(0).toLowerCase() + text.slice(1);
  const full = mode === 'datetime' && zone !== false ? '' : `${formatTime(value, { seconds: true })} · `;
  const title = `${label ? `${label} ` : ''}${full}${formatUtc(value)}`;
  return (
    <time dateTime={value} title={title} className={className}>
      {text}
    </time>
  );
}
