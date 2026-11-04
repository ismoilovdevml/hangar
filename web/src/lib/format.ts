import type { Format, OverviewRange } from '../api/types';

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'] as const;

/** Splits a byte count into a display value and unit using decimal (SI) units. */
export function splitBytes(bytes: number): { value: string; unit: string } {
  let v = Math.max(0, bytes);
  let i = 0;
  while (v >= 1000 && i < UNITS.length - 1) {
    v /= 1000;
    i++;
  }
  const digits = v >= 100 || i === 0 ? 0 : v >= 10 ? 1 : 2;
  return { value: Number(v.toFixed(digits)).toString(), unit: UNITS[i] ?? 'B' };
}

export function formatBytes(bytes: number): string {
  const { value, unit } = splitBytes(bytes);
  return `${value} ${unit}`;
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

export function formatSignedPercent(pct: number): string {
  const rounded = Math.round(pct);
  return `${rounded > 0 ? '+' : rounded < 0 ? '−' : '±'}${Math.abs(rounded)}%`;
}

export function relativeTime(iso: string, now: Date = new Date()): string {
  const diff = Math.max(0, now.getTime() - new Date(iso).getTime());
  const min = Math.floor(diff / 60_000);
  if (min < 1) return 'Just now';
  if (min < 60) return `${min} min ago`;
  const h = Math.floor(min / 60);
  if (h < 24) return `${h} h ago`;
  const d = Math.floor(h / 24);
  return d === 1 ? 'Yesterday' : `${d} days ago`;
}

export function greeting(now: Date = new Date()): string {
  const h = now.getHours();
  if (h < 5) return 'Good evening';
  if (h < 12) return 'Good morning';
  if (h < 18) return 'Good afternoon';
  return 'Good evening';
}

export function longDate(now: Date = new Date()): string {
  const part = (o: Intl.DateTimeFormatOptions) => new Intl.DateTimeFormat('en-GB', o).format(now);
  return `${part({ weekday: 'long' })}, ${part({ day: 'numeric' })} ${part({ month: 'long' })}`;
}

export function firstName(displayName: string): string {
  return displayName.trim().split(/\s+/)[0] ?? displayName;
}

/** How the instance is named in the shell: the unconfigured default reads as the product name. */
export function instanceName(name: string | undefined | null): string {
  const n = name?.trim() ?? '';
  return n === '' || n.toLowerCase() === 'hangar' ? 'Hangar' : n;
}

export function initials(displayName: string): string {
  const parts = displayName.trim().split(/\s+/).filter(Boolean);
  const letters = parts.length > 1 ? `${parts[0]![0]}${parts[parts.length - 1]![0]}` : (parts[0] ?? '?').slice(0, 2);
  return letters.toUpperCase();
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
  raw: { label: 'Raw', code: 'RAW', tone: 'neutral', bar: 'var(--fmt-raw)' },
};
