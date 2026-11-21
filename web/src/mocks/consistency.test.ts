/**
 * The demo's numbers agree across pages: the Overview, Blob stores,
 * Repositories, Search and Vulnerabilities read the same fixtures.
 */
import { describe, expect, it } from 'vitest';
import { catalogState } from './catalog';
import { blobStoreList, blobStores, overview, repositories } from './data';
import { seedFindings } from './findings';

describe('demo data', () => {
  const repos = repositories();
  const stored = repos.filter((r) => r.type !== 'group');
  const m = overview('today');

  it('stores every hosted and proxy repository on a store that counts it', () => {
    for (const b of blobStores) {
      const on = stored.filter((r) => r.blob_store === b.name);
      expect(b.repository_count, b.name).toBe(on.length);
      expect(b.used_bytes, b.name).toBe(on.reduce((n, r) => n + r.size_bytes, 0));
    }
    expect(blobStores.reduce((n, b) => n + b.repository_count, 0)).toBe(stored.length);
  });

  it('gives the Overview the blob stores’ total and the repositories’ formats', () => {
    expect(m.storage.used_bytes).toBe(blobStoreList().totals.used_bytes);
    expect(m.storage_by_format.reduce((n, f) => n + f.bytes, 0)).toBe(m.storage.used_bytes);
    // Not every store has a quota, so there is no capacity, as the server reports it.
    expect(m.storage.capacity_bytes).toBeNull();
    expect(m.storage.added_bytes_24h).toBe(blobStoreList().totals.growth_bytes_per_day);
  });

  it('counts the findings the Vulnerabilities page lists and the packages Search calls quarantined', () => {
    const open = seedFindings().filter((f) => f.status === 'open' || f.status === 'assigned');
    expect(m.issues.critical).toBe(open.filter((f) => f.severity === 'critical').length);
    expect(m.issues.open_findings).toBe(open.length);
    expect(m.issues.quarantined).toBe(catalogState.components.filter((c) => c.scan.state === 'quarantined').length);
    expect(m.issues).toMatchObject({ critical: 1, packages_affected: 1, quarantined: 1 });
  });
});
