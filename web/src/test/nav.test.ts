import { describe, expect, it } from 'vitest';
import { activeNav, NAV, SETTINGS_PAGES, visibleHub } from '../nav';

describe('sidebar', () => {
  it('holds only the everyday pages; configuration lives in Settings', () => {
    expect(NAV.map((s) => [s.title, s.items.map((i) => i.label)])).toEqual([
      ['Registry', ['Overview', 'Packages', 'Search', 'Repositories']],
      ['Storage', ['Blob stores', 'Cleanup policies']],
      ['Security', ['Vulnerabilities', 'Access & audit']],
      ['Operations', ['Scheduled tasks', 'Cluster & replication']],
    ]);
  });

  it.each([
    ['/', '/', true],
    ['/repositories/new', '/repositories', true],
    ['/repositories/npm/settings', '/repositories', true],
    ['/blob-stores/fs-local', '/blob-stores', true],
    ['/tasks/t_backup', '/tasks', true],
    ['/cluster/', '/cluster', true],
    ['/settings', '/cluster', false],
    ['/settings/system', '/cluster', false],
    ['/settings/proxies', '/cluster', false],
    ['/system/status', '/cluster', false],
    ['/system/logs', '/cluster', false],
    ['/settings/authentication', '/access', false],
    ['/account', '/access', false],
    ['/audit', '/access', false],
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
  it('keeps every page that left the sidebar reachable from the command palette', () => {
    const paths = SETTINGS_PAGES.items.map((i) => `${i.to}${i.hash ? `#${i.hash}` : ''}`);
    expect(paths).toEqual([
      '/settings',
      '/account',
      '/account#my-tokens-title',
      '/settings/authentication',
      '/settings/proxies',
      '/settings/system',
      '/system/status',
      '/system/logs',
      '/audit',
    ]);
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
