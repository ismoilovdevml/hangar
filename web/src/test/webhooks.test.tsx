import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import type { WebhookInput } from '../pages/settings/webhooksApi';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { webhooksState } from '../mocks/webhooks';
import { choose } from './choose';
import { renderApp } from './render';

function capture(method: string, path: RegExp) {
  const bodies: WebhookInput[] = [];
  server.events.on('request:start', async ({ request }) => {
    if (request.method === method && path.test(new URL(request.url).pathname)) {
      const text = await request.clone().text();
      if (text) bodies.push(JSON.parse(text) as WebhookInput);
    }
  });
  return bodies;
}

afterEach(() => server.events.removeAllListeners());

const setupUser = () => userEvent.setup({ delay: null });
const signInWith = (privileges: string[]) =>
  server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges }, { headers: { 'X-CSRF-Token': 'mock-csrf-0' } })));
const table = () => screen.findByRole('table', { name: 'Webhooks' });
const row = async (name: string) => {
  const t = await table();
  const cell = await within(t).findByText(name);
  return cell.closest('tr')!;
};

describe('webhooks page', () => {
  it('lists webhooks with the shortened URL, events, state and last delivery', async () => {
    renderApp('/settings/webhooks');
    expect(await screen.findByRole('heading', { level: 1, name: 'Webhooks' })).toBeInTheDocument();
    const ci = await row('ci-trigger');
    expect(within(ci).getByText('https://ci.acme.example/***')).toBeInTheDocument();
    expect(within(ci).getByText('Enabled')).toBeInTheDocument();
    expect(within(ci).getByText(/JSON · signed/)).toBeInTheDocument();
    expect(within(ci).getByText('Every repository')).toBeInTheDocument();
    expect(within(ci).getByText('HTTP 200')).toBeInTheDocument();
    const slack = await row('releases to Slack');
    expect(within(slack).getByText('Repositories: npm-*, maven-releases')).toBeInTheDocument();
    expect(within(slack).getByText('2 failures in a row')).toBeInTheDocument();
    const teams = await row('security in Teams');
    expect(within(teams).getByText('Disabled by the server')).toBeInTheDocument();
    expect(within(teams).getByText(/disabled after 20 failed attempts in a row; last error: HTTP 410 Gone/)).toBeInTheDocument();
    // The full URL with its credential never reaches the page.
    expect(document.body.textContent).not.toContain('token=s3cr3t');
  });

  it('creates a webhook; the URL and the secret are sent once and never shown', async () => {
    const posts = capture('POST', /^\/api\/v1\/webhooks$/);
    const user = setupUser();
    renderApp('/settings/webhooks');
    await user.click(await screen.findByRole('button', { name: 'Add webhook' }));
    const dialog = await screen.findByRole('dialog', { name: 'New webhook' });
    await user.click(within(dialog).getByRole('button', { name: 'Add webhook' }));
    expect(within(dialog).getByText('Use 1 to 64 letters, digits, spaces, ".", "_" or "-", starting with a letter or digit.')).toBeInTheDocument();
    expect(within(dialog).getByText('Enter the address the events are sent to.')).toBeInTheDocument();
    expect(posts).toHaveLength(0);

    await user.type(within(dialog).getByLabelText('Name'), 'deploys');
    await user.type(within(dialog).getByLabelText('URL'), 'https://chat.acme.example/hooks/abc');
    await choose(user, within(dialog).getByLabelText('Message format'), 'slack');
    await user.click(within(dialog).getByRole('checkbox', { name: /Component deleted/ }));
    await user.type(within(dialog).getByLabelText(/^Repositories/), 'docker-*{Enter}');
    await user.type(within(dialog).getByLabelText(/^Signing secret/), 'shh');
    await user.click(within(dialog).getByRole('button', { name: 'Add webhook' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(posts[0]).toEqual({
      name: 'deploys',
      url: 'https://chat.acme.example/hooks/abc',
      events: ['component.published', 'component.deleted'],
      repositories: ['docker-*'],
      format: 'slack',
      secret: 'shh',
      enabled: true,
    });
    expect(screen.getAllByText('Webhook deploys saved.').length).toBeGreaterThan(0);
    const created = await row('deploys');
    expect(within(created).getByText('https://chat.acme.example/***')).toBeInTheDocument();
    expect(webhooksState.stored('deploys')).toEqual({ url: 'https://chat.acme.example/hooks/abc', secret: 'shh' });
  });

  it('shows the server’s refusal of the URL at the field', async () => {
    const user = setupUser();
    renderApp('/settings/webhooks');
    await user.click(await screen.findByRole('button', { name: 'Add webhook' }));
    const dialog = await screen.findByRole('dialog', { name: 'New webhook' });
    await user.type(within(dialog).getByLabelText('Name'), 'internal');
    const url = within(dialog).getByLabelText('URL');
    await user.type(url, 'http://10.0.0.5/hook');
    await user.click(within(dialog).getByRole('button', { name: 'Add webhook' }));
    expect(await within(dialog).findByText(/The URL is refused by the outbound address policy: 10\.0\.0\.5 is an internal or reserved address/)).toBeInTheDocument();
    expect(url).toHaveAttribute('aria-invalid', 'true');
    expect(within(dialog).getByText('Fix the highlighted fields and save again.')).toBeInTheDocument();
  });

  it('asks for the secret again when the URL moves to another host, as the server does', async () => {
    const puts = capture('PUT', /^\/api\/v1\/webhooks\//);
    const user = setupUser();
    renderApp('/settings/webhooks');
    await user.click(within(await row('ci-trigger')).getByRole('button', { name: 'Edit ci-trigger' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit ci-trigger' });
    expect(within(dialog).getByText('https://ci.acme.example/***')).toBeInTheDocument();
    expect(within(dialog).getByRole('radio', { name: /Keep the stored secret/ })).toBeChecked();

    await user.click(within(dialog).getByRole('button', { name: 'Replace URL' }));
    await user.type(within(dialog).getByLabelText('New URL'), 'https://ci2.acme.example/hooks/hangar');
    expect(within(dialog).getByText(/The new URL is on another host/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(await within(dialog).findByText(/Url changed from https:\/\/ci\.acme\.example:443 to https:\/\/ci2\.acme\.example:443: the stored secret is only sent/)).toBeInTheDocument();
    expect(puts[0]).not.toHaveProperty('secret');

    await user.click(within(dialog).getByRole('radio', { name: /Replace it/ }));
    await user.type(within(dialog).getByLabelText('New signing secret'), 'new-secret');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(puts[1]).toMatchObject({ url: 'https://ci2.acme.example/hooks/hangar', secret: 'new-secret' });
    expect(webhooksState.stored('ci-trigger')).toEqual({ url: 'https://ci2.acme.example/hooks/hangar', secret: 'new-secret' });
  });

  it('keeps the stored URL and removes the secret on request', async () => {
    const puts = capture('PUT', /^\/api\/v1\/webhooks\//);
    const user = setupUser();
    renderApp('/settings/webhooks');
    await user.click(within(await row('ci-trigger')).getByRole('button', { name: 'Edit ci-trigger' }));
    const dialog = await screen.findByRole('dialog', { name: 'Edit ci-trigger' });
    await user.click(within(dialog).getByRole('radio', { name: /Remove it/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).not.toHaveProperty('url');
    expect(puts[0]).toMatchObject({ clear_secret: true });
    expect(webhooksState.stored('ci-trigger')).toEqual({ url: 'https://ci.acme.example/hooks/hangar?token=s3cr3t', secret: '' });
  });

  it('sends a test ping and says how it went', async () => {
    const user = setupUser();
    renderApp('/settings/webhooks');
    await user.click(within(await row('ci-trigger')).getByRole('button', { name: 'Test ci-trigger' }));
    expect(await screen.findByText('Test of ci-trigger: delivered · HTTP 200 in 64 ms')).toBeInTheDocument();
    await user.click(within(await row('security in Teams')).getByRole('button', { name: 'Test security in Teams' }));
    expect(await screen.findByText('Test of security in Teams: failed · HTTP 410 in 88 ms · HTTP 410 Gone')).toBeInTheDocument();
  });

  it('deletes a webhook after a confirmation', async () => {
    const user = setupUser();
    renderApp('/settings/webhooks');
    await user.click(within(await row('releases to Slack')).getByRole('button', { name: 'Delete releases to Slack' }));
    const confirm = await screen.findByRole('alertdialog', { name: 'Delete webhook releases to Slack?' });
    expect(confirm).toHaveTextContent('https://hooks.slack.com/***');
    await user.click(within(confirm).getByRole('button', { name: 'Delete webhook' }));
    await waitFor(() => expect(webhooksState.webhooks.map((h) => h.name)).toEqual(['ci-trigger', 'security in Teams']));
    expect(await screen.findAllByText('Webhook releases to Slack deleted.')).not.toHaveLength(0);
  });

  it('lets readers look but not change anything', async () => {
    signInWith(['repo:*:read', 'system:webhooks:read']);
    const user = setupUser();
    renderApp('/settings/webhooks');
    const ci = await row('ci-trigger');
    expect(screen.queryByRole('button', { name: 'Add webhook' })).not.toBeInTheDocument();
    expect(within(ci).queryByRole('button', { name: 'Test ci-trigger' })).not.toBeInTheDocument();
    expect(within(ci).queryByRole('button', { name: 'Delete ci-trigger' })).not.toBeInTheDocument();
    await user.click(within(ci).getByRole('button', { name: 'View ci-trigger' }));
    const dialog = await screen.findByRole('dialog', { name: 'Webhook ci-trigger' });
    expect(within(dialog).getByLabelText('Name')).toBeDisabled();
    expect(within(dialog).queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    expect(within(dialog).queryByRole('button', { name: 'Replace URL' })).not.toBeInTheDocument();
  });

  it('opens /settings on webhooks for who may only read those', async () => {
    signInWith(['repo:*:read', 'system:webhooks:read']);
    const { history } = renderApp('/settings');
    await waitFor(() => expect(history.location.pathname).toBe('/settings/webhooks'));
  });
});

describe('webhook deliveries', () => {
  const openDeliveries = async (user: ReturnType<typeof setupUser>, name: string) => {
    await user.click(within(await row(name)).getByRole('button', { name: `Deliveries of ${name}` }));
    return screen.findByRole('dialog', { name: `Deliveries of ${name}` });
  };

  it('pages through the deliveries, newest first, and filters them by state', async () => {
    const user = setupUser();
    renderApp('/settings/webhooks');
    const drawer = await openDeliveries(user, 'ci-trigger');
    const list = await within(drawer).findByRole('list', { name: 'Deliveries' });
    // 33 deliveries, 25 a page.
    expect(within(list).getAllByRole('listitem')).toHaveLength(25);
    expect(within(list).getAllByRole('listitem')[0]).toHaveTextContent('Pending');
    expect(within(drawer).getByRole('button', { name: 'Previous' })).toBeDisabled();
    await user.click(within(drawer).getByRole('button', { name: 'Next' }));
    await waitFor(() => expect(within(within(drawer).getByRole('list', { name: 'Deliveries' })).getAllByRole('listitem')).toHaveLength(8));
    expect(within(drawer).getByRole('button', { name: 'Next' })).toBeDisabled();
    await user.click(within(drawer).getByRole('button', { name: 'Previous' }));
    await waitFor(() => expect(within(within(drawer).getByRole('list', { name: 'Deliveries' })).getAllByRole('listitem')).toHaveLength(25));

    await choose(user, within(drawer).getByLabelText('State'), 'failed');
    await waitFor(() => expect(within(within(drawer).getByRole('list', { name: 'Deliveries' })).getAllByRole('listitem')).toHaveLength(2));
    for (const item of within(within(drawer).getByRole('list', { name: 'Deliveries' })).getAllByRole('listitem')) {
      expect(item).toHaveTextContent('Failed');
      expect(item).toHaveTextContent('HTTP 502 in 3012 ms · HTTP 502 Bad Gateway');
    }
    expect(within(drawer).queryByRole('navigation', { name: 'Pages of deliveries' })).not.toBeInTheDocument();
  });

  it('shows a delivery with its payload and sends it again', async () => {
    const user = setupUser();
    renderApp('/settings/webhooks');
    const drawer = await openDeliveries(user, 'ci-trigger');
    await choose(user, within(drawer).getByLabelText('State'), 'failed');
    const list = await within(drawer).findByRole('list', { name: 'Deliveries' });
    await waitFor(() => expect(within(list).getAllByRole('listitem')).toHaveLength(2));
    const first = within(list).getAllByRole('button')[0]!;
    await user.click(first);
    expect(first).toHaveAttribute('aria-expanded', 'true');
    expect(await within(drawer).findByText('8 of 8')).toBeInTheDocument();
    expect(within(drawer).getByText(/"instance": "https:\/\/pkg\.acme\.io"/)).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Redeliver' }));
    expect(await within(drawer).findByText('Queued as a new delivery.')).toBeInTheDocument();
    await choose(user, within(drawer).getByLabelText('State'), 'pending');
    await waitFor(() => expect(within(within(drawer).getByRole('list', { name: 'Deliveries' })).getAllByText('· redelivery')).toHaveLength(1));
  });

  it('does not offer to redeliver for a disabled webhook, and says why', async () => {
    const user = setupUser();
    renderApp('/settings/webhooks');
    const drawer = await openDeliveries(user, 'security in Teams');
    const list = await within(drawer).findByRole('list', { name: 'Deliveries' });
    await user.click(within(list).getAllByRole('button')[0]!);
    expect(await within(drawer).findByRole('button', { name: 'Redeliver' })).toBeDisabled();
    expect(within(drawer).getByText('Enable the webhook to send events again.')).toBeInTheDocument();
  });

  it('says when a filter hides every delivery', async () => {
    const user = setupUser();
    renderApp('/settings/webhooks');
    const drawer = await openDeliveries(user, 'releases to Slack');
    await within(drawer).findByRole('list', { name: 'Deliveries' });
    await choose(user, within(drawer).getByLabelText('State'), 'delivering');
    expect(await within(drawer).findByText('No deliveries match these filters.')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Clear filters' }));
    expect(await within(drawer).findByRole('list', { name: 'Deliveries' })).toBeInTheDocument();
  });
});
