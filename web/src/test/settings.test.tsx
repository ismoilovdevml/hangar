import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import type { Repository, RepositoryInput } from '../api/types';
import { mockState } from '../mocks/handlers';
import { session } from '../mocks/data';
import { mockSettings } from '../mocks/settings';
import { server } from '../mocks/node';
import { choose, valueOf } from './choose';
import { renderApp } from './render';

/** Records the JSON bodies sent to a method and path. */
function capture(method: string, path: string) {
  const bodies: unknown[] = [];
  server.events.on('request:start', async ({ request }) => {
    if (request.method === method && new URL(request.url).pathname === `/api/v1${path}`) {
      bodies.push(await request.clone().json());
    }
  });
  return bodies;
}

afterEach(() => server.events.removeAllListeners());

const oidcForm = () => screen.findByRole('form', { name: 'Single sign-on (OIDC)' });
const ldapForm = () => screen.findByRole('form', { name: 'LDAP directory' });

describe('settings navigation', () => {
  it('shows the Settings section to administrators', async () => {
    renderApp('/');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    expect(await within(nav).findByRole('link', { name: 'Authentication' })).toHaveAttribute('href', '/settings/authentication');
    expect(within(nav).getByRole('link', { name: 'Proxies' })).toHaveAttribute('href', '/settings/proxies');
  });

  it('hides it from other users and explains the page', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read'] })));
    renderApp('/settings/authentication');
    expect(await screen.findByRole('heading', { level: 1, name: 'Administrators only' })).toBeInTheDocument();
    expect(screen.getByText(/system:settings:read/)).toBeInTheDocument();
    const nav = screen.getByRole('navigation', { name: 'Primary' });
    expect(within(nav).queryByRole('link', { name: 'Authentication' })).not.toBeInTheDocument();
  });
});

