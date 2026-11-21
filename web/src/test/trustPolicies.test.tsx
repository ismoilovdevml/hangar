import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import type { TrustPolicyInput } from '../pages/settings/trustPoliciesApi';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { trustPoliciesState } from '../mocks/trustPolicies';
import { choose } from './choose';
import { renderApp } from './render';

function capture(method: string, path: RegExp) {
  const bodies: TrustPolicyInput[] = [];
  server.events.on('request:start', async ({ request }) => {
    if (request.method === method && path.test(new URL(request.url).pathname)) {
      const text = await request.clone().text();
      if (text) bodies.push(JSON.parse(text) as TrustPolicyInput);
    }
  });
  return bodies;
}

afterEach(() => server.events.removeAllListeners());

const setupUser = () => userEvent.setup({ delay: null });
type User = ReturnType<typeof setupUser>;
const table = () => screen.findByRole('table', { name: 'CI trust policies' });
const row = async (name: string) => (await within(await table()).findByText(name)).closest('tr')!;

async function openNew(user: User) {
  await user.click(await screen.findByRole('button', { name: 'Add policy' }));
  return screen.findByRole('dialog', { name: 'New trust policy' });
}

describe('CI trust policies', () => {
  it('lists the policies with their issuer, conditions, account, lifetime and scopes', async () => {
    renderApp('/settings/trust-policies');
    expect(await screen.findByRole('heading', { level: 1, name: 'CI trust policies' })).toBeInTheDocument();
    const gh = await row('acme-app-github');
    expect(within(gh).getByText('GitHub Actions')).toBeInTheDocument();
    expect(within(gh).getByText('repository = acme/app')).toBeInTheDocument();
    expect(within(gh).getByText('ref ~ refs/heads/main or refs/tags/v*')).toBeInTheDocument();
    expect(within(gh).getByText('repo:npm-internal:write')).toBeInTheDocument();
    expect(within(gh).getByText(/15 min/)).toBeInTheDocument();
    const gl = await row('billing-gitlab');
    expect(within(gl).getByText('GitLab')).toBeInTheDocument();
    expect(within(gl).getByText(/30 min/)).toBeInTheDocument();
  });

  it('creates a GitHub Actions policy from the preset', async () => {
    const posts = capture('POST', /^\/api\/v1\/trust-policies$/);
    const user = setupUser();
    renderApp('/settings/trust-policies');
    const dialog = await openNew(user);
    expect(within(dialog).getByRole('button', { name: 'GitHub Actions' })).toHaveAttribute('aria-pressed', 'true');
    expect(within(dialog).getByLabelText('Issuer')).toHaveValue('https://token.actions.githubusercontent.com');
    expect(within(dialog).getByRole('combobox', { name: 'Claim of condition 1' })).toHaveValue('repository');
    expect(within(dialog).getByRole('combobox', { name: 'Claim of condition 3' })).toHaveValue('environment');

    // The preset leaves the repository to fill in: the owner is not pinned yet.
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    expect(within(dialog).getByText('Add at least one value.')).toBeInTheDocument();
    expect(posts).toHaveLength(0);

    await user.type(within(dialog).getByLabelText('Name'), 'web-release');
    await user.type(within(dialog).getByLabelText('Values of condition 1'), 'acme/web{Enter}');
    await user.type(within(dialog).getByLabelText('Service account'), 'gitea-runner');
    await user.type(within(dialog).getByLabelText('Scopes'), 'repo:npm-internal:write{Enter}');
    await user.clear(within(dialog).getByLabelText('Token lifetime (minutes)'));
    await user.type(within(dialog).getByLabelText('Token lifetime (minutes)'), '10');
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(posts[0]).toEqual({
      name: 'web-release',
      description: '',
      enabled: true,
      issuer: 'https://token.actions.githubusercontent.com',
      conditions: [
        { claim: 'repository', match: 'exact', values: ['acme/web'] },
        { claim: 'ref', match: 'glob', values: ['refs/heads/main', 'refs/tags/v*'] },
        { claim: 'environment', match: 'exact', values: ['production'] },
      ],
      service_account: 'gitea-runner',
      scopes: ['repo:npm-internal:write'],
      max_ttl_seconds: 600,
    });
    expect(trustPoliciesState.policies.find((p) => p.name === 'web-release')?.audience).toBe('https://pkg.acme.io');
    expect(await row('web-release')).toBeInTheDocument();
  });

  it('switches to the GitLab preset and edits conditions', async () => {
    const posts = capture('POST', /^\/api\/v1\/trust-policies$/);
    const user = setupUser();
    renderApp('/settings/trust-policies');
    const dialog = await openNew(user);
    await user.click(within(dialog).getByRole('button', { name: 'GitLab' }));
    expect(within(dialog).getByLabelText('Issuer')).toHaveValue('https://gitlab.com');
    expect(within(dialog).getByRole('combobox', { name: 'Claim of condition 1' })).toHaveValue('project_path');
    expect(within(dialog).getByRole('combobox', { name: 'Claim of condition 2' })).toHaveValue('ref_protected');
    expect(within(dialog).queryByRole('combobox', { name: 'Claim of condition 3' })).not.toBeInTheDocument();
    expect(within(dialog).getByText(/One must fix the owner: project_path, namespace_path/)).toBeInTheDocument();

    // A glob that does not stop at a "/" would let acme-evil/… in.
    await user.type(within(dialog).getByLabelText('Values of condition 1'), 'acme*{Enter}');
    await choose(user, within(dialog).getByRole('combobox', { name: 'Match of condition 1' }), 'glob');
    await user.type(within(dialog).getByLabelText('Name'), 'billing');
    await user.type(within(dialog).getByLabelText('Service account'), 'gitea-runner');
    await user.type(within(dialog).getByLabelText('Scopes'), 'repo:maven-releases:write{Enter}');
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    expect(within(dialog).getByText(/GitLab signs jobs of every account on the platform/)).toBeInTheDocument();
    expect(posts).toHaveLength(0);

    await user.click(within(dialog).getByRole('button', { name: 'Remove value acme*' }));
    await user.type(within(dialog).getByLabelText('Values of condition 1'), 'acme/*{Enter}');
    await user.click(within(dialog).getByRole('button', { name: 'Add condition' }));
    await user.type(within(dialog).getByRole('combobox', { name: 'Claim of condition 3' }), 'environment');
    await user.type(within(dialog).getByLabelText('Values of condition 3'), 'production{Enter}');
    await user.click(within(dialog).getByRole('button', { name: 'Remove condition 2' }));
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]!.conditions).toEqual([
      { claim: 'project_path', match: 'glob', values: ['acme/*'] },
      { claim: 'environment', match: 'exact', values: ['production'] },
    ]);
  });

  it('shows the server’s owner-pinning and account refusals at their fields', async () => {
    const user = setupUser();
    let answer = 'conditions: this issuer signs jobs of every account on the platform: add a condition on one of repository, repository_owner with exact values or globs that fix the owner (acme/*, not acme*)';
    server.use(http.post('*/api/v1/trust-policies', () => HttpResponse.json({ code: 'invalid_argument', message: answer }, { status: 400 })));
    renderApp('/settings/trust-policies');
    const dialog = await openNew(user);
    await user.type(within(dialog).getByLabelText('Name'), 'x');
    await user.type(within(dialog).getByLabelText('Values of condition 1'), 'acme/web{Enter}');
    await user.type(within(dialog).getByLabelText('Service account'), 'nobody');
    await user.type(within(dialog).getByLabelText('Scopes'), 'repo:npm-internal:write{Enter}');
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    expect(await within(dialog).findByText(/^This issuer signs jobs of every account on the platform: add a condition on one of repository/)).toBeInTheDocument();
    expect(within(dialog).getByText('Fix the highlighted fields and save again.')).toBeInTheDocument();

    answer = 'conditions: condition 2 (ref): "*" matches anything; leave the condition out instead';
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    expect(await within(dialog).findByText('"*" matches anything; leave the condition out instead.')).toBeInTheDocument();
    expect(within(dialog).getByLabelText('Values of condition 2')).toHaveAttribute('aria-invalid', 'true');

    answer = 'service_account "nobody": no such service account';
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    expect(await within(dialog).findByText('Service_account "nobody": no such service account.')).toBeInTheDocument();
    expect(within(dialog).getByLabelText('Service account')).toHaveAttribute('aria-invalid', 'true');
  });

  it('explains that only privileges you hold can be handed out', async () => {
    const user = setupUser();
    server.use(
      http.post('*/api/v1/trust-policies', () =>
        HttpResponse.json({ code: 'forbidden', message: 'trust policy x: you do not hold repo:*:admin' }, { status: 403 }),
      ),
    );
    renderApp('/settings/trust-policies');
    const dialog = await openNew(user);
    await user.type(within(dialog).getByLabelText('Name'), 'x');
    await user.type(within(dialog).getByLabelText('Values of condition 1'), 'acme/web{Enter}');
    await user.type(within(dialog).getByLabelText('Service account'), 'gitea-runner');
    await user.type(within(dialog).getByLabelText('Scopes'), 'repo:*:admin{Enter}');
    await user.click(within(dialog).getByRole('button', { name: 'Add policy' }));
    expect(await within(dialog).findByText(/You can only hand out privileges you hold yourself: remove the scopes/)).toBeInTheDocument();
  });

  it('edits a policy without renaming it and warns that minted tokens are revoked', async () => {
    const puts = capture('PUT', /^\/api\/v1\/trust-policies\//);
    const user = setupUser();
    renderApp('/settings/trust-policies');
    await user.click(within(await row('billing-gitlab')).getByRole('button', { name: 'Edit billing-gitlab' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit billing-gitlab' });
    expect(dialog).toHaveTextContent('Saving revokes the tokens this policy minted before.');
    expect(within(dialog).getByLabelText('Name')).toHaveAttribute('readonly');
    expect(within(dialog).getByLabelText('Token lifetime (minutes)')).toHaveValue(30);
    await user.click(within(dialog).getByRole('switch', { name: 'Enabled' }));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toMatchObject({ name: 'billing-gitlab', enabled: false, audience: 'https://pkg.acme.io', max_ttl_seconds: 1800 });
    expect(await within(await row('billing-gitlab')).findByText('Disabled')).toBeInTheDocument();
  });

  it('shows a copyable exchange for each policy', async () => {
    const user = setupUser();
    renderApp('/settings/trust-policies');
    await user.click(within(await row('acme-app-github')).getByRole('button', { name: 'How to use acme-app-github' }));
    const dialog = await screen.findByRole('dialog', { name: 'Use acme-app-github' });
    const workflow = within(dialog).getByRole('region', { name: 'GitHub Actions workflow' });
    expect(workflow).toHaveTextContent('id-token: write');
    expect(workflow).toHaveTextContent('$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://pkg.acme.io');
    const curl = within(dialog).getByRole('region', { name: 'Exchange with curl' });
    expect(curl).toHaveTextContent(/curl -sSf -X POST https?:\/\/[^ ]+\/api\/v1\/auth\/oidc\/exchange/);
    expect(curl).toHaveTextContent('\\"policy\\": \\"acme-app-github\\"');
    expect(within(dialog).getByRole('button', { name: 'Copy token exchange with curl' })).toBeInTheDocument();
  });

  it('deletes a policy after a confirmation', async () => {
    const user = setupUser();
    renderApp('/settings/trust-policies');
    await user.click(within(await row('billing-gitlab')).getByRole('button', { name: 'Delete billing-gitlab' }));
    const confirm = await screen.findByRole('alertdialog', { name: 'Delete trust policy billing-gitlab?' });
    await user.click(within(confirm).getByRole('button', { name: 'Delete policy' }));
    await waitFor(() => expect(trustPoliciesState.policies.map((p) => p.name)).toEqual(['acme-app-github']));
  });

  it('lets settings readers look but not change anything', async () => {
    server.use(
      http.get('*/api/v1/auth/session', () =>
        HttpResponse.json({ ...session, privileges: ['repo:*:read', 'system:settings:read'] }, { headers: { 'X-CSRF-Token': 'mock-csrf-0' } }),
      ),
    );
    const user = setupUser();
    renderApp('/settings/trust-policies');
    const gh = await row('acme-app-github');
    expect(screen.queryByRole('button', { name: 'Add policy' })).not.toBeInTheDocument();
    expect(within(gh).queryByRole('button', { name: 'Delete acme-app-github' })).not.toBeInTheDocument();
    await user.click(within(gh).getByRole('button', { name: 'View acme-app-github' }));
    const dialog = await screen.findByRole('dialog', { name: 'Trust policy acme-app-github' });
    expect(within(dialog).getByLabelText('Issuer')).toBeDisabled();
    expect(within(dialog).queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
  });
});
