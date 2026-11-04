import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import type { RepositoryInput } from '../api/types';
import { session } from '../mocks/data';
import { mockState } from '../mocks/handlers';
import { server } from '../mocks/node';
import { renderApp } from './render';

function capturePuts() {
  const puts: RepositoryInput[] = [];
  server.events.on('request:start', async ({ request }) => {
    if (request.method === 'PUT') puts.push((await request.clone().json()) as RepositoryInput);
  });
  return puts;
}

afterEach(() => server.events.removeAllListeners());

const repo = (name: string) => mockState.repositories.find((r) => r.name === name)!;

describe('repository settings', () => {
  it('opens from the repository list', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories');
    await user.click(await screen.findByRole('link', { name: 'npmjs-proxy' }));
    await waitFor(() => expect(history.location.pathname).toBe('/repositories/npmjs-proxy/settings'));
    expect(await screen.findByRole('form', { name: 'Settings of npmjs-proxy' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Browse packages' })).toHaveAttribute('href', '/browse?repo=npmjs-proxy');
  });

  it('edits with the New repository form, pre-filled, and saves only what changed', async () => {
    const puts = capturePuts();
    const user = userEvent.setup();
    renderApp('/repositories/npmjs-proxy/settings');
    const form = await screen.findByRole('form', { name: 'Settings of npmjs-proxy' });
    // Name and blob store are shown, not editable.
    expect(within(form).queryByRole('textbox', { name: 'Name' })).not.toBeInTheDocument();
    expect(within(form).getByText('Cannot be changed after creation')).toBeInTheDocument();
    expect(within(form).getByLabelText('Remote URL')).toHaveValue('https://registry.npmjs.org');
    expect(within(form).getByLabelText('Metadata cached for')).toHaveValue('30m0s');
    const save = screen.getByRole('button', { name: 'Save changes' });
    expect(save).toBeDisabled();

    await user.clear(within(form).getByLabelText('Metadata cached for'));
    await user.type(within(form).getByLabelText('Metadata cached for'), '45m');
    await user.type(within(form).getByLabelText(/Description/), 'Cached npmjs.org');
    expect(save).toBeEnabled();
    await user.click(save);

    expect(await screen.findByRole('button', { name: 'Saved' })).toBeDisabled();
    expect(puts).toHaveLength(1);
    expect(puts[0]).toMatchObject({ name: 'npmjs-proxy', format: 'npm', type: 'proxy', blob_store: 'fs-local', description: 'Cached npmjs.org', online: true });
    expect(puts[0]!.config.proxy).toMatchObject({ remote_url: 'https://registry.npmjs.org', metadata_ttl: '45m', negative_cache_ttl: '10m0s' });
    expect(repo('npmjs-proxy').config.proxy?.metadata_ttl).toBe('45m');
  });

  it('shows the YAML and copyable client configuration for the repository', async () => {
    renderApp('/repositories/npm-internal/settings');
    await screen.findByRole('form', { name: 'Settings of npm-internal' });
    const client = screen.getByRole('button', { name: 'Copy client configuration' }).parentElement!;
    expect(client).toHaveTextContent('npm config set registry https://pkg.acme.io/repository/npm-internal/');
    expect(client).toHaveTextContent('npm publish --registry https://pkg.acme.io/repository/npm-internal/');
    expect(screen.getByRole('button', { name: 'Copy repository configuration as YAML' }).parentElement).toHaveTextContent('name: npm-internal');
  });

  it('takes a repository offline and back online', async () => {
    const puts = capturePuts();
    const user = userEvent.setup();
    renderApp('/repositories/docker-prod/settings');
    await user.click(await screen.findByRole('button', { name: 'Take offline' }));
    expect(await screen.findByRole('button', { name: 'Bring online' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByText('Offline')).toBeInTheDocument();
    expect(repo('docker-prod').online).toBe(false);
    expect(puts[0]).toMatchObject({ online: false, config: { hosted: { write_policy: 'allow' } } });
    await user.click(screen.getByRole('button', { name: 'Bring online' }));
    await waitFor(() => expect(repo('docker-prod').online).toBe(true));
  });

  it('deletes only after the name is typed', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories/npm-sandbox/settings');
    await user.click(await screen.findByRole('button', { name: 'Delete npm-sandbox' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Delete npm-sandbox?' });
    const confirm = within(dialog).getByRole('button', { name: 'Delete repository' });
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByLabelText(/to confirm/), 'npm-sandbo');
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByLabelText(/to confirm/), 'xx');
    expect(confirm).toBeDisabled();
    await user.clear(within(dialog).getByLabelText(/to confirm/));
    await user.type(within(dialog).getByLabelText(/to confirm/), 'npm-sandbox');
    await user.click(confirm);
    await waitFor(() => expect(history.location.pathname).toBe('/repositories'));
    expect(mockState.repositories.some((r) => r.name === 'npm-sandbox')).toBe(false);
  });

  it('explains why a group member cannot be deleted', async () => {
    const user = userEvent.setup();
    renderApp('/repositories/npm-internal/settings');
    await user.click(await screen.findByRole('button', { name: 'Delete npm-internal' }));
    const dialog = screen.getByRole('alertdialog');
    await user.type(within(dialog).getByLabelText(/to confirm/), 'npm-internal');
    await user.click(within(dialog).getByRole('button', { name: 'Delete repository' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('is a member of group(s) npm-all');
    expect(mockState.repositories.some((r) => r.name === 'npm-internal')).toBe(true);
  });

  it('is read-only without admin rights on the repository', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read', 'repo:*:write'] })));
    renderApp('/repositories/docker-prod/settings');
    expect(await screen.findByText(/Changing them needs admin rights/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save changes' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Take offline' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Delete docker-prod' })).toBeDisabled();
    expect(screen.getByLabelText(/Description/)).toBeDisabled();
  });

  it('says when the repository does not exist', async () => {
    renderApp('/repositories/missing-repo/settings');
    expect(await screen.findByText('This repository does not exist, or you cannot see it.')).toBeInTheDocument();
  });
});
