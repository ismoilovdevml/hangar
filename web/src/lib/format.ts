import type { Format, OverviewRange } from '../api/types';

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'] as const;

/** Splits a byte count into a display value and unit, in binary (IEC) units: 1536 → 1.5 KiB. */
export function splitBytes(bytes: number): { value: string; unit: string } {
  let v = Math.max(0, bytes);
  let i = 0;
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  const digits = v >= 100 || i === 0 ? 0 : v >= 10 ? 1 : 2;
  return { value: Number(v.toFixed(digits)).toString(), unit: UNITS[i] ?? 'B' };
}

/** A byte count for people: "231 MiB", "1.5 KiB", "0 B". */
export function formatBytes(bytes: number): string {
  const { value, unit } = splitBytes(bytes);
  return `${value} ${unit}`;
}

const COUNT = new Intl.NumberFormat('en-US', { maximumFractionDigits: 0 });

/** A count in full with thousands separators, the one way to show one: 76992 → "76,992". */
export function formatCount(n: number): string {
  return COUNT.format(n);
}

/** 18_400_000 → "18.4M", 1200 → "1.2K". */
export function formatCompact(n: number): string {
  const abs = Math.abs(n);
  const fmt = (v: number, s: string) => `${Number(v.toFixed(v >= 100 ? 0 : 1))}${s}`;
  if (abs >= 1e9) return fmt(n / 1e9, 'B');
  if (abs >= 1e6) return fmt(n / 1e6, 'M');
  if (abs >= 1e3) return fmt(n / 1e3, 'K');
  return String(n);
}

export function formatPercent(ratio: number, digits = 1): string {
  return (ratio * 100).toFixed(digits);
}

/** A share as a whole percentage that never reads "0%" for something: 0.002 → "<1%", 0 → "0%". */
export function formatShare(ratio: number): string {
  if (ratio > 0 && ratio < 0.005) return '<1%';
  if (ratio < 1 && ratio > 0.995) return '>99%';
  return `${Math.round(ratio * 100)}%`;
}

export function formatSignedPercent(pct: number): string {
  const rounded = Math.round(pct);
  return `${rounded > 0 ? '+' : rounded < 0 ? '−' : '±'}${Math.abs(rounded)}%`;
}

/* ---------------------------------------------------------------- time
 *
 * Every timestamp is shown in the browser's time zone with the zone spelled
 * out ("11 Nov 2026, 12:23 (UTC+5)"); the same instant in UTC is the hover
 * text. Exports and the API stay in UTC (RFC 3339).
 */

const MONTH = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'] as const;
const WEEKDAY = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'] as const;
const pad = (n: number) => String(n).padStart(2, '0');

type When = string | number | Date;

function toDate(v: When): Date | null {
  const d = v instanceof Date ? v : new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** The browser's offset from UTC at `d` as a label: "UTC+5", "UTC+5:30", "UTC-3", "UTC". */
export function zoneLabel(d: Date = new Date()): string {
  const east = -d.getTimezoneOffset();
  if (east === 0) return 'UTC';
  const abs = Math.abs(east);
  const mins = abs % 60;
  return `UTC${east > 0 ? '+' : '-'}${Math.floor(abs / 60)}${mins ? `:${pad(mins)}` : ''}`;
}

/** Minutes east of UTC in the browser, for APIs that count days in the viewer's zone. */
export function tzOffsetMinutes(d: Date = new Date()): number {
  return -d.getTimezoneOffset() || 0;
}

/** Local time of day: "12:23", "12:23:05" or "12:23:05.123". */
export function formatClock(v: When, opts: { seconds?: boolean; ms?: boolean } = {}): string {
  const d = toDate(v);
  if (!d) return String(v);
  let t = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (opts.seconds || opts.ms) t += `:${pad(d.getSeconds())}`;
  if (opts.ms) t += `.${String(d.getMilliseconds()).padStart(3, '0')}`;
  return t;
}

/** Local calendar day: "11 Nov 2026". */
export function formatDate(v: When): string {
  const d = toDate(v);
  if (!d) return String(v);
  return `${d.getDate()} ${MONTH[d.getMonth()]} ${d.getFullYear()}`;
}

/**
 * The one way to show an instant: local date and time with the zone,
 * "11 Nov 2026, 12:23 (UTC+5)". `zone: false` leaves the zone out where the
 * page already names it once.
 */
export function formatTime(v: When, opts: { seconds?: boolean; zone?: boolean } = {}): string {
  const d = toDate(v);
  if (!d) return String(v);
  const text = `${formatDate(d)}, ${formatClock(d, { seconds: opts.seconds })}`;
  return opts.zone === false ? text : `${text} (${zoneLabel(d)})`;
}

/**
 * Server-written text with RFC 3339 instants in it ("No heartbeat since
 * 2026-11-15T08:34:39.547Z"), with each instant as formatTime shows it.
 */
export function localTimesInText(text: string): string {
  return text.replace(/\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})/g, (iso) => formatTime(iso));
}

