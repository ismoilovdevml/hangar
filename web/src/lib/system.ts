import type { SystemConfig } from '../api/opsTypes';

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

/** Key names whose values are secrets even if a server forgot to mask them. */
const SECRET_KEY = /(password|passwd|secret|token|private[_-]?key|access[_-]?key[_-]?id|session[_-]?token|api[_-]?key|credential)/i;
const MASK = '••••••';

/** Settings about secrets rather than secrets themselves ("session_token_ttl", "max_token_lifetime"). */
const SECRET_SETTING = /(ttl|lifetime|length|name|header|interval|timeout|limit|policy)$/i;

export const isSecretKey = (key: string) => SECRET_KEY.test(key) && !SECRET_SETTING.test(key);

/** Masks the password in a URL with credentials ("postgres://u:p@h/db" → "postgres://u:••••••@h/db"). */
export function maskUrlPassword(v: string): string {
  return v.replace(/^([a-z][a-z0-9+.-]*:\/\/[^/:@\s]+):[^@/\s]+@/i, `$1:${MASK}@`);
}

export interface ConfigEntry {
  key: string;
  value: string;
  /** The value was masked (by the server or here). */
  secret: boolean;
  source?: 'default' | 'file' | 'env';
}

export interface ConfigSection {
  name: string;
  entries: ConfigEntry[];
}

function render(v: unknown): string {
  if (v === null || v === undefined || v === '') return '';
  if (Array.isArray(v)) return v.map((x) => (typeof x === 'object' ? JSON.stringify(x) : String(x))).join(', ');
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

/**
 * Turns the nested effective configuration into sections of dotted keys,
 * masking anything secret-looking as defence in depth (the server already
 * redacts). Values the server marked "REDACTED" are reported as secret.
 */
export function flattenConfig(cfg: SystemConfig): ConfigSection[] {
  const sections = new Map<string, ConfigEntry[]>();
  const walk = (node: unknown, path: string[]) => {
    if (node && typeof node === 'object' && !Array.isArray(node)) {
      for (const [k, v] of Object.entries(node as Record<string, unknown>)) walk(v, [...path, k]);
      return;
    }
    const key = path.join('.');
    const section = path.length > 1 ? path[0]! : 'general';
    let value = render(node);
    let secret = false;
    if (value && (isSecretKey(path[path.length - 1] ?? '') || value === 'REDACTED')) {
      value = MASK;
      secret = true;
    } else if (/:\/\//.test(value)) {
      const masked = maskUrlPassword(value).replace(/:REDACTED@/, `:${MASK}@`);
      secret = masked !== value;
      value = masked;
    }
    const list = sections.get(section) ?? [];
    list.push({ key, value, secret, source: cfg.sources?.[key] });
    sections.set(section, list);
  };
  walk(cfg.config, []);
  return [...sections.entries()].map(([name, entries]) => ({ name, entries }));
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

/** Reads a dotted path from the nested configuration. */
export function configValue(cfg: SystemConfig | undefined, path: string): unknown {
  let node: unknown = cfg?.config;
  for (const k of path.split('.')) {
    if (!node || typeof node !== 'object') return undefined;
    node = (node as Record<string, unknown>)[k];
  }
  return node;
}
