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

const presetForm = (name: string) => screen.findByRole('form', { name });
const card = (name: string) => screen.getByRole('button', { name: new RegExp(`^${name}`) });
type User = ReturnType<typeof userEvent.setup>;
/** Opens the fields a preset fills in. */
const customize = (user: User, form: HTMLElement) => user.click(within(form).getByRole('button', { name: 'Customize' }));
const openRoles = (user: User, form: HTMLElement) => user.click(within(form).getByRole('button', { name: /^Groups and roles/ }));

describe('settings navigation', () => {
  it('reaches sign-in and proxies from both the Settings page and the sidebar', async () => {
    renderApp('/settings');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    expect(await screen.findByRole('link', { name: /^Sign-in/ })).toHaveAttribute('href', '/settings/authentication');
    expect(screen.getByRole('link', { name: /^Internet access/ })).toHaveAttribute('href', '/settings/proxies');
    expect(within(nav).getByRole('link', { name: 'Authentication' })).toHaveAttribute('href', '/settings/authentication');
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
  it('opens on the provider in use, with its status, redirect URI and stored secret state', async () => {
    renderApp('/settings/authentication');
    const form = await presetForm('Keycloak');
    const chips = screen.getByRole('list', { name: 'Sign-in methods' });
    expect(await within(chips).findByText('company SSO')).toBeInTheDocument();
    expect(within(chips).getByText('Active Directory')).toBeInTheDocument();
    expect(within(chips).getByText('Local accounts')).toBeInTheDocument();
    expect(card('Keycloak')).toHaveAttribute('aria-pressed', 'true');
    expect(card('Keycloak')).toHaveTextContent('in use');
    expect(card('Okta')).toHaveAttribute('aria-pressed', 'false');
    expect(within(form).getByText('https://hangar.acme.example/api/v1/auth/oidc/callback')).toBeInTheDocument();
    expect(within(form).getByRole('button', { name: 'Copy the redirect URI' })).toBeInTheDocument();
    const secret = within(form).getByLabelText('Client secret');
    expect(secret).toHaveValue('');
    expect(secret).toHaveAccessibleDescription('A secret is stored. Type a new one to replace it.');
    expect(within(form).getByRole('switch', { name: 'Allow sign-in with Keycloak' })).toBeChecked();
    expect(within(form).getByRole('list', { name: 'Preset values' })).toHaveTextContent('openid profile email');
    expect(within(form).getByText(/New users get/)).toHaveTextContent('New users get Reader');
    expect(within(form).getByRole('button', { name: 'Save' })).toBeInTheDocument();
  });

  it('prefills a new provider from its preset', async () => {
    mockSettings.nothingSaved();
    const puts = capture('PUT', '/identity-providers/oidc');
    const user = userEvent.setup();
    renderApp('/settings/authentication');

    // Nothing saved yet: the first card, filled for Active Directory.
    let form = await presetForm('Active Directory');
    expect(within(form).getByText('Only 3 things to fill in. The rest is set for Active Directory.')).toBeInTheDocument();
    expect(within(form).getByLabelText('Server')).toHaveAttribute('placeholder', 'ldaps://dc1.example.com');
    await user.type(within(form).getByLabelText('Service account'), 'hangar-svc@example.com');
    const used = within(form).getByRole('list', { name: 'Preset values' });
    expect(used).toHaveTextContent('sAMAccountName');
    expect(used).toHaveTextContent('nested groups on');
    expect(used).toHaveTextContent('DC=example,DC=com');
    await customize(user, form);
    expect(within(form).getByLabelText('User base DN')).toHaveValue('DC=example,DC=com');
    expect(within(form).getByLabelText('Group base DN')).toHaveValue('DC=example,DC=com');
    expect(within(form).getByLabelText('Group filter')).toHaveValue('(member:1.2.840.113556.1.4.1941:={dn})');
    expect(within(form).getByRole('checkbox', { name: /Resolve nested groups/ })).toBeChecked();

    // FreeIPA keeps the account and searches its own user tree.
    await user.click(card('FreeIPA'));
    form = await presetForm('FreeIPA');
    expect(within(form).getByLabelText('Service account')).toHaveValue('hangar-svc@example.com');
    expect(within(form).getByRole('list', { name: 'Preset values' })).toHaveTextContent('cn=users,cn=accounts,dc=example,dc=com');

    await user.click(card('Keycloak'));
    form = await presetForm('Keycloak');
    expect(within(form).getByLabelText('Issuer URL')).toHaveAttribute('placeholder', 'https://<host>/realms/<realm>');
    expect(within(form).getByRole('button', { name: 'Add Keycloak' })).toBeInTheDocument();

    await user.click(card('Google Workspace'));
    form = await presetForm('Google Workspace');
    expect(within(form).getByLabelText('Issuer URL')).toHaveValue('https://accounts.google.com');

    await user.click(card('Microsoft Entra ID'));
    form = await presetForm('Microsoft Entra ID');
    const issuer = within(form).getByLabelText('Issuer URL');
    expect(issuer).toHaveValue('');
    await user.type(issuer, 'https://login.microsoftonline.com/<tenant-id>/v2.0');
    await user.click(within(form).getByRole('button', { name: 'Add Microsoft Entra ID' }));
    expect(issuer).toHaveAccessibleDescription('Replace the <…> part with your own value.');
    expect(puts).toHaveLength(0);

    await user.click(card('Okta'));
    form = await presetForm('Okta');
    await user.type(within(form).getByLabelText('Issuer URL'), 'https://acme.okta.com');
    await user.type(within(form).getByLabelText('Client ID'), 'hangar');
    await user.type(within(form).getByLabelText('Client secret'), 's3cret');
    await user.click(within(form).getByRole('button', { name: 'Add Okta' }));
    expect(await within(form).findByText('Saved. People can sign in with Okta on every node.')).toBeInTheDocument();
    expect(puts[0]).toMatchObject({
      enabled: true,
      preset: 'okta',
      issuer_url: 'https://acme.okta.com',
      client_secret: 's3cret',
      display_name: 'Okta',
      scopes: ['openid', 'profile', 'email', 'groups'],
      groups_claim: 'groups',
    });
    // Saved: the card is in use and the form now edits it.
    expect(within(form).getByRole('button', { name: 'Save' })).toBeInTheDocument();
    expect(card('Okta')).toHaveTextContent('in use');
  });

  it('keeps typed secrets when switching presets', async () => {
    mockSettings.nothingSaved();
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    await presetForm('Active Directory');
    await user.click(card('Keycloak'));
    let form = await presetForm('Keycloak');
    await user.type(within(form).getByLabelText('Client ID'), 'hangar');
    await user.type(within(form).getByLabelText('Client secret'), 'oidc-secret');

    await user.click(card('Okta'));
    form = await presetForm('Okta');
    expect(within(form).getByLabelText('Client ID')).toHaveValue('hangar');
    expect(within(form).getByLabelText('Client secret')).toHaveValue('oidc-secret');

    await user.click(card('OpenLDAP'));
    form = await presetForm('OpenLDAP');
    await user.type(within(form).getByLabelText('Password'), 'bind-secret');

    await user.click(card('Keycloak'));
    form = await presetForm('Keycloak');
    expect(within(form).getByLabelText('Client secret')).toHaveValue('oidc-secret');
    await user.click(card('Active Directory'));
    form = await presetForm('Active Directory');
    expect(within(form).getByLabelText('Password')).toHaveValue('bind-secret');
  });

  it('asks for the new secret when another provider replaces the one in use', async () => {
    const puts = capture('PUT', '/identity-providers/oidc');
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    await presetForm('Keycloak');
    await user.click(card('Okta'));
    const form = await presetForm('Okta');
    expect(within(form).getByText(/Adding Okta replaces Keycloak \(company SSO\)/)).toBeInTheDocument();
    expect(within(form).getByLabelText('Client ID')).toHaveValue('hangar');
    expect(within(form).getByLabelText('Issuer URL')).toHaveValue('');
    await user.type(within(form).getByLabelText('Issuer URL'), 'https://acme.okta.com');
    await user.click(within(form).getByRole('button', { name: 'Switch to Okta' }));
    expect(within(form).getByLabelText('Client secret')).toHaveAccessibleDescription('Enter the client secret Okta issued, or remove the stored one.');
    expect(puts).toHaveLength(0);

    // Back on Keycloak, the stored values return.
    await user.click(card('Keycloak'));
    const kc = await presetForm('Keycloak');
    expect(within(kc).getByLabelText('Issuer URL')).toHaveValue('https://sso.acme.example/realms/acme');
  });

  it('saves the form, with the mapping table and the role pickers, and updates the login page', async () => {
    const puts = capture('PUT', '/identity-providers/oidc');
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    const form = await presetForm('Keycloak');
    await customize(user, form);
    const label = within(form).getByLabelText('Button label');
    await user.clear(label);
    await user.type(label, 'Acme SSO');
    await openRoles(user, form);
    await user.click(within(form).getByRole('button', { name: 'Add mapping' }));
    await user.type(within(form).getByLabelText('Group, row 3'), '/auditors');
    await choose(user, within(form).getByLabelText('Add a role for /auditors'), 'Reader');
    await user.click(within(form).getByRole('button', { name: 'Remove Reader from /auditors' }));
    await choose(user, within(form).getByLabelText('Add a role for /auditors'), 'CI deployer');
    await user.click(within(form).getByRole('button', { name: 'Save' }));

    expect(await within(form).findByText('Saved. People can sign in with Acme SSO on every node.')).toBeInTheDocument();
    expect(puts).toHaveLength(1);
    expect(puts[0]).toMatchObject({
      preset: 'keycloak',
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
    const form = await presetForm('Keycloak');
    const issuer = within(form).getByLabelText('Issuer URL');
    await user.clear(issuer);
    await user.type(issuer, 'http://sso.example.com');
    await openRoles(user, form);
    await user.click(within(form).getByRole('button', { name: 'Add mapping' }));
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(issuer).toHaveAccessibleDescription('Use https (http is accepted for localhost only).');
    expect(within(form).getByLabelText('Group, row 3')).toHaveAccessibleDescription('Enter a group name.');

    server.use(
      http.put('*/api/v1/identity-providers/oidc', () =>
        HttpResponse.json(
          { code: 'invalid_argument', message: 'invalid settings', details: { fields: { 'group_roles[0].roles': 'role "Admin" does not exist', groups_claim: 'is not valid' } } },
          { status: 400 },
        ),
      ),
    );
    await user.clear(issuer);
    await user.type(issuer, 'https://sso.example.com');
    await user.click(within(form).getByRole('button', { name: 'Remove mapping row 3' }));
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(await within(form).findByText('role "Admin" does not exist')).toBeInTheDocument();
    // An error in a preset field opens the details.
    expect(within(form).getByLabelText('Groups')).toHaveAccessibleDescription('is not valid');
    expect(within(form).getByRole('alert')).toHaveTextContent('Fix the highlighted fields and save again.');
  });

  it('removes a stored secret only when asked', async () => {
    const puts = capture('PUT', '/identity-providers/oidc');
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    const form = await presetForm('Keycloak');
    await user.click(within(form).getByRole('button', { name: 'Remove' }));
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
    const form = await presetForm('Keycloak');
    await user.click(within(form).getByRole('button', { name: 'Test' }));
    const result = await within(form).findByRole('status');
    expect(result).toHaveTextContent('Test passed');
    expect(result).toHaveTextContent('Discovery ok, credentials are verified at first sign-in');
    expect(result).toHaveTextContent('Client credentials');
    expect(tests[0]).toMatchObject({ settings: { issuer_url: 'https://sso.acme.example/realms/acme', preset: 'keycloak' } });
  });

  it('looks an LDAP user up in a test', async () => {
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    await presetForm('Keycloak');
    await user.click(card('Active Directory'));
    const form = await presetForm('Active Directory');
    await user.type(within(form).getByLabelText('Username to look up in the test'), 'jdoe');
    await user.click(within(form).getByRole('button', { name: 'Test' }));
    const result = await within(form).findByRole('status');
    expect(result).toHaveTextContent('Test passed');
    expect(result).toHaveTextContent('CN=jdoe,OU=People,DC=acme,DC=example');
    expect(result).toHaveTextContent('Roles granted by the mapping');
  });

  it('shows providers from the configuration file read-only', async () => {
    const tests = capture('POST', '/identity-providers/oidc:test');
    mockSettings.oidcFromFile();
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    // The file stores no preset: the issuer tells it is Keycloak.
    const form = await presetForm('Keycloak');
    expect(within(form).getByText(/is set in the configuration file/)).toBeInTheDocument();
    expect(within(form).getByLabelText(/Issuer URL/)).toBeDisabled();
    expect(within(form).getAllByText('set in configuration file').length).toBeGreaterThan(2);
    expect(within(form).queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    await openRoles(user, form);
    expect(within(form).queryByRole('button', { name: 'Add mapping' })).not.toBeInTheDocument();
    // Test runs against the file's settings.
    await user.click(within(form).getByRole('button', { name: 'Test' }));
    expect(await within(form).findByRole('status')).toHaveTextContent('Test passed');
    expect(tests[0]).toEqual({});

    // Another OIDC preset cannot replace it here.
    await user.click(card('Okta'));
    const okta = await screen.findByRole('region', { name: 'Okta' });
    expect(within(okta).getByText(/the configuration file sets it/)).toBeInTheDocument();
    expect(within(okta).queryByLabelText(/Issuer URL/)).not.toBeInTheDocument();
    expect(within(okta).getByText('https://hangar.acme.example/api/v1/auth/oidc/callback')).toBeInTheDocument();

    // The directory stays editable.
    await user.click(card('Active Directory'));
    const ldap = await presetForm('Active Directory');
    expect(within(ldap).getByRole('button', { name: 'Save' })).toBeInTheDocument();
  });

  it('turns LDAP sign-in off for the login page', async () => {
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    await presetForm('Keycloak');
    await user.click(card('Active Directory'));
    const form = await presetForm('Active Directory');
    await user.click(within(form).getByRole('switch', { name: 'Allow sign-in with Active Directory' }));
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(await within(form).findByText('Saved. LDAP sign-in is off.')).toBeInTheDocument();
    const providers = await (await fetch('/api/v1/auth/providers')).json();
    expect(providers.ldap.enabled).toBe(false);
  });

  it('offers OpenID Connect or LDAP under Other', async () => {
    const user = userEvent.setup();
    renderApp('/settings/authentication');
    await presetForm('Keycloak');
    await user.click(card('Other'));
    let form = await presetForm('Other');
    expect(within(form).getByRole('radio', { name: 'OpenID Connect' })).toBeChecked();
    expect(within(form).getByLabelText('Issuer URL')).toBeInTheDocument();
    // Every field is open for a provider without a preset.
    expect(within(form).getByLabelText('Button label')).toBeInTheDocument();
    await user.click(within(form).getByRole('radio', { name: 'LDAP directory' }));
    form = await presetForm('Other');
    expect(within(form).getByLabelText('Server')).toBeInTheDocument();
    expect(within(form).getByLabelText('User filter')).toBeInTheDocument();
  });

  it('shows the settings read-only without system:settings:admin', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read', 'system:settings:read'] })));
    renderApp('/settings/authentication');
    const form = await presetForm('Keycloak');
    expect(within(form).getByLabelText('Issuer URL')).toBeDisabled();
    expect(within(form).getByText(/Read-only: changing sign-in needs system:settings:admin/)).toBeInTheDocument();
    expect(within(form).queryByRole('button', { name: 'Test' })).not.toBeInTheDocument();
    expect(within(form).queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
  });
});

describe('proxies', () => {
  /** The proxy list once loaded. */
  const proxyList = async () => {
    await screen.findByRole('listitem', { name: 'corp-proxy' });
    return screen.getByRole('list', { name: 'Outbound proxies' });
  };

  it('says how remotes are reached when there is no proxy', async () => {
    mockSettings.noProxies();
    renderApp('/settings/proxies');
    expect(await screen.findByRole('heading', { name: 'Connecting directly' })).toBeInTheDocument();
    expect(screen.getByText(/Add one only if your network blocks direct internet access/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Add a proxy' })).toBeInTheDocument();
    expect(screen.queryByRole('list', { name: 'Outbound proxies' })).not.toBeInTheDocument();
  });

  it('names the proxy from the server environment when there is one', async () => {
    mockSettings.noProxies('http://proxy.example.com:3128 (HTTPS_PROXY)');
    renderApp('/settings/proxies');
    expect(await screen.findByRole('heading', { name: 'Using the server’s proxy' })).toBeInTheDocument();
    expect(screen.getByText('http://proxy.example.com:3128 (HTTPS_PROXY)')).toBeInTheDocument();
  });

  it('adds the first proxy with the short form', async () => {
    mockSettings.noProxies();
    const posts = capture('POST', '/proxies');
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await user.click(await screen.findByRole('button', { name: 'Add a proxy' }));
    const form = screen.getByRole('form', { name: 'New proxy' });
    const address = within(form).getByLabelText('Proxy address');
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(address).toHaveAccessibleDescription('Enter the proxy address, e.g. http://proxy.example.com:3128.');
    await user.type(address, 'proxy.example.com');
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(address).toHaveAccessibleDescription('Add the port, e.g. :3128.');
    await user.type(address, ':3128');
    expect(within(form).getByText('proxy.example.com', { selector: 'code' })).toBeInTheDocument();
    await user.click(within(form).getByRole('checkbox', { name: 'Use for every proxy repository' }));
    await user.click(within(form).getByRole('button', { name: 'Test' }));
    expect(await within(form).findByText(/api\.nuget\.org answered 200 in 184 ms/)).toBeInTheDocument();
    await user.click(within(form).getByRole('button', { name: 'Save' }));

    expect(await screen.findByText('Proxy proxy.example.com saved and in use on every node.')).toBeInTheDocument();
    expect(posts[0]).toEqual({
      name: 'proxy.example.com', scheme: 'http', host: 'proxy.example.com', port: 3128, username: '', password: '', clear_password: false,
      no_proxy: [], redirect_hosts: [], ca_pem: '', default: true,
    });
    const row = await screen.findByRole('listitem', { name: 'proxy.example.com' });
    expect(row).toHaveTextContent('Platform default');
  });

  it('keeps credentials, exceptions and certificates behind the disclosures', async () => {
    const posts = capture('POST', '/proxies');
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await proxyList();
    await user.click(screen.getByRole('button', { name: 'Add another proxy' }));
    const form = screen.getByRole('form', { name: 'New proxy' });
    expect(within(form).queryByLabelText('Username')).not.toBeInTheDocument();
    expect(within(form).queryByLabelText(/Skip the proxy for/)).not.toBeInTheDocument();

    // Credentials typed into the address move to their own fields.
    const address = within(form).getByLabelText('Proxy address');
    await user.type(address, 'https://svc:pa55@edge-proxy.example.com:8443');
    await user.tab();
    expect(address).toHaveValue('https://edge-proxy.example.com:8443');
    expect(within(form).getByRole('button', { name: 'Needs a username and password' })).toHaveAttribute('aria-expanded', 'true');
    expect(within(form).getByLabelText('Username')).toHaveValue('svc');

    await user.click(within(form).getByRole('button', { name: 'Choose a name' }));
    const name = within(form).getByLabelText('Name');
    expect(name).toHaveValue('edge-proxy.example.com');
    await user.clear(name);
    await user.type(name, 'Edge');
    await user.click(within(form).getByRole('button', { name: 'Exceptions and certificates' }));
    await user.type(within(form).getByLabelText(/Skip the proxy for/), '.corp.example{Enter}');
    await user.type(within(form).getByLabelText(/Redirect target hosts/), '.cdn.example{Enter}');
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(name).toHaveAccessibleDescription("Use lower-case letters, digits, '.', '_' or '-', starting with a letter or digit.");
    await user.clear(name);
    await user.type(name, 'edge');
    await user.click(within(form).getByRole('button', { name: 'Save' }));

    expect(await screen.findByText('Proxy edge saved and in use on every node.')).toBeInTheDocument();
    expect(posts[0]).toEqual({
      name: 'edge', scheme: 'https', host: 'edge-proxy.example.com', port: 8443, username: 'svc', password: 'pa55', clear_password: false,
      no_proxy: ['.corp.example'], redirect_hosts: ['.cdn.example'], ca_pem: '', default: false,
    });
  });

  it('lists proxies with the default and usage', async () => {
    renderApp('/settings/proxies');
    const items = within(await proxyList()).getAllByRole('listitem');
    expect(items).toHaveLength(2);
    expect(items[0]).toHaveTextContent('corp-proxy');
    expect(items[0]).toHaveTextContent('Platform default');
    expect(items[0]).toHaveTextContent('http://proxy.acme.example:3128');
    expect(items[0]).toHaveTextContent(/\d+ inherit it/);
    expect(items[1]).toHaveTextContent('dmz-proxy');
  });

  it('edits a proxy and keeps its stored password', async () => {
    const puts = capture('PUT', '/proxies/corp-proxy');
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await proxyList();
    await user.click(screen.getByRole('button', { name: 'Edit corp-proxy' }));
    const form = screen.getByRole('form', { name: 'Proxy corp-proxy' });
    expect(within(form).getByLabelText('Proxy address')).toHaveValue('http://proxy.acme.example:3128');
    expect(within(form).getByLabelText('Password')).toHaveAccessibleDescription('A password is stored. Type a new one to replace it.');
    expect(within(form).getByRole('button', { name: 'Exceptions and certificates' })).toHaveAttribute('aria-expanded', 'true');
    await user.clear(within(form).getByLabelText('Proxy address'));
    await user.type(within(form).getByLabelText('Proxy address'), 'http://proxy2.acme.example:8080');
    await user.click(within(form).getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Proxy corp-proxy saved and in use on every node.')).toBeInTheDocument();
    expect(puts[0]).toMatchObject({ host: 'proxy2.acme.example', port: 8080, password: '', clear_password: false, default: true });
  });

  it('tests a proxy through the API and shows status and latency', async () => {
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await proxyList();
    await user.click(screen.getByRole('button', { name: 'Test corp-proxy' }));
    const panel = await screen.findByRole('region', { name: 'Test of corp-proxy' });
    expect(within(panel).getByRole('status')).toHaveTextContent('HTTP 200 in 184 ms');
  });

  it('refuses to delete a proxy repositories use', async () => {
    const nuget = mockState.repositories.find((r) => r.name === 'nuget-proxy') as Repository;
    nuget.config.proxy!.http = { proxy_name: 'dmz-proxy' };
    const user = userEvent.setup();
    renderApp('/settings/proxies');
    await proxyList();
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
    const row = await screen.findByRole('listitem', { name: 'config' });
    expect(row).toHaveTextContent('set in configuration file');
    expect(within(row).queryByRole('button', { name: 'Delete config' })).not.toBeInTheDocument();
    await user.click(within(row).getByRole('button', { name: 'View config' }));
    const form = screen.getByRole('form', { name: 'Proxy config' });
    expect(within(form).getByLabelText(/^Proxy address/)).toBeDisabled();
    expect(within(form).queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    await user.click(within(form).getByRole('button', { name: 'Close' }));

    // Another proxy cannot become the default while the file sets one.
    await user.click(screen.getByRole('button', { name: 'Add another proxy' }));
    const add = screen.getByRole('form', { name: 'New proxy' });
    const def = within(add).getByRole('checkbox', { name: 'Use for every proxy repository' });
    expect(def).toBeDisabled();
    expect(def).toHaveAccessibleDescription('The proxy of the configuration file is the default.');
  });

  it('shows proxies read-only without system:settings:admin', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read', 'system:settings:read'] })));
    mockSettings.noProxies();
    renderApp('/settings/proxies');
    expect(await screen.findByRole('heading', { name: 'Connecting directly' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add a proxy' })).not.toBeInTheDocument();
    expect(screen.getByText('Adding a proxy needs system:settings:admin.')).toBeInTheDocument();
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
