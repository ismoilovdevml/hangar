import { choose, valueOf } from './choose';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import type { BlobStoreInput } from '../api/types';
import { catalogState } from '../mocks/catalog';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

afterEach(() => server.events.removeAllListeners());

describe('blob stores', () => {
  it('shows totals and one card per store with usage, quota and state', async () => {
    renderApp('/blob-stores');
    await screen.findByRole('article', { name: 's3-eu-primary' });
    const totals = screen.getByRole('region', { name: 'Storage totals' });
    expect(within(totals).getByRole('article', { name: 'Saved by deduplication' })).toHaveTextContent('1.35TiB');
    expect(within(totals).getByRole('article', { name: 'Growth' })).toHaveTextContent('+38GiB / day7-day average');
    expect(within(totals).getByRole('article', { name: 'Reclaimable now' })).toHaveTextContent('Freed by the next blob compaction');

    const stores = screen.getByRole('region', { name: 'Blob stores' });
    const primary = within(stores).getByRole('article', { name: 's3-eu-primary' });
    expect(within(primary).getByText('Healthy')).toBeInTheDocument();
    expect(within(primary).getByRole('meter', { name: 's3-eu-primary usage' })).toHaveAttribute('aria-valuetext', '4.77 TiB of 12 TiB');
    expect(within(primary).getByText('SSE-S3')).toBeInTheDocument();
    const local = within(stores).getByRole('article', { name: 'fs-local' });
    expect(within(local).getByText('Not checked yet')).toBeInTheDocument();
    expect(within(local).getByText('No soft quota set')).toBeInTheDocument();
    expect(within(stores).getByRole('article', { name: 's3-archive' })).toHaveTextContent('Read-only');
  });

  it('validates and creates an S3 store, sending the secret once', async () => {
    const posts: BlobStoreInput[] = [];
    server.events.on('request:start', async ({ request }) => {
      if (request.method === 'POST' && new URL(request.url).pathname === '/api/v1/blob-stores') posts.push((await request.clone().json()) as BlobStoreInput);
    });
    const user = userEvent.setup();
    const { history } = renderApp('/blob-stores');
    await user.click(await screen.findByRole('button', { name: 'Add blob store' }));
    expect(history.location.search).toContain('add=true');
    const form = screen.getByRole('form', { name: 'Add a blob store' });
    await user.click(within(form).getByRole('radio', { name: 'S3 / MinIO' }));
    await user.type(within(form).getByLabelText('Name'), 'minio-onprem');
    await user.type(within(form).getByLabelText('Access key id'), 'AKIA1');
    await user.click(within(form).getByRole('button', { name: 'Add blob store' }));
    expect(within(form).getByText('Enter the bucket name.')).toBeInTheDocument();
    expect(within(form).getByText('Enter the secret for this access key.')).toBeInTheDocument();
    expect(within(form).getByLabelText('Bucket')).toHaveFocus();

    await user.type(within(form).getByLabelText('Bucket'), 'artifacts');
    await user.type(within(form).getByLabelText('Endpoint'), 'https://minio.internal:9000');
    await user.type(within(form).getByLabelText('Secret access key'), 'top-secret');
    expect(within(form).getByLabelText('Secret access key')).toHaveAttribute('type', 'password');
    await user.type(within(form).getByLabelText('Soft quota'), '3');
    await user.click(within(form).getByLabelText('Path-style addressing (MinIO and most S3-compatible servers)'));
    await user.click(within(form).getByRole('button', { name: 'Add blob store' }));

    expect(await screen.findByRole('heading', { level: 1, name: /minio-onprem/ })).toBeInTheDocument();
    expect(posts).toEqual([
      {
        name: 'minio-onprem',
        kind: 's3',
        soft_quota_bytes: 3e12,
        s3: { bucket: 'artifacts', endpoint: 'https://minio.internal:9000', use_path_style: true, access_key_id: 'AKIA1', secret_access_key: 'top-secret' },
      },
    ]);
    // The detail page never shows the secret.
    expect(await screen.findByText('Stored encrypted, write-only')).toBeInTheDocument();
    expect(screen.queryByText('top-secret')).not.toBeInTheDocument();
    expect(catalogState.blobStores.find((b) => b.name === 'minio-onprem')?.s3).not.toHaveProperty('secret_access_key', 'top-secret');
  });

  it('reports a name that is already taken', async () => {
    const user = userEvent.setup();
    renderApp('/blob-stores?add=true');
    const form = await screen.findByRole('form', { name: 'Add a blob store' });
    await user.type(within(form).getByLabelText('Name'), 'fs-local');
    await user.type(within(form).getByLabelText('Directory on the server'), '/data/other');
    await user.click(within(form).getByRole('button', { name: 'Add blob store' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent('A blob store with this name already exists.');
  });

  it('hides creation from users without the blob store admin privilege', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read', 'system:blobstores:read'] })));
    renderApp('/blob-stores?add=true');
    expect(await screen.findByRole('heading', { level: 1, name: 'Blob stores' })).toBeInTheDocument();
    expect(await screen.findByRole('region', { name: 'Storage totals' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add blob store' })).not.toBeInTheDocument();
    expect(screen.queryByRole('form', { name: 'Add a blob store' })).not.toBeInTheDocument();
  });

  it('shows a store with the repositories that keep files in it', async () => {
    renderApp('/blob-stores/fs-local');
    expect(await screen.findByRole('heading', { level: 1, name: /fs-local/ })).toBeInTheDocument();
    expect(await screen.findByText('/var/lib/hangar/blobs', { selector: 'dd' })).toBeInTheDocument();
    const table = await screen.findByRole('table', { name: 'Repositories using fs-local' });
    await waitFor(() => expect(within(table).getByRole('link', { name: 'maven-snapshots' })).toHaveAttribute('href', '/repositories/maven-snapshots/settings'));
    expect(within(table).queryByRole('link', { name: 'docker-prod' })).not.toBeInTheDocument();
  });

  it('changes the soft quota and refuses to delete a store in use', async () => {
    const user = userEvent.setup();
    renderApp('/blob-stores/fs-local');
    const manage = await screen.findByRole('region', { name: 'Manage' });
    const quota = within(manage).getByLabelText('Soft quota');
    expect(quota).toHaveValue('');
    await user.type(quota, '500');
    await choose(user, within(manage).getByLabelText('Quota unit'), 'GB');
    await user.click(within(manage).getByRole('button', { name: 'Save quota' }));
    expect(await within(manage).findByRole('button', { name: 'Saved' })).toBeDisabled();
    expect(catalogState.blobStores.find((b) => b.name === 'fs-local')?.soft_quota_bytes).toBe(500e9);
    expect(within(manage).getByRole('button', { name: 'Delete fs-local' })).toBeDisabled();
    expect(within(manage).getByText(/Deleting needs an empty store/)).toBeInTheDocument();
  });

  it('shows a binary quota exactly and does not save it untouched', async () => {
    const user = userEvent.setup();
    catalogState.blobStores.find((b) => b.name === 'fs-local')!.soft_quota_bytes = 2 ** 31;
    renderApp('/blob-stores/fs-local');
    const manage = await screen.findByRole('region', { name: 'Manage' });
    expect(within(manage).getByLabelText('Soft quota')).toHaveValue('2');
    expect(valueOf(within(manage).getByLabelText('Quota unit'))).toBe('GiB');
    expect(within(manage).getByRole('button', { name: 'Save quota' })).toBeDisabled();
    // Typing the same value back is still no change.
    await user.clear(within(manage).getByLabelText('Soft quota'));
    await user.type(within(manage).getByLabelText('Soft quota'), '2');
    expect(within(manage).getByRole('button', { name: 'Save quota' })).toBeDisabled();
    await user.type(within(manage).getByLabelText('Soft quota'), '.5');
    await user.click(within(manage).getByRole('button', { name: 'Save quota' }));
    expect(await within(manage).findByRole('button', { name: 'Saved' })).toBeDisabled();
    expect(catalogState.blobStores.find((b) => b.name === 'fs-local')?.soft_quota_bytes).toBe(2.5 * 2 ** 30);
  });

  it('deletes an empty store after its name is typed', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/blob-stores?add=true');
    const form = await screen.findByRole('form', { name: 'Add a blob store' });
    await user.type(within(form).getByLabelText('Name'), 'scratch');
    await user.type(within(form).getByLabelText('Directory on the server'), '/srv/scratch');
    await user.click(within(form).getByRole('button', { name: 'Add blob store' }));
    const manage = await screen.findByRole('region', { name: 'Manage' });
    await user.click(within(manage).getByRole('button', { name: 'Delete scratch' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Delete scratch?' });
    expect(within(dialog).getByLabelText(/to confirm/)).toHaveFocus();
    expect(within(dialog).getByRole('button', { name: 'Delete blob store' })).toBeDisabled();
    await user.keyboard('scratch');
    await user.click(within(dialog).getByRole('button', { name: 'Delete blob store' }));
    await waitFor(() => expect(history.location.pathname).toBe('/blob-stores'));
    expect(catalogState.blobStores.some((b) => b.name === 'scratch')).toBe(false);
  });

  it('says when a store does not exist', async () => {
    renderApp('/blob-stores/nope');
    expect(await screen.findByRole('heading', { level: 1, name: 'Blob store not found' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to blob stores' })).toHaveAttribute('href', '/blob-stores');
  });

  it('tests S3 settings before adding the store and says why they fail', async () => {
    const posts: string[] = [];
    server.events.on('request:start', ({ request }) => {
      if (request.method === 'POST') posts.push(new URL(request.url).pathname);
    });
    const user = userEvent.setup();
    renderApp('/blob-stores?add=true');
    const form = await screen.findByRole('form', { name: 'Add a blob store' });
    await user.click(within(form).getByRole('radio', { name: 'S3 / MinIO' }));
    await user.type(within(form).getByLabelText('Name'), 'ux-s3bad');
    await user.type(within(form).getByLabelText('Bucket'), 'no-such-bucket');
    await user.type(within(form).getByLabelText('Endpoint'), 'http://minio:9000');
    await user.click(within(form).getByRole('button', { name: 'Add blob store' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent(
      'Connection test failed (bucket): bucket "no-such-bucket" not found at http://minio:9000 (NoSuchBucket)',
    );
    // Not saved: the next click says it saves anyway.
    expect(posts).toEqual(['/api/v1/blob-stores:test']);
    expect(within(form).getByRole('button', { name: 'Add anyway' })).toBeInTheDocument();

    await user.clear(within(form).getByLabelText('Bucket'));
    await user.type(within(form).getByLabelText('Bucket'), 'artifacts');
    await user.click(within(form).getByRole('button', { name: 'Test connection' }));
    expect(await within(form).findByRole('status')).toHaveTextContent('Connected: bucket, write, read back, delete work');
    expect(within(form).getByRole('button', { name: 'Add blob store' })).toBeInTheDocument();
  });

  it('marks a store from the server configuration and offers no connection editing', async () => {
    renderApp('/blob-stores/s3-eu-primary');
    const config = await screen.findByRole('region', { name: 'Configuration' });
    expect(within(config).getByText('From server configuration')).toBeInTheDocument();
    const manage = screen.getByRole('region', { name: 'Manage' });
    expect(within(manage).queryByRole('button', { name: 'Edit connection' })).not.toBeInTheDocument();
    expect(within(manage).getByRole('button', { name: 'Delete s3-eu-primary' })).toBeDisabled();
  });

  it('edits the connection of an S3 store in use: credentials and addressing, not the location', async () => {
    const puts: BlobStoreInput[] = [];
    server.events.on('request:start', async ({ request }) => {
      if (request.method === 'PUT') puts.push((await request.clone().json()) as BlobStoreInput);
    });
    const user = userEvent.setup();
    renderApp('/blob-stores/s3-archive');
    const manage = await screen.findByRole('region', { name: 'Manage' });
    await user.click(within(manage).getByRole('button', { name: 'Edit connection' }));
    const form = within(manage).getByRole('form', { name: 'Connection of s3-archive' });
    expect(within(form).getByLabelText('Bucket')).toBeDisabled();
    expect(within(form).getByLabelText('Endpoint')).toBeDisabled();

    await user.clear(within(form).getByLabelText('Access key id'));
    await user.type(within(form).getByLabelText('Access key id'), 'ARCHIVE02');
    expect(within(form).getByText('A new access key needs its secret.')).toBeInTheDocument();
    expect(within(form).getByRole('button', { name: 'Save connection' })).toBeDisabled();
    await user.type(within(form).getByLabelText('Secret access key'), 'new-secret');
    await user.click(within(form).getByLabelText('Path-style addressing (MinIO and most S3-compatible servers)'));
    await user.click(within(form).getByRole('button', { name: 'Save connection' }));

    await waitFor(() => expect(within(manage).getByRole('button', { name: 'Edit connection' })).toBeInTheDocument());
    expect(puts.at(-1)).toEqual({
      name: 's3-archive',
      kind: 's3',
      soft_quota_bytes: 40 * 1024 ** 4,
      s3: { bucket: 'acme-archive', endpoint: 'https://minio.acme.example:9000', prefix: 'hangar', use_path_style: true, access_key_id: 'ARCHIVE02', secret_access_key: 'new-secret' },
    });
    const config = screen.getByRole('region', { name: 'Configuration' });
    expect(within(config).getByText('Path-style')).toBeInTheDocument();
  });

  it('runs a blob compaction from the reclaimable total, after confirming', async () => {
    const runs: string[] = [];
    server.events.on('request:start', ({ request }) => {
      if (request.method === 'POST' && request.url.includes('/tasks/')) runs.push(new URL(request.url).pathname);
    });
    const user = userEvent.setup();
    renderApp('/blob-stores');
    await screen.findByRole('article', { name: 's3-eu-primary' });
    const card = screen.getByRole('article', { name: 'Reclaimable now' });
    await user.click(await within(card).findByRole('button', { name: 'Run compaction' }));
    await user.click(within(screen.getByRole('alertdialog', { name: 'Run blob compaction now?' })).getByRole('button', { name: 'Run compaction' }));
    expect(await within(card).findByRole('link', { name: 'Compaction running' })).toBeInTheDocument();
    expect(runs).toEqual(['/api/v1/tasks/t_compact/run']);
    expect(screen.queryByRole('button', { name: /Move repositories/ })).not.toBeInTheDocument();
  });
});
