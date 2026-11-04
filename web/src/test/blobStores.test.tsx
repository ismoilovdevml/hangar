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
    expect(within(totals).getByRole('article', { name: 'Saved by deduplication' })).toHaveTextContent('3.51TB');
    expect(within(totals).getByRole('article', { name: 'Growth' })).toHaveTextContent('+38GB / day');
    expect(within(totals).getByRole('article', { name: 'Reclaimable now' })).toHaveTextContent('Freed by the next blob compaction');

    const stores = screen.getByRole('region', { name: 'Blob stores' });
    const primary = within(stores).getByRole('article', { name: 's3-eu-primary' });
    expect(within(primary).getByText('Healthy')).toBeInTheDocument();
    expect(within(primary).getByRole('meter', { name: 's3-eu-primary usage' })).toHaveAttribute('aria-valuetext', '4.62 TB of 12 TB');
    expect(within(primary).getByText('SSE-S3')).toBeInTheDocument();
    const local = within(stores).getByRole('article', { name: 'fs-local' });
    expect(within(local).getByText('Not monitored')).toBeInTheDocument();
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
    expect(screen.getByText('Stored encrypted, write-only')).toBeInTheDocument();
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
    expect(await screen.findByRole('button', { name: 'Add blob store' })).toBeDisabled();
    expect(screen.queryByRole('form', { name: 'Add a blob store' })).not.toBeInTheDocument();
  });

  it('shows a store with the repositories that keep files in it', async () => {
    renderApp('/blob-stores/fs-local');
    expect(await screen.findByRole('heading', { level: 1, name: /fs-local/ })).toBeInTheDocument();
    expect(await screen.findByText('/var/lib/hangar/blobs', { selector: 'dd' })).toBeInTheDocument();
    const table = await screen.findByRole('table', { name: 'Repositories using fs-local' });
    await waitFor(() => expect(within(table).getByRole('link', { name: 'maven-releases' })).toHaveAttribute('href', '/repositories/maven-releases/settings'));
    expect(within(table).queryByRole('link', { name: 'docker-prod' })).not.toBeInTheDocument();
  });

  it('says when a store does not exist', async () => {
    renderApp('/blob-stores/nope');
    expect(await screen.findByText('This blob store does not exist.')).toBeInTheDocument();
  });
});
