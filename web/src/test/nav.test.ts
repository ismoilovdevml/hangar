import { describe, expect, it } from 'vitest';
import { activeNav, NAV, SETTINGS_PAGES, visibleHub } from '../nav';

describe('sidebar', () => {
  it('keeps every working page; only My account moved into Settings', () => {
    expect(NAV.map((s) => [s.title, s.items.map((i) => i.label)])).toEqual([
      ['Registry', ['Overview', 'Packages', 'Search', 'Repositories']],
      ['Storage', ['Blob stores', 'Cleanup policies']],
      ['Security', ['Vulnerabilities', 'Access & audit', 'Audit log']],
      ['Operations', ['Scheduled tasks', 'Cluster & replication', 'System status', 'System logs']],
      ['Settings', ['Settings', 'Authentication', 'Proxies']],
    ]);
  });

  it.each([
    ['/', '/', true],
    ['/repositories/new', '/repositories', true],
    ['/repositories/npm/settings', '/repositories', true],
    ['/blob-stores/fs-local', '/blob-stores', true],
    ['/tasks/t_backup', '/tasks', true],
    ['/cluster/', '/cluster', true],
    ['/settings', '/settings', true],
    ['/settings/system', '/settings', true],
    ['/settings/proxies', '/settings/proxies', true],
    ['/system/status', '/system/status', true],
    ['/system/logs', '/system/logs', true],
    ['/settings/authentication', '/settings/authentication', true],
    ['/account', '/settings', false],
    ['/api-docs', '/settings', false],
    ['/audit', '/audit', true],
  ])('lights up the right entry on %s', (path, to, own) => {
    expect(activeNav(path)).toEqual({ to, own });
  });

  it('lights up nothing on an unknown page', () => {
    expect(activeNav('/nowhere')).toBeUndefined();
    // A prefix of a word is not the same page.
    expect(activeNav('/searching')).toBeUndefined();
  });
});

describe('settings pages', () => {
  it('keeps the pages without a sidebar entry reachable from the command palette', () => {
    const paths = SETTINGS_PAGES.items.map((i) => `${i.to}${i.hash ? `#${i.hash}` : ''}`);
    expect(paths).toEqual(['/account', '/account#my-tokens-title', '/settings/system', '/api-docs']);
  });
});

describe('settings hub', () => {
  const ids = (privileges: string[]) => visibleHub(privileges).map((g) => g.entries.map((e) => e.id));

  it('shows administrators everything', () => {
    expect(ids(['system:*'])).toEqual([
      ['addresses', 'instance'],
      ['sign-in', 'internet', 'storage'],
      ['nodes', 'configuration', 'status', 'logs', 'audit'],
      ['account', 'tokens', 'api-docs'],
    ]);
  });

  it('shows a plain user only their account', () => {
    expect(ids(['repo:*:read'])).toEqual([['account', 'tokens', 'api-docs']]);
    expect(ids([])).toEqual([['account', 'tokens', 'api-docs']]);
  });

  it('follows each privilege', () => {
    expect(ids(['system:blobstores:read'])).toEqual([['storage'], ['account', 'tokens', 'api-docs']]);
    expect(ids(['system:cluster:admin'])).toEqual([
      ['nodes', 'status', 'logs'],
      ['account', 'tokens', 'api-docs'],
    ]);
    expect(ids(['system:settings:read'])[0]).toEqual(['addresses', 'instance']);
  });
});
