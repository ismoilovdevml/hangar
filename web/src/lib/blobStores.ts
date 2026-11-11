import type { BlobStore, BlobStoreInput, BlobStoreState } from '../api/types';
import type { Tone } from './format';

export const STORE_STATE: Record<BlobStoreState, { label: string; tone: Extract<Tone, 'ok' | 'warn' | 'bad' | 'neutral'> }> = {
  ok: { label: 'Healthy', tone: 'ok' },
  degraded: { label: 'Degraded', tone: 'warn' },
  unavailable: { label: 'Unavailable', tone: 'bad' },
  read_only: { label: 'Read-only', tone: 'neutral' },
  unknown: { label: 'Not checked yet', tone: 'neutral' },
};

/** What the last health check found, for a store that is not healthy. */
export function storeProblem(s: Pick<BlobStore, 'state' | 'state_detail'>): string | null {
  if (s.state !== 'unavailable' && s.state !== 'degraded') return null;
  return s.state_detail || (s.state === 'unavailable' ? 'The last health check failed' : 'Low on free space');
}

/** Share of the soft quota in use, or null without a quota. */
export function quotaRatio(s: Pick<BlobStore, 'used_bytes' | 'soft_quota_bytes'>): number | null {
  const q = s.soft_quota_bytes;
  return q && q > 0 ? s.used_bytes / q : null;
}

export const ALMOST_FULL = 0.9;

/** The state pill: the reported state, or "Over quota"/"Almost full" for a healthy store near its quota. */
export function storeBadge(s: BlobStore): { label: string; tone: Tone } {
  const r = quotaRatio(s);
  if ((s.state === 'ok' || s.state === 'unknown') && r !== null) {
    if (r >= 1) return { label: 'Over quota', tone: 'bad' };
    if (r >= ALMOST_FULL) return { label: 'Almost full', tone: 'warn' };
  }
  return STORE_STATE[s.state];
}

/** Dedup savings as a share of what the content would take without dedup. */
export function dedupShare(used: number, saved: number): number | null {
  const raw = used + saved;
  return raw > 0 && saved > 0 ? saved / raw : null;
}

/** "about 6 months", "12 days", "less than a day". */
export function humanDays(days: number): string {
  if (days < 1) return 'less than a day';
  if (days < 45) return `${Math.round(days)} day${Math.round(days) === 1 ? '' : 's'}`;
  if (days < 548) return `about ${Math.round(days / 30.4)} months`;
  return `about ${Math.round(days / 365)} years`;
}

/* ---------------------------------------------------------------- create form */

export type StoreKind = BlobStoreInput['kind'];
export type QuotaUnit = 'GB' | 'TB';

export const QUOTA_UNITS: { value: QuotaUnit; label: string }[] = [
  { value: 'GB', label: 'GB' },
  { value: 'TB', label: 'TB' },
];

export interface BlobStoreForm {
  name: string;
  kind: StoreKind;
  quota: string;
  quotaUnit: QuotaUnit;
  path: string;
  endpoint: string;
  region: string;
  bucket: string;
  prefix: string;
  accessKeyId: string;
  secretAccessKey: string;
  usePathStyle: boolean;
}

export const initialBlobStoreForm = (): BlobStoreForm => ({
  name: '',
  kind: 'fs',
  quota: '',
  quotaUnit: 'TB',
  path: '',
  endpoint: '',
  region: '',
  bucket: '',
  prefix: '',
  accessKeyId: '',
  secretAccessKey: '',
  usePathStyle: false,
});

export type BlobStoreField = 'name' | 'quota' | 'path' | 'endpoint' | 'bucket' | 'accessKeyId' | 'secretAccessKey';
export type BlobStoreErrors = Partial<Record<BlobStoreField, string>>;

export const STORE_NAME = /^[a-z0-9][a-z0-9-]{1,62}$/;
const UNIT: Record<QuotaUnit, number> = { GB: 1e9, TB: 1e12 };

/** Mirrors the server's checks (catalog.NewBlobStore.Validate); the server stays the authority. */
export function validateBlobStoreForm(f: BlobStoreForm): BlobStoreErrors {
  const e: BlobStoreErrors = {};
  if (!f.name) e.name = 'Enter a name.';
  else if (!STORE_NAME.test(f.name)) e.name = 'Use 2–63 lowercase letters, digits and dashes, starting with a letter or digit.';
  if (f.quota.trim()) {
    const n = Number(f.quota);
    if (!Number.isFinite(n) || n <= 0) e.quota = 'Enter a positive number, or leave empty for no quota.';
  }
  if (f.kind === 'fs') {
    if (!f.path.trim()) e.path = 'Enter the directory on the server.';
    else if (!f.path.trim().startsWith('/')) e.path = 'Use an absolute path, e.g. /var/lib/hangar/blobs.';
  } else {
    if (!f.bucket.trim()) e.bucket = 'Enter the bucket name.';
    const ep = f.endpoint.trim();
    if (ep) {
      let ok = false;
      try {
        const u = new URL(ep);
        ok = (u.protocol === 'http:' || u.protocol === 'https:') && !!u.host && !u.username && !u.password;
      } catch {
        ok = false;
      }
      if (!ok) e.endpoint = 'Use an http:// or https:// address without credentials.';
    }
    if (f.accessKeyId.trim() && !f.secretAccessKey) e.secretAccessKey = 'Enter the secret for this access key.';
    if (!f.accessKeyId.trim() && f.secretAccessKey) e.accessKeyId = 'Enter the access key id for this secret.';
  }
  return e;
}

/** A typed quota in bytes: null for empty (no quota), NaN when it is not a positive number. */
export function quotaBytes(value: string, unit: QuotaUnit): number | null {
  if (!value.trim()) return null;
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? Math.round(n * UNIT[unit]) : NaN;
}

/** The largest unit that shows a quota without a long fraction (for pre-filling the editor). */
export function quotaToForm(bytes: number | null | undefined): { quota: string; quotaUnit: QuotaUnit } {
  if (!bytes) return { quota: '', quotaUnit: 'TB' };
  const unit: QuotaUnit = bytes >= 1e12 ? 'TB' : 'GB';
  return { quota: String(Number((bytes / UNIT[unit]).toFixed(3))), quotaUnit: unit };
}

/** The request body for POST /blob-stores. Includes the secret; never log it. */
export function toBlobStoreInput(f: BlobStoreForm): BlobStoreInput {
  const input: BlobStoreInput = { name: f.name, kind: f.kind };
  if (f.quota.trim()) input.soft_quota_bytes = Math.round(Number(f.quota) * UNIT[f.quotaUnit]);
  if (f.kind === 'fs') input.fs = { path: f.path.trim() };
  else {
    input.s3 = { bucket: f.bucket.trim(), use_path_style: f.usePathStyle };
    if (f.endpoint.trim()) input.s3.endpoint = f.endpoint.trim();
    if (f.region.trim()) input.s3.region = f.region.trim();
    if (f.prefix.trim()) input.s3.prefix = f.prefix.trim();
    if (f.accessKeyId.trim()) {
      input.s3.access_key_id = f.accessKeyId.trim();
      input.s3.secret_access_key = f.secretAccessKey;
    }
  }
  return input;
}
