import { choose, valueOf } from './choose';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import type { RepositoryInput } from '../api/types';
import { mockState } from '../mocks/handlers';
import { server } from '../mocks/node';
import { renderApp } from './render';

function capturePosts() {
  const posts: { body: RepositoryInput; csrf: string | null }[] = [];
  server.events.on('request:start', async ({ request }) => {
    if (request.method === 'POST' && new URL(request.url).pathname === '/api/v1/repositories') {
      posts.push({ body: (await request.clone().json()) as RepositoryInput, csrf: request.headers.get('X-CSRF-Token') });
    }
  });
  return posts;
}

afterEach(() => server.events.removeAllListeners());

const step = (name: RegExp) => screen.getByRole('region', { name });
const yamlText = () => screen.getByRole('button', { name: 'Copy repository configuration as YAML' }).parentElement!.textContent ?? '';

describe('new repository form', () => {
  it('reports every missing choice and focuses the first one', async () => {
    const user = userEvent.setup();
    renderApp('/repositories/new');
    await user.click(await screen.findByRole('button', { name: 'Create repository' }));
    expect(screen.getByRole('alert')).toHaveTextContent('settings need attention');
    expect(screen.getByText('Pick a format.')).toBeInTheDocument();
    expect(screen.getByText('Pick how the repository behaves.')).toBeInTheDocument();
    expect(screen.getByText('Enter a name.')).toBeInTheDocument();
    expect(within(step(/What will it store/)).getByRole('radio', { name: /Docker/ })).toHaveFocus();
  });

  it('marks formats this server does not speak', async () => {
    // A build without the Linux package formats.
    server.use(http.get('*/api/v1/formats', () => HttpResponse.json({ items: [{ name: 'npm' }, { name: 'cargo' }] })));
    renderApp('/repositories/new');
    const formats = await screen.findByRole('region', { name: /What will it store/ });
    await waitFor(() => expect(within(formats).getByRole('radio', { name: /APT/ })).toBeDisabled());
    expect(within(formats).getByRole('radio', { name: /RPM/ })).toBeDisabled();
    expect(within(formats).getByRole('radio', { name: /npm/ })).toBeEnabled();
    expect(within(formats).getByRole('radio', { name: /Cargo/ })).toBeEnabled();
  });

  it('offers APT and RPM with their distribution hints', async () => {
    renderApp('/repositories/new');
    const formats = await screen.findByRole('region', { name: /What will it store/ });
    await waitFor(() => expect(within(formats).getByRole('radio', { name: /APT/ })).toBeEnabled());
    expect(within(formats).getByRole('radio', { name: /APT/ })).toHaveAccessibleName(/Debian, Ubuntu/);
    expect(within(formats).getByRole('radio', { name: /RPM/ })).toHaveAccessibleName(/RHEL, Rocky, Fedora/);
  });

  it('validates the name, the remote URL and the cache durations of a proxy', async () => {
    const user = userEvent.setup();
    renderApp('/repositories/new?format=npm&type=proxy');
    const name = await screen.findByLabelText('Name');
    expect(name).toHaveValue('npm-proxy');
    expect(screen.getByLabelText('Remote URL')).toHaveValue('https://registry.npmjs.org');

    await user.clear(name);
    await user.type(name, 'Bad_Name');
    await user.clear(screen.getByLabelText('Remote URL'));
    await user.type(screen.getByLabelText('Remote URL'), 'registry.npmjs.org');
    await user.clear(screen.getByLabelText('Metadata cached for'));
    await user.type(screen.getByLabelText('Metadata cached for'), '7d');
    await user.click(screen.getByRole('button', { name: 'Create repository' }));

    expect(screen.getByLabelText('Name')).toHaveAccessibleDescription(/Use 2–63 lowercase letters/);
    expect(screen.getByLabelText('Remote URL')).toHaveAccessibleDescription('Use an absolute http:// or https:// address.');
    expect(screen.getByLabelText('Metadata cached for')).toHaveAccessibleDescription(/Use a duration like 30m, 24h or 1h30m/);
    expect(screen.getByLabelText('Name')).toHaveFocus();
  });

  it('requires credentials for upstream auth and a valid CA bundle', async () => {
    const user = userEvent.setup();
    renderApp('/repositories/new?format=docker&type=proxy');
    await user.click(await screen.findByText('Upstream credentials and connection'));
    await choose(user, screen.getByLabelText('Authentication'), 'header');
    await user.type(screen.getByLabelText(/Extra trusted certificates/), 'not a pem');
    await user.click(screen.getByRole('button', { name: 'Create repository' }));

    expect(screen.getByLabelText('Header name')).toHaveAccessibleDescription('Enter a header name such as Private-Token.');
    expect(screen.getByLabelText('Header value')).toHaveAccessibleDescription('Enter the header value.');
    expect(screen.getByLabelText(/Extra trusted certificates/)).toHaveAccessibleDescription(/Paste PEM certificates/);
  });

  it('creates a proxy with auth, outbound proxy and CA bundle, sending the CSRF token', async () => {
    const posts = capturePosts();
    const user = userEvent.setup();
    const { history } = renderApp('/repositories/new');
    await user.click(await screen.findByRole('radio', { name: /npm/ }));
    await user.click(screen.getByRole('radio', { name: /Proxy/ }));
    await user.clear(screen.getByLabelText('Name'));
    await user.type(screen.getByLabelText('Name'), 'npm-mirror');
    await user.click(screen.getByText('Upstream credentials and connection'));
    await choose(user, screen.getByLabelText('Authentication'), 'bearer');
    await user.type(screen.getByLabelText('Token'), 's3cret-token');
    await choose(user, screen.getByLabelText('Outbound proxy'), 'none');
    await user.type(screen.getByLabelText(/Extra trusted certificates/), '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----');
    await choose(user, screen.getByLabelText('Cleanup policy'), 'keep-last-20');

    // The YAML preview mirrors the form but never shows the secret.
    const yaml = yamlText();
    expect(yaml).toContain('name: npm-mirror');
    expect(yaml).toContain('remote_url: https://registry.npmjs.org');
    expect(yaml).toContain('proxy: none');
    expect(yaml).toContain('# token: write-only, not shown');
    expect(yaml).not.toContain('s3cret-token');

    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    await waitFor(() => expect(history.location.pathname).toBe('/repositories'));
    expect(new URLSearchParams(history.location.search).get('q')).toBe('npm-mirror');

    expect(posts).toHaveLength(1);
    expect(posts[0]!.csrf).toBe(mockState.csrf);
    expect(posts[0]!.body).toEqual({
      name: 'npm-mirror',
      format: 'npm',
      type: 'proxy',
      online: true,
      blob_store: 's3-eu-primary',
      config: {
        anonymous_read: false,
        scan_on_arrival: true,
        quarantine_critical: true,
        cleanup_policies: ['keep-last-20'],
        proxy: {
          remote_url: 'https://registry.npmjs.org',
          metadata_ttl: '24h',
          content_ttl: '0s',
          negative_cache_ttl: '10m',
          auth: { kind: 'bearer', token: 's3cret-token' },
          http: { proxy: 'none', ca_bundle: '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n' },
        },
      },
    });
    expect(await screen.findByText('npm-mirror')).toBeInTheDocument();
  });

  it('builds an ordered group with a hosted write member', async () => {
    const posts = capturePosts();
    const user = userEvent.setup();
    renderApp('/repositories/new?format=npm&type=group');
    const add = await screen.findByLabelText('Repository to add');
    await waitFor(() => expect(add).toBeEnabled());
    await choose(user, add, 'npmjs-proxy');
    await user.click(screen.getByRole('button', { name: 'Add member' }));
    await choose(user, add, 'npm-internal');
    await user.click(screen.getByRole('button', { name: 'Add member' }));
    await user.click(screen.getByRole('button', { name: 'Move npm-internal up' }));
    await choose(user, screen.getByLabelText('Publishing through the group'), 'npm-internal');
    await user.clear(screen.getByLabelText('Name'));
    await user.type(screen.getByLabelText('Name'), 'npm-everything');
    expect(screen.queryByLabelText('Blob store')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Create repository' }));

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]!.body.config.group).toEqual({ members: ['npm-internal', 'npmjs-proxy'], write_member: 'npm-internal' });
    expect(posts[0]!.body.blob_store).toBeUndefined();
  });

  it('asks for group members before creating', async () => {
    const user = userEvent.setup();
    renderApp('/repositories/new?format=maven&type=group');
    await user.click(await screen.findByRole('button', { name: 'Create repository' }));
    expect(screen.getByText('Add at least one member repository.')).toBeInTheDocument();
  });

  it('shows a taken name on the name field', async () => {
    const user = userEvent.setup();
    renderApp('/repositories/new?format=npm&type=hosted');
    const name = await screen.findByLabelText('Name');
    await user.clear(name);
    await user.type(name, 'npm-internal');
    await waitFor(() => expect(valueOf(screen.getByLabelText('Blob store'))).toBe('s3-eu-primary'));
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('A repository with this name already exists. Choose another name.');
    expect(screen.getByLabelText('Name')).toHaveAccessibleDescription(/A repository with this name already exists\./);
  });
});
