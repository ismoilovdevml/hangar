import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

describe('command palette', () => {
  it('opens with ⌘K, filters, and navigates with the keyboard', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/');
    await screen.findByRole('navigation', { name: 'Primary' });

    await user.keyboard('{Meta>}k{/Meta}');
    const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
    const input = within(dialog).getByRole('combobox');
    expect(input).toHaveFocus();

    await user.type(input, 'cluster');
    const options = within(dialog).getAllByRole('option');
    // A matching page ranks first; the package search is the fallback at the end.
    expect(options[0]).toHaveTextContent('Cluster & replication');
    expect(options.at(-1)).toHaveTextContent('Search packages for “cluster”');
    // ArrowUp at the top stays there instead of wrapping to the last option.
    await user.keyboard('{ArrowUp}');
    expect(within(dialog).getByRole('option', { name: 'Cluster & replication' })).toHaveAttribute('aria-selected', 'true');
    await user.keyboard('{Enter}');

    await waitFor(() => expect(history.location.pathname).toBe('/cluster'));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('opens with Ctrl+K and the search button, and closes with Escape', async () => {
    const user = userEvent.setup();
    // The Overview has its own search box; every other page has the search button.
    renderApp('/repositories');
    const trigger = await screen.findByRole('button', { name: /Search packages, digests, CVEs/ });

    await user.keyboard('{Control>}k{/Control}');
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

    await user.click(trigger);
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(trigger).toHaveFocus());
  });

  it.each([
    ['proxies', 'Proxies', '/settings/proxies', ''],
    ['sso', 'Authentication', '/settings/authentication', ''],
    ['tokens', 'Access tokens', '/account', '#my-tokens-title'],
    ['profile', 'My account', '/account', ''],
    ['health', 'System status', '/system/status', ''],
    ['errors', 'System logs', '/system/logs', ''],
    ['compliance', 'Audit log', '/audit', ''],
    ['hangar.yaml', 'System', '/settings/system', ''],
    ['preferences', 'Settings', '/settings', ''],
  ])('finds pages that left the sidebar: "%s" opens %s', async (query, label, path, hash) => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories');
    await screen.findByRole('navigation', { name: 'Primary' });
    await user.keyboard('{Control>}k{/Control}');
    const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
    await user.type(within(dialog).getByRole('combobox'), query);
    await user.click(within(dialog).getByRole('option', { name: label }));
    await waitFor(() => expect(history.location.pathname).toBe(path));
    expect(history.location.hash).toBe(hash);
  });

  it('lists only the pages the user may open', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read'] }, { headers: { 'X-CSRF-Token': 'mock-csrf-0' } })));
    const user = userEvent.setup();
    renderApp('/repositories');
    await screen.findByRole('navigation', { name: 'Primary' });
    await user.keyboard('{Control>}k{/Control}');
    const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
    const labels = within(dialog)
      .getAllByRole('option')
      .map((o) => o.textContent);
    expect(labels).toEqual(expect.arrayContaining(['Settings', 'My account', 'Access tokens', 'System']));
    for (const hidden of ['Authentication', 'Proxies', 'System status', 'System logs', 'Audit log']) {
      expect(labels).not.toContain(hidden);
    }
  });

  it('opens a page on Enter when its name matches', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories');
    await screen.findByRole('navigation', { name: 'Primary' });
    await user.keyboard('{Control>}k{/Control}');
    const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
    await user.type(within(dialog).getByRole('combobox'), 'blob');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(history.location.pathname).toBe('/blob-stores'));
  });

  it('never wraps from the empty state to Sign out', async () => {
    const user = userEvent.setup();
    renderApp('/repositories');
    await screen.findByRole('navigation', { name: 'Primary' });
    await user.keyboard('{Control>}k{/Control}');
    const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
    await user.keyboard('{ArrowUp}');
    expect(within(dialog).getAllByRole('option')[0]).toHaveAttribute('aria-selected', 'true');
    expect(within(dialog).getByRole('option', { name: 'Sign out' })).toHaveAttribute('aria-selected', 'false');
  });

  it('jumps to a repository, a package, an advisory and a user', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories');
    await screen.findByRole('navigation', { name: 'Primary' });
    const open = async (q: string) => {
      await user.keyboard('{Control>}k{/Control}');
      const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
      await user.type(within(dialog).getByRole('combobox'), q);
      return dialog;
    };

    let dialog = await open('npm-internal');
    await user.click(await within(dialog).findByRole('option', { name: /^npm-internal\s*npm · hosted$/ }));
    await waitFor(() => expect(history.location.pathname).toBe('/repositories/npm-internal/settings'));

    dialog = await open('ui-kit');
    await user.click(await within(dialog).findByRole('option', { name: /^@acme\/ui-kit@/ }));
    await waitFor(() => expect(history.location.pathname).toBe('/browse'));
    expect(history.location.search).toContain('name=ui-kit');

    dialog = await open('CVE-2020-8203');
    expect(within(dialog).queryByRole('option', { name: /^npm-/ })).not.toBeInTheDocument();
    await user.click(await within(dialog).findByRole('option', { name: /^GHSA-p6mc-m468-83gw · @acme\/ui-kit/ }));
    await waitFor(() => expect(history.location.pathname).toBe('/vulnerabilities'));
    expect(history.location.search).toContain('finding=');

    dialog = await open('j.karimov');
    await user.click(await within(dialog).findByRole('option', { name: /^j\.karimov\s*Jasur Karimov$/ }));
    await waitFor(() => expect(history.location.pathname).toBe('/access'));
    expect(history.location.search).toContain('user=');
  });
});
