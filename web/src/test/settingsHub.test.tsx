import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

/** Signs the mock session in as a user with only `privileges`. */
function signInAs(privileges: string[]) {
  server.use(
    http.get('*/api/v1/auth/session', () =>
      HttpResponse.json({ ...session, privileges, user: { ...session.user, roles: ['Reader'] } }, { headers: { 'X-CSRF-Token': 'mock-csrf-0' } }),
    ),
  );
}

const row = (name: string) => screen.getByRole('link', { name: new RegExp(`^${name}`) });

describe('settings hub', () => {
  it('lists every setting with how it is set now and where it is changed', async () => {
    renderApp('/settings');
    expect(await screen.findByRole('heading', { level: 1, name: 'Settings' })).toBeInTheDocument();
    expect(screen.getByText('Everything about this Hangar in one place.')).toBeInTheDocument();

    const install = screen.getByRole('region', { name: 'This installation' });
    const addresses = within(install).getByRole('link', { name: /^Addresses/ });
    await waitFor(() => expect(addresses).toHaveTextContent('Addresseshttps://pkg.acme.io'));
    expect(addresses).toHaveAttribute('href', '/settings/system#endpoints-title');
    expect(within(install).getByRole('link', { name: /^Instance name/ })).toHaveAttribute('href', '/settings/system');

    await waitFor(() => expect(row('Sign-in')).toHaveTextContent('company SSO · SAML · LDAP · local accounts'));
    expect(row('Sign-in')).toHaveAttribute('href', '/settings/authentication');
    await waitFor(() => expect(row('Internet access')).toHaveTextContent('corp-proxy · proxy.acme.example:3128 · 1 more proxy'));
    expect(row('Internet access')).toHaveAttribute('href', '/settings/proxies');
    await waitFor(() => expect(row('Storage')).toHaveTextContent(/^Storage3 blob stores · .+ used · 2 not healthy$/));
    expect(row('Storage')).toHaveAttribute('href', '/blob-stores');

    await waitFor(() => expect(row('Nodes')).toHaveTextContent('2 of 3 up · hangar-1 leader'));
    expect(row('Nodes')).toHaveAttribute('href', '/cluster');
    expect(row('Configuration')).toHaveAttribute('href', '/settings/system#config-title');
    await waitFor(() => expect(row('System status')).toHaveTextContent(/\d+ of \d+ passing/));
    expect(row('System status')).toHaveAttribute('href', '/system/status');
    expect(row('System logs')).toHaveAttribute('href', '/system/logs');
    expect(row('Audit log')).toHaveAttribute('href', '/audit');

    const mine = screen.getByRole('region', { name: 'Your account' });
    expect(within(mine).getByRole('link', { name: /^My account/ })).toHaveTextContent('Dilshod Ismoilov · d.ismoilov@acme.example');
    expect(within(mine).getByRole('link', { name: /^My account/ })).toHaveAttribute('href', '/account');
    expect(within(mine).getByRole('link', { name: /^Access tokens/ })).toHaveTextContent('1 token expiring soon');
    expect(within(mine).getByRole('link', { name: /^Access tokens/ })).toHaveAttribute('href', '/account#my-tokens-title');
    // This build has the API reference page, so the hub links to it.
    expect(screen.getByRole('link', { name: /^API reference/ })).toHaveAttribute('href', '/api-docs');

    const status = await screen.findByRole('link', { name: 'All systems normal' });
    expect(status).toHaveAttribute('href', '/system/status');
    expect(status.closest('p')).toHaveTextContent(/^Hangar v1\.0\.0 · 237ab95/);
  });

  it('shows a plain user only their own account', async () => {
    signInAs(['repo:*:read']);
    renderApp('/settings');
    expect(await screen.findByRole('heading', { level: 1, name: 'Settings' })).toBeInTheDocument();
    expect(screen.getAllByRole('region').map((r) => r.getAttribute('aria-label'))).toEqual(['Your account']);
    expect(row('My account')).toBeInTheDocument();
    expect(row('Access tokens')).toBeInTheDocument();
    for (const name of ['Addresses', 'Sign-in', 'Internet access', 'Storage', 'Nodes', 'Configuration', 'System status', 'System logs', 'Audit log']) {
      expect(screen.queryByRole('link', { name: new RegExp(`^${name}`) })).not.toBeInTheDocument();
    }
  });

  it('shows each operations page only to the privilege that opens it', async () => {
    signInAs(['repo:*:read', 'system:audit:read', 'system:cluster:read']);
    renderApp('/settings');
    expect(await screen.findByRole('link', { name: /^Audit log/ })).toBeInTheDocument();
    expect(row('Nodes')).toBeInTheDocument();
    expect(row('System status')).toBeInTheDocument();
    // Logs need cluster:admin; sign-in and configuration need settings:read.
    expect(screen.queryByRole('link', { name: /^System logs/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /^Sign-in/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /^Configuration/ })).not.toBeInTheDocument();
  });

  it('says so when a status line cannot be read', async () => {
    server.use(http.get('*/api/v1/proxies', () => HttpResponse.json({ code: 'internal', message: 'boom' }, { status: 500 })));
    renderApp('/settings');
    await waitFor(() => expect(row('Internet access')).toHaveTextContent('Could not be read'));
    expect(row('Internet access')).toHaveAttribute('href', '/settings/proxies');
  });

  it('opens a setting from the keyboard', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/settings');
    const signIn = await screen.findByRole('link', { name: /^Sign-in/ });
    signIn.focus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(history.location.pathname).toBe('/settings/authentication'));
  });
});

describe('pages under Settings', () => {
  it.each([
    ['/account', 'My account'],
    ['/settings/system', 'System'],
    ['/settings/proxies', 'Outbound proxy'],
    ['/system/status', null],
    ['/system/logs', 'System logs'],
    ['/audit', 'Audit log'],
  ])('keeps %s working, with "Settings" in the breadcrumb', async (path, heading) => {
    const { history } = renderApp(path);
    const crumbs = await screen.findByRole('navigation', { name: 'Breadcrumb' });
    expect(await within(crumbs).findByRole('link', { name: 'Settings' })).toHaveAttribute('href', '/settings');
    if (heading) expect(await screen.findByRole('heading', { level: 1, name: heading })).toBeInTheDocument();
    expect(history.location.pathname).toBe(path);
  });

  it('leaves the breadcrumb of sidebar pages alone', async () => {
    renderApp('/blob-stores');
    const crumbs = await screen.findByRole('navigation', { name: 'Breadcrumb' });
    expect(await within(crumbs).findByText('Blob stores')).toBeInTheDocument();
    expect(within(crumbs).queryByRole('link', { name: 'Settings' })).not.toBeInTheDocument();
  });

  it('opens Settings from the account menu', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories');
    await user.click(await screen.findByRole('button', { name: /^Account/ }));
    await user.click(within(screen.getByRole('menu')).getByRole('menuitem', { name: 'Settings' }));
    await waitFor(() => expect(history.location.pathname).toBe('/settings'));
    expect(await screen.findByRole('heading', { level: 1, name: 'Settings' })).toBeInTheDocument();
  });
});
