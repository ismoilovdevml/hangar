import { describe, expect, it } from 'vitest';
import { ACCOUNT_PAGES, activeNav, canSee, NAV, SYSTEM_TABS, visibleNav } from '../nav';

describe('sidebar', () => {
  it('has one entry per area: System gathers status, nodes and logs, the audit log is one page', () => {
    expect(NAV.map((s) => [s.title, s.items.map((i) => i.label)])).toEqual([
      ['Registry', ['Overview', 'Packages', 'Search', 'Repositories']],
      ['Storage', ['Blob stores', 'Cleanup policies']],
      ['Security', ['Vulnerabilities', 'Access', 'Audit log']],
      ['Operations', ['Scheduled tasks', 'System']],
      ['Settings', ['Authentication', 'Proxies']],
    ]);
  });

  it('shows a plain user System, but no Settings section', () => {
    expect(visibleNav(['repo:*:read']).map((s) => [s.title, s.items.map((i) => i.label)])).toEqual([
      ['Registry', ['Overview', 'Packages', 'Search', 'Repositories']],
      ['Security', ['Vulnerabilities']],
      ['Operations', ['System']],
    ]);
  });

  it.each([
    ['/', '/'],
    ['/repositories/new', '/repositories'],
    ['/repositories/npm/settings', '/repositories'],
    ['/blob-stores/fs-local', '/blob-stores'],
    ['/tasks/t_backup', '/tasks'],
    ['/system', '/system'],
    ['/system/status', '/system'],
    ['/system/logs/', '/system'],
    ['/settings/proxies', '/settings/proxies'],
    ['/settings/authentication', '/settings/authentication'],
    ['/audit', '/audit'],
  ])('lights up the right entry on %s', (path, to) => {
    expect(activeNav(path)).toBe(to);
  });

  it('lights up nothing on a page without an entry', () => {
    expect(activeNav('/nowhere')).toBeUndefined();
    expect(activeNav('/account')).toBeUndefined();
    // A prefix of a word is not the same page.
    expect(activeNav('/searching')).toBeUndefined();
  });
});

describe('system tabs', () => {
  const tabs = (privileges: string[] | undefined) => SYSTEM_TABS.filter((t) => canSee(privileges, t.requires)).map((t) => t.label);

  it('follows the cluster privileges; About is for everyone', () => {
    expect(tabs(['system:*'])).toEqual(['Status', 'Nodes', 'Logs', 'About']);
    expect(tabs(['system:cluster:read'])).toEqual(['Status', 'Nodes', 'About']);
    expect(tabs(['repo:*:read'])).toEqual(['About']);
    // A server that does not report privileges gets no client-side gating.
    expect(tabs(undefined)).toEqual(['Status', 'Nodes', 'Logs', 'About']);
  });
});

describe('account pages', () => {
  it('keeps the pages without a sidebar entry reachable from the command palette', () => {
    const paths = ACCOUNT_PAGES.items.map((i) => `${i.to}${i.hash ? `#${i.hash}` : ''}`);
    expect(paths).toEqual(['/account', '/account#my-tokens-title', '/api-docs']);
  });
});
