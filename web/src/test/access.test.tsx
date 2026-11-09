import { choose } from './choose';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { accessState } from '../mocks/access';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);

describe('access & audit: members', () => {
  it('lists members with roles, source, tokens and status, and side panels', async () => {
    renderApp('/access');
    const table = await screen.findByRole('table', { name: 'Members' });
    const row = (await within(table).findByText('Jasur Karimov')).closest('tr')!;
    expect(within(row).getByText('Publisher')).toBeInTheDocument();
    expect(within(row).getByText('Single sign-on')).toBeInTheDocument();
    expect(within(row).getByText('Active')).toBeInTheDocument();
    // Service accounts are on their own tab.
    expect(within(table).queryByText('gitea-runner')).not.toBeInTheDocument();
    const locked = within(table).getByText('Bekzod Yusupov').closest('tr')!;
    expect(within(locked).getByText('Locked')).toBeInTheDocument();

    expect(screen.getByRole('region', { name: 'Sign-in providers' })).toBeInTheDocument();
    const audit = screen.getByRole('region', { name: 'Audit log' });
    expect((await within(audit).findAllByText(/to m\.nazarova/)).length).toBeGreaterThan(0);
    const tabs = screen.getByRole('navigation', { name: 'Access sections' });
    expect(within(tabs).getByRole('link', { name: 'Members' })).toHaveAttribute('aria-current', 'page');
    expect(within(tabs).getByRole('link', { name: /^Roles/ })).toHaveTextContent('Roles4');
  });

  it('filters by status and role through the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/access');
    const table = await screen.findByRole('table', { name: 'Members' });
    await within(table).findByText('Jasur Karimov');
    await choose(user, screen.getByLabelText('Status'), 'locked');
    await waitFor(() => expect(search(history).get('status')).toBe('locked'));
    await waitFor(() => expect(within(table).queryByText('Jasur Karimov')).not.toBeInTheDocument());
    expect(within(table).getByText('Bekzod Yusupov')).toBeInTheDocument();

    await choose(user, screen.getByLabelText('Status'), '');
    await choose(user, screen.getByLabelText('Role'), 'Admin');
    await waitFor(() => expect(search(history).get('role')).toBe('Admin'));
    await waitFor(() => expect(within(table).queryByText('Bekzod Yusupov')).not.toBeInTheDocument());
    expect(within(table).getByText('Nodira Tursunova')).toBeInTheDocument();
  });

  it('opens a member drawer and disables, unlocks and resets them', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/access');
    await user.click(await screen.findByRole('button', { name: 'Open Bekzod Yusupov' }));
    await waitFor(() => expect(search(history).get('user')).toBe('u_09'));
    const drawer = await screen.findByRole('dialog', { name: 'Bekzod Yusupov' });
    expect(within(drawer).getByText('Locked')).toBeInTheDocument();

    await user.click(within(drawer).getByRole('button', { name: 'Unlock' }));
    expect(await within(drawer).findByText('Account unlocked.')).toBeInTheDocument();
    await waitFor(() => expect(within(drawer).queryByRole('button', { name: 'Unlock' })).not.toBeInTheDocument());

    await user.click(within(drawer).getByRole('button', { name: 'Disable' }));
    const confirm = await screen.findByRole('dialog', { name: 'Disable Bekzod Yusupov?' });
    await user.click(within(confirm).getByRole('button', { name: 'Disable account' }));
    expect(await within(drawer).findByText('Account disabled.')).toBeInTheDocument();
    expect(accessState.users.find((u) => u.id === 'u_09')?.disabled).toBe(true);
    expect(await within(drawer).findByRole('button', { name: 'Enable' })).toBeInTheDocument();

    await user.click(within(drawer).getByRole('button', { name: 'Reset password' }));
    const reset = await screen.findByRole('dialog', { name: 'Reset password for Bekzod Yusupov' });
    await user.click(within(reset).getByRole('button', { name: 'Generate' }));
    expect((within(reset).getByLabelText('New password') as HTMLInputElement).value).toMatch(/^\w{5}-\w{5}-\w{5}-\w{5}$/);
    await user.click(within(reset).getByRole('button', { name: 'Set password' }));
    expect(await within(reset).findByText(/Password set/)).toBeInTheDocument();
    await user.click(within(reset).getByRole('button', { name: 'Done' }));

    // Effective permissions explain the grant.
    await user.click(within(drawer).getByRole('tab', { name: 'Permissions' }));
    expect(await within(drawer).findByText('docker-prod')).toBeInTheDocument();
    expect(within(drawer).getAllByText(/Read: Reader \(repo:\*:read\)/).length).toBeGreaterThan(0);
    expect(within(drawer).getByText(/disabled: none of this applies/)).toBeInTheDocument();

    await user.keyboard('{Escape}');
    await waitFor(() => expect(search(history).get('user')).toBeNull());
  });

  it('keeps you from disabling or deleting yourself', async () => {
    renderApp('/access?user=u_01');
    const drawer = await screen.findByRole('dialog', { name: 'Dilshod Ismoilov' });
    await within(drawer).findByText('Overview');
    expect(within(drawer).getByRole('button', { name: 'Edit' })).toBeInTheDocument();
    expect(within(drawer).queryByRole('button', { name: 'Disable' })).not.toBeInTheDocument();
    expect(within(drawer).queryByRole('button', { name: 'Delete' })).not.toBeInTheDocument();
    expect(within(drawer).queryByRole('button', { name: 'Reset password' })).not.toBeInTheDocument();
  });

  it('adds a member with a role and validates the form', async () => {
    const user = userEvent.setup();
    renderApp('/access');
    await user.click(await screen.findByRole('button', { name: 'Add member' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add member' });
    expect(within(dialog).getByLabelText('Username')).toHaveFocus();
    await user.click(within(dialog).getByRole('button', { name: 'Add member' }));
    expect(within(dialog).getByText(/Start with a letter or digit/)).toBeInTheDocument();
    expect(within(dialog).getByText(/At least 12 characters/)).toBeInTheDocument();

    await user.type(within(dialog).getByLabelText('Username'), 'o.rahimov');
    await user.type(within(dialog).getByLabelText(/Display name/), 'Otabek Rahimov');
    await user.type(within(dialog).getByLabelText('Initial password'), 'a long initial passphrase');
    await user.click(await within(dialog).findByRole('checkbox', { name: /Publisher/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Add member' }));
    expect(await screen.findByRole('dialog', { name: 'Otabek Rahimov' })).toBeInTheDocument();
    expect(accessState.users.find((u) => u.username === 'o.rahimov')?.roles).toEqual(['Reader', 'Publisher']);
  });
});

describe('access & audit: tokens', () => {
  it('creates a token from a preset and shows the secret with ready snippets exactly once', async () => {
    const user = userEvent.setup();
    renderApp('/access?tab=tokens');
    const table = await screen.findByRole('table', { name: 'All access tokens' });
    expect(await within(table).findByText('gitea deploy')).toBeInTheDocument();
    expect(within(table).getByText('10.8.0.4')).toBeInTheDocument();
    expect(within(within(table).getByText('old laptop').closest('tr')!).getByText('Expired')).toBeInTheDocument();
    expect(within(within(table).getByText('laptop CLI').closest('tr')!).getByText('Expiring soon')).toBeInTheDocument();
    const updater = within(table).getByText('image updater').closest('tr')!;
    expect(within(updater).getByText('Read-only')).toBeInTheDocument();
    expect(within(updater).getByText('10.8.0.0/16')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'New token' }));
    const dialog = await screen.findByRole('dialog', { name: 'New personal access token' });
    await user.click(within(dialog).getByRole('button', { name: 'Create token' }));
    expect(within(dialog).getByText('Give the token a name you will recognise.')).toBeInTheDocument();
    expect(within(dialog).getAllByRole('radio', { name: /days|year|Custom date/ }).map((r) => r.closest('label')!.textContent)).toEqual([
      '7 days',
      '30 days',
      '90 days',
      '1 year',
      'Custom date',
    ]);

    await user.type(within(dialog).getByLabelText('Name'), 'ci publish');
    await user.click(within(dialog).getByRole('radio', { name: '90 days' }));
    await user.click(within(dialog).getByRole('radio', { name: /Publish to selected repositories/ }));
    const picker = await within(dialog).findByRole('group', { name: 'Repositories' });
    await user.click(await within(picker).findByRole('checkbox', { name: 'nuget-internal' }));
    await user.type(within(dialog).getByLabelText('Or a pattern'), 'npm-*');
    await user.click(within(dialog).getByRole('button', { name: 'Add pattern' }));
    expect(within(dialog).getByRole('button', { name: 'Remove npm-*' })).toBeInTheDocument();
    const summary = within(dialog).getByTestId('token-summary');
    expect(summary).toHaveTextContent('Can publish to nuget-internal and npm-*');
    expect(summary).toHaveTextContent('Can download from nuget-internal and npm-*');
    expect(summary).toHaveTextContent('Cannot manage users & roles');

    await user.click(within(dialog).getByText('Restrictions (optional)'));
    await user.type(within(dialog).getByLabelText('Works only from these addresses'), 'office');
    await user.click(within(dialog).getByRole('button', { name: 'Create token' }));
    expect(within(dialog).getByText(/office is not an IP address/)).toBeInTheDocument();
    await user.clear(within(dialog).getByLabelText('Works only from these addresses'));
    await user.type(within(dialog).getByLabelText('Works only from these addresses'), '10.20.0.0/16');

    await user.click(within(dialog).getByRole('button', { name: 'Create token' }));
    const secretDialog = await screen.findByRole('dialog', { name: 'Copy your token now' });
    const secret = within(secretDialog).getByTestId('token-secret').textContent!;
    expect(secret).toMatch(/^hgr_/);
    expect(within(secretDialog).getByRole('button', { name: 'Copy token' })).toBeInTheDocument();
    const created = accessState.tokens.find((t) => t.name === 'ci publish')!;
    expect(created.scopes).toEqual(['repo:npm-*:read', 'repo:npm-*:write', 'repo:nuget-internal:read', 'repo:nuget-internal:write']);
    expect(created.allowed_cidrs).toEqual(['10.20.0.0/16']);
    const days = (new Date(created.expires_at).getTime() - Date.now()) / 86_400_000;
    expect(Math.round(days)).toBe(90);

    // Snippets carry the server URL and the secret.
    expect(within(secretDialog).getByText(`curl -fsS -H "Authorization: Bearer ${secret}" https://pkg.acme.io/api/v1/auth/session`)).toBeInTheDocument();
    expect(within(secretDialog).getByText(`HANGAR_TOKEN=${secret}`)).toBeInTheDocument();
    await user.click(within(secretDialog).getByRole('radio', { name: 'Package clients' }));
    expect(await within(secretDialog).findByText(`//pkg.acme.io/repository/npm-internal/:_authToken=${secret}`)).toBeInTheDocument();
    expect(within(secretDialog).getByText(/dotnet nuget push <package>.nupkg -s https:\/\/pkg.acme.io\/repository\/nuget-internal\/index.json/)).toBeInTheDocument();
    await user.click(within(secretDialog).getByRole('radio', { name: 'Automation' }));
    expect(within(secretDialog).getByText('Give this to an automated client')).toBeInTheDocument();
    expect(within(secretDialog).getByText('# Start with the guide: https://pkg.acme.io/llms.txt')).toBeInTheDocument();
    expect(within(secretDialog).getByRole('link', { name: '/llms.txt' })).toHaveAttribute('href', '/llms.txt');

    const done = within(secretDialog).getByRole('button', { name: 'Done' });
    expect(done).toBeDisabled();
    await user.click(within(secretDialog).getByRole('checkbox', { name: /stored the token/ }));
    await user.click(done);
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    // The list never shows a secret.
    expect(await within(table).findByText('ci publish')).toBeInTheDocument();
    expect(within(table).getByText('ci publish').closest('tr')!.textContent).toMatch(/hgr_Ab3d…/);
    expect(screen.queryByText(/hgr_Ab3dEf6h/)).not.toBeInTheDocument();
  });

  it('warns about full admin and builds a custom token with a live summary', async () => {
    const user = userEvent.setup();
    renderApp('/account');
    await user.click(await screen.findByRole('button', { name: 'New token' }));
    const dialog = await screen.findByRole('dialog', { name: 'New personal access token' });
    await user.click(within(dialog).getByRole('radio', { name: /Full admin/ }));
    expect(within(dialog).getByRole('note')).toHaveTextContent(/can do everything you can/);
    expect(within(dialog).getByTestId('token-summary')).toHaveTextContent('Can manage every administration area');

    await user.click(within(dialog).getByRole('radio', { name: /^Custom ?Pick/ }));
    await user.type(within(dialog).getByLabelText('Or a pattern'), 'docker-*');
    await user.click(within(dialog).getByRole('button', { name: 'Add pattern' }));
    await user.click(within(dialog).getByRole('checkbox', { name: 'Delete' }));
    await choose(user, within(dialog).getByRole('combobox', { name: /Audit log/ }), 'Read');
    const summary = within(dialog).getByTestId('token-summary');
    expect(summary).toHaveTextContent('Can delete packages in docker-*');
    expect(summary).toHaveTextContent('Can download from docker-*');
    expect(summary).toHaveTextContent('Can view audit log');
    expect(summary).toHaveTextContent('Cannot publish packages');

    await user.click(within(dialog).getByText('Restrictions (optional)'));
    await user.click(within(dialog).getByRole('checkbox', { name: /Read-only/ }));
    expect(summary).toHaveTextContent('Cannot change anything: the token is read-only');
    expect(summary).not.toHaveTextContent('Can delete packages');

    await user.type(within(dialog).getByLabelText('Name'), 'audit reader');
    await user.click(within(dialog).getByRole('button', { name: 'Create token' }));
    await screen.findByRole('dialog', { name: 'Copy your token now' });
    expect(accessState.tokens.find((t) => t.name === 'audit reader')).toMatchObject({
      scopes: ['repo:docker-*:delete', 'repo:docker-*:read', 'system:audit:read'],
      read_only: true,
    });
  });

  it('greys out what the owner cannot grant and explains why', async () => {
    server.use(
      http.get('*/api/v1/auth/session', () =>
        HttpResponse.json({ ...session, privileges: ['repo:*:read', 'repo:npm-*:write'], user: { ...session.user, roles: ['Publisher'] } }),
      ),
    );
    const user = userEvent.setup();
    renderApp('/account');
    await user.click(await screen.findByRole('button', { name: 'New token' }));
    const dialog = await screen.findByRole('dialog', { name: 'New personal access token' });
    const admin = within(dialog).getByRole('radio', { name: /Full admin/ });
    expect(admin).toBeDisabled();
    expect(admin.closest('label')).toHaveTextContent('Only administrators can create a full-admin token.');
    expect(within(dialog).getByRole('radio', { name: /Security reviewer/ })).toBeDisabled();
    expect(within(dialog).getByRole('radio', { name: /Repository admin/ })).toBeDisabled();

    await user.click(within(dialog).getByRole('radio', { name: /Publish to selected repositories/ }));
    const picker = await within(dialog).findByRole('group', { name: 'Repositories' });
    expect(await within(picker).findByRole('checkbox', { name: /npm-internal/ })).toBeEnabled();
    const docker = within(picker).getByRole('checkbox', { name: /docker-prod/ });
    expect(docker).toBeDisabled();
    expect(docker.closest('label')).toHaveAttribute('title', 'Your roles do not let you publish to docker-prod.');

    await user.click(within(dialog).getByRole('radio', { name: /^Custom ?Pick/ }));
    expect(within(dialog).getByRole('checkbox', { name: 'Admin' })).toBeDisabled();
    expect(within(dialog).getByRole('group', { name: 'Administration areas' })).toHaveTextContent('Your roles grant no access here');
  });

  it('explains that a password change comes before new tokens', async () => {
    server.use(
      http.post('*/api/v1/tokens', () =>
        HttpResponse.json({ code: 'password_change_required', message: 'set a new password before creating tokens' }, { status: 403 }),
      ),
    );
    const user = userEvent.setup();
    renderApp('/account');
    await user.click(await screen.findByRole('button', { name: 'New token' }));
    const dialog = await screen.findByRole('dialog', { name: 'New personal access token' });
    await user.type(within(dialog).getByLabelText('Name'), 'ci');
    await user.click(within(dialog).getByRole('button', { name: 'Create token' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/Set a new password first/);
  });

  it('rotates a token, shows the new secret and ends the old one', async () => {
    const user = userEvent.setup();
    renderApp('/account');
    const table = await screen.findByRole('table', { name: 'Your access tokens' });
    await user.click(await within(table).findByRole('button', { name: 'Rotate token release script' }));
    const dialog = await screen.findByRole('dialog', { name: 'Rotate “release script”?' });
    expect(dialog).toHaveTextContent('same name, permissions and restrictions, valid for 230 days');
    await user.click(within(dialog).getByRole('radio', { name: /Revoke it now/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Create replacement' }));
    const secretDialog = await screen.findByRole('dialog', { name: 'Copy the new token now' });
    expect(within(secretDialog).getByTestId('token-secret').textContent).toMatch(/^hgr_/);
    expect(secretDialog).toHaveTextContent('The old token hgr_0002… is revoked.');
    const mine = accessState.tokens.filter((t) => t.name === 'release script');
    expect(mine).toHaveLength(1);
    expect(mine[0]!.id).not.toBe('t_0002');
    expect(mine[0]!.scopes).toEqual(['repo:docker-prod:write']);
    expect(accessState.audit[0]?.action).toBe('token.rotate');
  });

  it('rotates with a grace period for the old token', async () => {
    const user = userEvent.setup();
    renderApp('/account');
    const table = await screen.findByRole('table', { name: 'Your access tokens' });
    await user.click(await within(table).findByRole('button', { name: 'Rotate token laptop CLI' }));
    const dialog = await screen.findByRole('dialog', { name: 'Rotate “laptop CLI”?' });
    await user.click(within(dialog).getByRole('button', { name: 'Create replacement' }));
    const secretDialog = await screen.findByRole('dialog', { name: 'Copy the new token now' });
    expect(secretDialog).toHaveTextContent(/The old token hgr_0001… keeps working until .* \(in (23 h|1 day)\)/);
    expect(accessState.tokens.filter((t) => t.name === 'laptop CLI')).toHaveLength(2);
  });

  it('revokes a token after confirmation', async () => {
    const user = userEvent.setup();
    renderApp('/access?tab=tokens');
    const table = await screen.findByRole('table', { name: 'All access tokens' });
    await user.click(await within(table).findByRole('button', { name: 'Revoke token image updater' }));
    const confirm = await screen.findByRole('dialog', { name: 'Revoke “image updater”?' });
    expect(within(confirm).getByText(/service account argo-cd/)).toBeInTheDocument();
    await user.click(within(confirm).getByRole('button', { name: 'Revoke token' }));
    await waitFor(() => expect(within(table).queryByText('image updater')).not.toBeInTheDocument());
    expect(accessState.audit[0]?.action).toBe('token.revoke');
  });

  it('creates a service account token from its drawer', async () => {
    const user = userEvent.setup();
    renderApp('/access?tab=service-accounts');
    const table = await screen.findByRole('table', { name: 'Service accounts' });
    await user.click(await within(table).findByRole('button', { name: 'Open gitea-runner' }));
    const drawer = await screen.findByRole('dialog', { name: 'gitea-runner' });
    await user.click(within(drawer).getByRole('tab', { name: 'Tokens' }));
    expect(await within(drawer).findByText('gitea deploy')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'New token' }));
    const dialog = await screen.findByRole('dialog', { name: 'New token for gitea-runner' });
    await user.type(within(dialog).getByLabelText('Name'), 'release');
    // The service account (CI deployer) cannot administer repositories.
    expect(within(dialog).getByRole('radio', { name: /Full admin/ })).toBeDisabled();
    await user.click(within(dialog).getByRole('radio', { name: /Publish to selected repositories/ }));
    await user.click(await within(dialog).findByRole('checkbox', { name: 'docker-prod' }));
    await user.click(within(dialog).getByRole('button', { name: 'Create token' }));
    await screen.findByRole('dialog', { name: 'Copy your token now' });
    expect(accessState.tokens.find((t) => t.name === 'release')).toMatchObject({
      owner: 'gitea-runner',
      owner_kind: 'service_account',
      scopes: ['repo:docker-prod:read', 'repo:docker-prod:write'],
    });
  });
});

describe('access & audit: roles and audit', () => {
  it('creates a custom role with the privilege editor', async () => {
    const user = userEvent.setup();
    renderApp('/access?tab=roles');
    const table = await screen.findByRole('table', { name: 'Roles' });
    expect(await within(table).findByText('CI deployer')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'New role' }));
    const dialog = await screen.findByRole('dialog', { name: 'New role' });
    await user.type(within(dialog).getByLabelText('Name'), 'npm team');
    const first = within(dialog).getByRole('group', { name: 'Privilege 1' });
    const pattern = within(first).getByLabelText('Repository name or pattern');
    await user.clear(pattern);
    await user.type(pattern, 'npm-*');
    await choose(user, within(first).getByLabelText('Action'), 'write');
    expect(within(first).getByText('Publish to repositories matching npm-*')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: /Administration privilege/ }));
    const second = within(dialog).getByRole('group', { name: 'Privilege 2' });
    expect(within(second).getByText('Read access to Audit log')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Create role' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(await within(table).findByText('npm team')).toBeInTheDocument();
  });

  it('shows built-in roles read-only with their members', async () => {
    const user = userEvent.setup();
    renderApp('/access?tab=roles');
    const table = await screen.findByRole('table', { name: 'Roles' });
    await user.click(await within(table).findByRole('button', { name: 'Admin' }));
    const dialog = await screen.findByRole('dialog', { name: 'Role Admin' });
    expect(within(dialog).getByText('repo:*:admin')).toBeInTheDocument();
    expect(within(dialog).queryByRole('button', { name: 'Save role' })).not.toBeInTheDocument();
    expect(await within(dialog).findByText('Nodira Tursunova')).toBeInTheDocument();
  });

  it('filters the audit log to access events by category', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/access?tab=audit');
    const table = await screen.findByRole('table', { name: 'Access audit log' });
    expect(await within(table).findByText('No events of this type yet.')).toBeInTheDocument();
    await user.click(screen.getByRole('radio', { name: 'Sign-ins' }));
    await waitFor(() => expect(search(history).get('events')).toBe('signins'));
    expect((await within(table).findAllByText('failed to sign in')).length).toBeGreaterThan(0);
    expect(within(table).getAllByText('Denied').length).toBeGreaterThan(0);
    expect(within(table).queryByText('created repository repo:npm-sandbox')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Export CSV' })).toHaveAttribute('href', '/api/v1/audit/export?format=csv&action=auth.*');
  });

  it('tells people without admin rights where their own settings are', async () => {
    server.use(
      http.get('*/api/v1/auth/session', () =>
        HttpResponse.json({ ...session, privileges: ['repo:*:read'], user: { ...session.user, roles: ['Reader'] } }, { headers: { 'X-CSRF-Token': 'mock-csrf-0' } }),
      ),
    );
    renderApp('/access');
    expect(await screen.findByText(/You don’t have access to people, tokens and roles/)).toBeInTheDocument();
    expect(within(screen.getByRole('main')).getByRole('link', { name: 'My account' })).toHaveAttribute('href', '/account');
    expect(screen.queryByRole('table', { name: 'Members' })).not.toBeInTheDocument();
  });
});

describe('my account', () => {
  it('changes the password and signs out other sessions', async () => {
    const user = userEvent.setup();
    renderApp('/account');
    expect(await screen.findByRole('heading', { level: 1, name: 'My account' })).toBeInTheDocument();
    expect(screen.getAllByText(/laptop CLI/).length).toBeGreaterThan(0);
    const pw = screen.getByRole('region', { name: 'Password' });
    await user.type(within(pw).getByLabelText('Current password'), 'password1234');
    await user.type(within(pw).getByLabelText('New password'), 'a much longer passphrase');
    await user.type(within(pw).getByLabelText('Repeat new password'), 'a different passphrase');
    await user.click(within(pw).getByRole('button', { name: 'Change password' }));
    expect(within(pw).getByText('The passwords do not match.')).toBeInTheDocument();
    await user.clear(within(pw).getByLabelText('Current password'));
    await user.type(within(pw).getByLabelText('Current password'), 'wrong password');
    await user.clear(within(pw).getByLabelText('Repeat new password'));
    await user.type(within(pw).getByLabelText('Repeat new password'), 'a much longer passphrase');
    await user.click(within(pw).getByRole('button', { name: 'Change password' }));
    expect(await within(pw).findByText('the current password is incorrect')).toBeInTheDocument();
    await user.clear(within(pw).getByLabelText('Current password'));
    await user.type(within(pw).getByLabelText('Current password'), 'password1234');
    await user.click(within(pw).getByRole('button', { name: 'Change password' }));
    expect(await within(pw).findByText(/Password changed/)).toBeInTheDocument();

    const devices = screen.getByRole('region', { name: 'Signed-in devices' });
    await waitFor(() => expect(within(devices).queryByText(/Firefox on Linux/)).not.toBeInTheDocument());
    expect(within(devices).getByText(/Chrome on macOS/)).toBeInTheDocument();
    expect(within(devices).getByText('This session')).toBeInTheDocument();
  });

  it('ends another session and lists personal tokens', async () => {
    const user = userEvent.setup();
    renderApp('/account');
    const devices = await screen.findByRole('region', { name: 'Signed-in devices' });
    await user.click(await within(devices).findByRole('button', { name: 'End session from 192.0.2.44' }));
    const confirm = await screen.findByRole('dialog', { name: 'End this session?' });
    await user.click(within(confirm).getByRole('button', { name: 'End session' }));
    expect(await within(devices).findByText('Session ended.')).toBeInTheDocument();
    expect(within(devices).getByRole('button', { name: 'Sign out everywhere else' })).toBeDisabled();

    const tokens = screen.getByRole('table', { name: 'Your access tokens' });
    expect(await within(tokens).findByText('release script')).toBeInTheDocument();
    expect(within(tokens).queryByText('gitea deploy')).not.toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'What you can do' })).toBeInTheDocument();
  });
});
