import type { ScanState, ScanSummary, SearchHit } from '../api/types';
import type { Tone } from './format';
import { componentName, unsupportedText } from './overview';

/** A search hit's package name in its format's idiom (older servers send only coordinates). */
export const hitName = (h: SearchHit) => componentName({ format: h.format, namespace: h.namespace, name: h.name ?? h.coordinates });

/** Raw files have no version; show a dash instead of an empty pill. */
export const hasVersion = (v: string | undefined): v is string => !!v && v.length > 0;

/**
 * Asset links point at the server's base URL. The web UI is served by the same
 * server, so a same-origin path carries the session cookie and downloads with
 * the signed-in user's permissions; other URLs are left as they are.
 */
export function downloadHref(url: string): string {
  try {
    const u = new URL(url);
    if (u.pathname.startsWith('/repository/')) return `${u.pathname}${u.search}`;
  } catch {
    // relative already
  }
  return url;
}

/**
 * The security state as a pill, with the worst severity spelled out. Search
 * hits carry only the state; "pending" also covers servers without a scanner.
 */
export function securityPill(scan: ScanSummary | ScanState, format?: string): { label: string; tone: Tone } {
  const s: ScanSummary = typeof scan === 'string' ? { state: scan, critical: 0, high: 0, medium: 0, low: 0 } : scan;
  switch (s.state) {
    case 'clean':
      return { label: 'Clean', tone: 'ok' };
    case 'pending':
      return { label: 'Not scanned', tone: 'neutral' };
    case 'failed':
      return { label: 'Scan failed', tone: 'warn' };
    case 'unsupported':
      return { label: unsupportedText(s.scan_error, format), tone: 'neutral' };
    case 'quarantined':
      return { label: 'Quarantined', tone: 'bad' };
    case 'findings':
      if (s.critical) return { label: `${s.critical} critical`, tone: 'bad' };
      if (s.high) return { label: `${s.high} high`, tone: 'warn' };
      if (s.medium) return { label: `${s.medium} medium`, tone: 'warn' };
      if (s.low) return { label: `${s.low} low`, tone: 'neutral' };
      return { label: 'Findings', tone: 'warn' };
  }
}

/* ---------------------------------------------------------------- browse tabs */

export const BROWSE_TABS = ['readme', 'versions', 'files', 'repositories', 'dependents', 'security'] as const;
export type BrowseTab = (typeof BROWSE_TABS)[number];
export const isBrowseTab = (v: unknown): v is BrowseTab => BROWSE_TABS.includes(v as BrowseTab);
