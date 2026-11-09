export const DEPENDENCY_LABELS: Record<string, string> = {
  database: 'PostgreSQL',
  blobstore: 'Blob storage',
  eventbus: 'Event bus (NATS)',
  search: 'Search index',
  scanner: 'Vulnerability scanner',
};

export function dependencyLabel(name: string): string {
  return DEPENDENCY_LABELS[name] ?? name;
}

/** 93_784 → "1 d 2 h", 3_720 → "1 h 2 min", 45 → "45 s". */
export function formatUptime(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  const d = Math.floor(s / 86_400);
  const h = Math.floor((s % 86_400) / 3_600);
  const m = Math.floor((s % 3_600) / 60);
  if (d > 0) return h ? `${d} d ${h} h` : `${d} d`;
  if (h > 0) return m ? `${h} h ${m} min` : `${h} h`;
  if (m > 0) return `${m} min`;
  return `${s} s`;
}

/** Host (and port) Docker clients log in to, from the base URL. */
export function hostOf(url: string | undefined): string | undefined {
  if (!url) return undefined;
  try {
    return new URL(url).host || undefined;
  } catch {
    return undefined;
  }
}
