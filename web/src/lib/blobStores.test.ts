import { describe, expect, it } from 'vitest';
import type { BlobStore } from '../api/types';
import { dedupShare, humanDays, initialBlobStoreForm, storeBadge, toBlobStoreInput, validateBlobStoreForm } from './blobStores';

const store = (over: Partial<BlobStore>): BlobStore => ({
  id: 'b', name: 'b', kind: 'fs', state: 'ok', location: '/x', used_bytes: 0, repository_count: 0, blob_count: 0, ...over,
});

describe('blob store display', () => {
  it('flags stores near or over their soft quota, otherwise shows the reported state', () => {
    expect(storeBadge(store({ used_bytes: 95, soft_quota_bytes: 100 })).label).toBe('Almost full');
    expect(storeBadge(store({ used_bytes: 120, soft_quota_bytes: 100 })).label).toBe('Over quota');
    expect(storeBadge(store({ used_bytes: 10, soft_quota_bytes: 100 })).label).toBe('Healthy');
    expect(storeBadge(store({ state: 'read_only', used_bytes: 99, soft_quota_bytes: 100 })).label).toBe('Read-only');
    expect(storeBadge(store({ state: 'unknown' })).label).toBe('Not checked yet');
  });

  it('expresses dedup savings as a share of the raw size', () => {
    expect(dedupShare(78, 22)).toBeCloseTo(0.22);
    expect(dedupShare(0, 0)).toBeNull();
    expect(dedupShare(10, 0)).toBeNull();
  });

  it('says time to full in plain words', () => {
    expect(humanDays(0.3)).toBe('less than a day');
    expect(humanDays(12)).toBe('12 days');
    expect(humanDays(180)).toBe('about 6 months');
    expect(humanDays(900)).toBe('about 2 years');
  });
});

describe('new blob store form', () => {
  it('mirrors the server checks', () => {
    const f = { ...initialBlobStoreForm(), name: 'A', path: 'relative', quota: '-1' };
    expect(validateBlobStoreForm(f)).toEqual({
      name: expect.stringContaining('lowercase'),
      path: expect.stringContaining('absolute'),
      quota: expect.stringContaining('positive'),
    });
    const s3 = { ...initialBlobStoreForm(), kind: 's3' as const, name: 'ok-name', endpoint: 'http://u:p@minio:9000', accessKeyId: 'AK' };
    expect(validateBlobStoreForm(s3)).toEqual({
      bucket: expect.any(String),
      endpoint: expect.stringContaining('without credentials'),
      secretAccessKey: expect.any(String),
    });
  });

  it('builds the request body with the quota in bytes and the secret only with its key', () => {
    expect(toBlobStoreInput({ ...initialBlobStoreForm(), name: 'fs-two', path: ' /data/b ', quota: '1.5', quotaUnit: 'TB' })).toEqual({
      name: 'fs-two',
      kind: 'fs',
      soft_quota_bytes: 1.5e12,
      fs: { path: '/data/b' },
    });
    expect(
      toBlobStoreInput({ ...initialBlobStoreForm(), kind: 's3', name: 's3-a', bucket: 'bkt', region: 'eu-1', accessKeyId: 'AK', secretAccessKey: 's3cr3t', usePathStyle: true }),
    ).toEqual({
      name: 's3-a',
      kind: 's3',
      s3: { bucket: 'bkt', region: 'eu-1', use_path_style: true, access_key_id: 'AK', secret_access_key: 's3cr3t' },
    });
  });
});