/** A calendar day sent without a time ("2026-10-26") as "26 Oct": the day as written, in no zone. */
export function formatDayMonth(day: string): string {
  const m = /^\d{4}-(\d{2})-(\d{2})$/.exec(day);
  const month = m ? MONTH[Number(m[1]) - 1] : undefined;
  return m && month ? `${Number(m[2])} ${month}` : day;
}

/** The same instant in UTC, for hover text: "11 Nov 2026, 07:23:05 UTC". */
export function formatUtc(v: When, opts: { seconds?: boolean } = { seconds: true }): string {
  const d = toDate(v);
  if (!d) return String(v);
  const clock = `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}${opts.seconds ? `:${pad(d.getUTCSeconds())}` : ''}`;
  return `${d.getUTCDate()} ${MONTH[d.getUTCMonth()]} ${d.getUTCFullYear()}, ${clock} UTC`;
}

const sameLocalDay = (a: Date, b: Date) => a.toDateString() === b.toDateString();

/** Local time of day for times today ("17:37"), with the day otherwise ("3 Nov 17:37"). */
export function formatShortTime(v: When, now: Date = new Date()): string {
  const d = toDate(v);
  if (!d) return String(v);
  const clock = formatClock(d);
  return sameLocalDay(d, now) ? clock : `${d.getDate()} ${MONTH[d.getMonth()]} ${clock}`;
}

/** How long ago, in sentence case: "Just now", "4 min ago", "3 h ago", "Yesterday", "5 days ago". */
export function relativeTime(iso: When, now: Date = new Date()): string {
  const d = toDate(iso);
  if (!d) return String(iso);
  const diff = Math.max(0, now.getTime() - d.getTime());
  const min = Math.floor(diff / 60_000);
  if (min < 1) return 'Just now';
  if (min < 60) return `${min} min ago`;
  const h = Math.floor(min / 60);
  if (h < 24) return `${h} h ago`;
  const days = Math.floor(h / 24);
  return days === 1 ? 'Yesterday' : `${days} days ago`;
}

/** How far ahead, in sentence case: "In 14 min", "Today 22:00", "Tomorrow 02:00", "Sun 03:00", "12 Nov 03:00" (local). */
export function relativeFuture(v: When, now: Date = new Date()): string {
  const d = toDate(v);
  if (!d) return String(v);
  const diff = d.getTime() - now.getTime();
  if (diff <= 60_000) return 'Due now';
  if (diff < 60 * 60_000) return `In ${Math.round(diff / 60_000)} min`;
  const dayOf = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const days = Math.round((dayOf(d) - dayOf(now)) / 86_400_000);
  const clock = formatClock(d);
  if (days === 0) return `Today ${clock}`;
  if (days === 1) return `Tomorrow ${clock}`;
  if (days < 7) return `${WEEKDAY[d.getDay()]} ${clock}`;
  return `${d.getDate()} ${MONTH[d.getMonth()]} ${clock}`;
}

export function longDate(now: Date = new Date()): string {
  return `${['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'][now.getDay()]}, ${now.getDate()} ${
    ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'][now.getMonth()]
  }`;
}

/** 75_000 → "1 min 15 s", 4_000 → "4 s", 3_900_000 → "1 h 5 min", 93_784_000 → "1 d 2 h". */
export function formatDuration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return s % 60 && m < 10 ? `${m} min ${s % 60} s` : `${m} min`;
  const h = Math.floor(m / 60);
  if (h < 24) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
  const d = Math.floor(h / 24);
  return h % 24 ? `${d} d ${h % 24} h` : `${d} d`;
}