describe('authentication settings', () => {
  it('shows the status, the redirect URI and the stored secret state', async () => {
    renderApp('/settings/authentication');
    const form = await oidcForm();
    expect(await screen.findByText('Single sign-on (OIDC)', { selector: 'strong' })).toBeInTheDocument();
    expect(within(form).getByText('https://hangar.acme.example/api/v1/auth/oidc/callback')).toBeInTheDocument();
    expect(within(form).getByRole('button', { name: 'Copy the redirect URI' })).toBeInTheDocument();
    const secret = within(form).getByLabelText('Client secret');
    expect(secret).toHaveValue('');
    expect(secret).toHaveAccessibleDescription('A secret is stored. Type a new one to replace it.');
    expect(within(form).getByRole('switch', { name: 'Allow sign-in with single sign-on' })).toBeChecked();
  });

  it('saves the form, with the mapping table and the role pickers, and updates the login page', async () => {
    const puts = capture('PUT', '/identity-providers/oidc');
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    const form = await oidcForm();
    const label = within(form).getByLabelText('Button label');
    await user.clear(label);
    await user.type(label, 'Acme SSO');
    await user.click(within(form).getByRole('button', { name: 'Add mapping' }));
    await user.type(within(form).getByLabelText('Group, row 3'), '/auditors');
    await choose(user, within(form).getByLabelText('Add a role for /auditors'), 'Reader');
    await user.click(within(form).getByRole('button', { name: 'Remove Reader from /auditors' }));
    await choose(user, within(form).getByLabelText('Add a role for /auditors'), 'CI deployer');
    await user.click(within(form).getByRole('button', { name: 'Save' }));

    expect(await within(form).findByText('Saved. Single sign-on is on for every node.')).toBeInTheDocument();
    expect(puts).toHaveLength(1);
    expect(puts[0]).toMatchObject({
      display_name: 'Acme SSO',
      client_secret: '',
      clear_client_secret: false,
      group_roles: [
        { group: '/platform-admins', roles: ['Admin'] },
        { group: '/developers', roles: ['Publisher'] },
        { group: '/auditors', roles: ['CI deployer'] },
      ],
      default_roles: ['Reader'],
    });
    const providers = await (await fetch('/api/v1/auth/providers')).json();
    expect(providers.oidc.display_name).toBe('Acme SSO');
  });

  it('checks fields before saving and shows server errors by field', async () => {
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    const form = await oidcForm();
    const issuer = within(form).getByLabelText('Issuer URL');
    await user.clear(issuer);
    await user.type(issuer, 'http://sso.example.com');
    await user.click(within(form).getByRole('button', { name: 'Add mapping' }));
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(issuer).toHaveAccessibleDescription('Use https (http is accepted for localhost only).');
    expect(within(form).getByLabelText('Group, row 3')).toHaveAccessibleDescription('Enter a group name.');

    server.use(
      http.put('*/api/v1/identity-providers/oidc', () =>
        HttpResponse.json(
          { code: 'invalid_argument', message: 'invalid settings', details: { fields: { 'group_roles[0].roles': 'role "Admin" does not exist' } } },
          { status: 400 },
        ),
      ),
    );
    await user.clear(issuer);
    await user.type(issuer, 'https://sso.example.com');
    await user.click(within(form).getByRole('button', { name: 'Remove mapping row 3' }));
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(await within(form).findByText('role "Admin" does not exist')).toBeInTheDocument();
    expect(within(form).getByRole('alert')).toHaveTextContent('Fix the highlighted fields and save again.');
  });

  it('removes a stored secret only when asked', async () => {
    const puts = capture('PUT', '/identity-providers/oidc');
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    const form = await oidcForm();
    await user.click(within(form).getAllByRole('button', { name: 'Remove' })[0]!);
    expect(within(form).getByText('Removed when you save.')).toBeInTheDocument();
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toMatchObject({ clear_client_secret: true, client_secret: '' });
    await waitFor(() => expect(mockSettings.oidc.client_secret_configured).toBe(false));
  });

  it('tests the form values', async () => {
    const tests = capture('POST', '/identity-providers/oidc:test');
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    const form = await oidcForm();
    await user.click(within(form).getByRole('button', { name: 'Test' }));
    const result = await within(form).findByRole('status');
    expect(result).toHaveTextContent('Test passed');
    expect(result).toHaveTextContent('Discovery ok, credentials are verified at first sign-in');
    expect(result).toHaveTextContent('Client credentials');
    expect(tests[0]).toMatchObject({ settings: { issuer_url: 'https://sso.acme.example/realms/acme' } });
  });

  it('looks an LDAP user up in a test', async () => {
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    const form = await ldapForm();
    await user.type(within(form).getByLabelText('Username to look up in the test'), 'jdoe');
    await user.click(within(form).getByRole('button', { name: 'Test' }));
    const result = await within(form).findByRole('status');
    expect(result).toHaveTextContent('Test passed');
    expect(result).toHaveTextContent('CN=jdoe,OU=People,DC=acme,DC=example');
    expect(result).toHaveTextContent('Roles granted by the mapping');
  });

  it('shows providers from the configuration file read-only', async () => {
    mockSettings.oidcFromFile();
    renderApp('/settings/authentication');
    const form = await oidcForm();
    expect(within(form).getByText(/is set in the configuration file/)).toBeInTheDocument();
    expect(within(form).getByLabelText(/Issuer URL/)).toBeDisabled();
    expect(within(form).getAllByText('set in configuration file').length).toBeGreaterThan(5);
    expect(within(form).queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    expect(within(form).queryByRole('button', { name: 'Add mapping' })).not.toBeInTheDocument();
    // The LDAP card stays editable.
    const ldap = await ldapForm();
    expect(within(ldap).getByRole('button', { name: 'Save' })).toBeInTheDocument();
  });

  it('turns LDAP sign-in off for the login page', async () => {
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    const form = await ldapForm();
    await user.click(within(form).getByRole('switch', { name: 'Allow sign-in with directory accounts' }));
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(await within(form).findByText('Saved. LDAP sign-in is off.')).toBeInTheDocument();
    const providers = await (await fetch('/api/v1/auth/providers')).json();
    expect(providers.ldap.enabled).toBe(false);
  });
});

describe('proxies', () => {
  /** The proxies table once loaded. */
  const table = async () => {
    await screen.findByRole('row', { name: /corp-proxy/ });
    return screen.getByRole('table', { name: 'Outbound proxies' });
  };

  it('lists proxies with the default and usage', async () => {
    renderApp('/settings/proxies');
    const rows = within(await table()).getAllByRole('row');
    expect(rows).toHaveLength(3);
    expect(rows[1]).toHaveTextContent('corp-proxy');
    expect(rows[1]).toHaveTextContent('Platform default');
    expect(rows[1]).toHaveTextContent('3128');
    expect(rows[2]).toHaveTextContent('dmz-proxy');
  });

  it('validates and creates a proxy', async () => {
    const posts = capture('POST', '/proxies');
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await table();
    await user.click(screen.getByRole('button', { name: 'New proxy' }));
    const editor = screen.getByRole('form', { name: 'New proxy' });
    await user.clear(within(editor).getByLabelText('Port'));
    await user.click(within(editor).getByRole('button', { name: 'Create proxy' }));
    expect(within(editor).getByLabelText('Name')).toHaveAccessibleDescription('Enter a name.');
    expect(within(editor).getByLabelText('Host')).toHaveAccessibleDescription('Enter the proxy host name or IP address.');
    expect(within(editor).getByLabelText('Port')).toHaveAccessibleDescription('Enter a port between 1 and 65535.');

    await user.type(within(editor).getByLabelText('Name'), 'edge');
    await choose(user, within(editor).getByLabelText('Scheme'), 'https');
    await user.type(within(editor).getByLabelText('Host'), 'edge-proxy.example.com');
    await user.type(within(editor).getByLabelText('Port'), '8443');
    await user.type(within(editor).getByLabelText(/No proxy for/), '.corp.example{Enter}');
    await user.type(within(editor).getByLabelText(/Redirect target hosts/), '.cdn.example{Enter}');
    await user.click(within(editor).getByRole('button', { name: 'Create proxy' }));

    expect(await screen.findByText('Proxy edge created and in use on every node.')).toBeInTheDocument();
    expect(posts[0]).toEqual({
      name: 'edge', scheme: 'https', host: 'edge-proxy.example.com', port: 8443, username: '', password: '', clear_password: false,
      no_proxy: ['.corp.example'], redirect_hosts: ['.cdn.example'], ca_pem: '', default: false,
    });
    expect(within(await table()).getByRole('row', { name: /edge-proxy\.example\.com/ })).toBeInTheDocument();
  });

  it('tests a proxy through the API and shows status and latency', async () => {
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await table();
    await user.click(screen.getByRole('button', { name: 'Test corp-proxy' }));
    const panel = await screen.findByRole('region', { name: 'Test of corp-proxy' });
    expect(within(panel).getByRole('status')).toHaveTextContent('HTTP 200 in 184 ms');
  });

  it('refuses to delete a proxy repositories use', async () => {
    const nuget = mockState.repositories.find((r) => r.name === 'nuget-proxy') as Repository;
    nuget.config.proxy!.http = { proxy_name: 'dmz-proxy' };
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await table();
    await user.click(screen.getByRole('button', { name: 'Delete dmz-proxy' }));
    const dialog = screen.getByRole('alertdialog');
    expect(dialog).toHaveTextContent('1 repositories select this proxy');
    await user.click(within(dialog).getByRole('button', { name: 'Delete proxy' }));
    expect(await within(dialog).findByText(/is used by 1 repositories \(nuget-proxy\)/)).toBeInTheDocument();
    expect(mockSettings.proxies.some((p) => p.name === 'dmz-proxy')).toBe(true);
  });

  it('shows the proxy of the configuration file read-only', async () => {
    mockSettings.proxyFromFile();
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await screen.findByRole('row', { name: /^config/ });
    const rows = within(await table()).getAllByRole('row');
    expect(rows[1]).toHaveTextContent('config');
    expect(rows[1]).toHaveTextContent('set in configuration file');
    expect(within(rows[1]!).queryByRole('button', { name: 'Delete config' })).not.toBeInTheDocument();
    await user.click(within(rows[1]!).getByRole('button', { name: 'View config' }));
    const editor = screen.getByRole('form', { name: 'Proxy config' });
    expect(within(editor).getByLabelText(/^Host/)).toBeDisabled();
    expect(within(editor).queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
  });
});

describe('repository proxy picker', () => {
  it('selects a named proxy for a new proxy repository', async () => {
    const posts = capture('POST', '/repositories');
    const user = userEvent.setup();
    renderApp('/repositories/new?format=npm&type=proxy');
    await user.clear(await screen.findByLabelText('Name'));
    await user.type(screen.getByLabelText('Name'), 'npm-dmz');
    await user.click(screen.getByText('Upstream credentials and connection'));
    const picker = screen.getByLabelText('Outbound proxy');
    expect(valueOf(picker)).toBe('inherit');
    await user.click(picker);
    const list = screen.getByRole('listbox');
    expect(within(list).getByRole('option', { name: /Platform default/ })).toHaveTextContent('Currently corp-proxy (proxy.acme.example:3128)');
    expect(within(list).getByRole('option', { name: /No proxy \(direct\)/ })).toBeInTheDocument();
    await user.click(within(list).getByRole('option', { name: /dmz-proxy/ }));
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect((posts[0] as RepositoryInput).config.proxy?.http).toEqual({ proxy_name: 'dmz-proxy' });
  });
});