/**
 * A Go duration in its shortest form: "24h0m0s" → "24h", "1h30m0s" → "1h30m",
 * "10m0s" → "10m", "0s" → "0s". Anything else comes back unchanged.
 */
export function shortGoDuration(v: string): string {
  const t = v.trim();
  if (!/^(\d+h)?(\d+m(?!s))?(\d+(\.\d+)?s)?$/.test(t) || t === '') return v;
  const short = t.replace(/(^|[hm])0m(?!s)/, '$1').replace(/([hm])0s$/, '$1');
  return short === '' ? '0s' : short;
}

/** How the instance is named in the shell: the unconfigured default reads as the product name. */
export function instanceName(name: string | undefined | null): string {
  const n = name?.trim() ?? '';
  return n === '' || n.toLowerCase() === 'hangar' ? 'Hangar' : n;
}

/**
 * Two letters for an avatar. Names are split on spaces and on the separators
 * usernames use (`-`, `_`, `.`), so "qa-bob" and "qa-help" read "QB" and "QH"
 * rather than both "QA"; an email address counts only up to the "@".
 */
export function initials(displayName: string): string {
  const name = displayName.trim().replace(/@.*$/, '');
  const parts = name.split(/[\s._-]+/).filter(Boolean);
  const letters = parts.length > 1 ? `${parts[0]![0]}${parts[parts.length - 1]![0]}` : (parts[0] ?? '?').slice(0, 2);
  return letters.toUpperCase();
}

/** "v0.1.6" for a release, whether or not the server already wrote the "v"; other builds ("dev") as they are. */
export function versionText(v: string): string {
  const t = v.trim();
  if (/^[vV]\d/.test(t)) return `v${t.slice(1)}`;
  return /^\d/.test(t) ? `v${t}` : t;
}

export const RANGES: { id: OverviewRange; label: string; title: string; requests: string }[] = [
  { id: '1h', label: '1h', title: 'Traffic · last hour', requests: 'Requests · last hour' },
  { id: 'today', label: 'Today', title: 'Traffic today', requests: 'Requests today' },
  { id: '7d', label: '7 days', title: 'Traffic · 7 days', requests: 'Requests · 7 days' },
  { id: '30d', label: '30 days', title: 'Traffic · 30 days', requests: 'Requests · 30 days' },
];

export function isRange(v: unknown): v is OverviewRange {
  return RANGES.some((r) => r.id === v);
}

export type Tone = 'ok' | 'warn' | 'bad' | 'info' | 'peach' | 'violet' | 'sage' | 'neutral';

export const FORMAT_META: Record<Format, { label: string; code: string; tone: Tone; bar: string }> = {
  docker: { label: 'Docker', code: 'OCI', tone: 'info', bar: 'var(--fmt-docker)' },
  npm: { label: 'npm', code: 'NPM', tone: 'peach', bar: 'var(--fmt-npm)' },
  maven: { label: 'Maven', code: 'MVN', tone: 'violet', bar: 'var(--fmt-maven)' },
  pypi: { label: 'PyPI', code: 'PY', tone: 'sage', bar: 'var(--fmt-pypi)' },
  go: { label: 'Go', code: 'GO', tone: 'info', bar: 'var(--fmt-go)' },
  helm: { label: 'Helm', code: 'HELM', tone: 'info', bar: 'var(--fmt-helm)' },
  nuget: { label: 'NuGet', code: 'NUGET', tone: 'violet', bar: 'var(--fmt-nuget)' },
  cargo: { label: 'Cargo', code: 'CRATE', tone: 'peach', bar: 'var(--fmt-cargo)' },
  apt: { label: 'APT', code: 'APT', tone: 'neutral', bar: 'var(--fmt-apt)' },
  rpm: { label: 'RPM', code: 'RPM', tone: 'neutral', bar: 'var(--fmt-rpm)' },
  raw: { label: 'Raw', code: 'RAW', tone: 'neutral', bar: 'var(--fmt-raw)' },
};
