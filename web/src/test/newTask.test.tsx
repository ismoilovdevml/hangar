import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { TaskInput } from '../api/opsTypes';
import { server } from '../mocks/node';
import { choose } from './choose';
import { renderApp } from './render';

const API = '/api/v1';

describe('new import and pre-warm tasks', () => {
  it('creates an import task with credentials and filters, runs it and opens it', async () => {
    let sent: TaskInput | undefined;
    server.events.on('request:start', async ({ request }) => {
      if (request.method === 'POST' && new URL(request.url).pathname === `${API}/tasks`) sent = (await request.clone().json()) as TaskInput;
    });
    const user = userEvent.setup();
    const { history } = renderApp('/tasks/new?kind=import&repository=npm-internal');
    const form = await screen.findByRole('form', { name: 'New task' });
    await user.click(within(form).getByRole('button', { name: /Create and run/ }));
    expect(await within(form).findByText(/Lower-case letters/)).toBeInTheDocument();
    expect(within(form).getByText('An http(s) URL of the source registry')).toBeInTheDocument();

    await user.type(within(form).getByLabelText('Task name'), 'import-npm');
    await user.type(within(form).getByLabelText('Source URL'), 'https://old.example.com/repository/npm/');
    await user.type(within(form).getByLabelText(/^Username/), 'mover');
    await user.type(within(form).getByLabelText(/^Password/), 's3cret');
    await user.type(within(form).getByLabelText(/^Packages/), '@acme/*{enter}left-pad');
    await user.type(within(form).getByLabelText(/^Newest/), '3');
    await user.click(within(form).getByRole('button', { name: /Create and run/ }));

    await waitFor(() => expect(history.location.pathname).toBe('/tasks/t_import_npm'));
    expect(sent).toMatchObject({
      name: 'import-npm',
      type: 'repository_import',
      config: {
        repository: 'npm-internal',
        source: { url: 'https://old.example.com/repository/npm/', username: 'mover', password: 's3cret' },
        packages: ['@acme/*', 'left-pad'],
        latest: 3,
      },
    });
    server.events.removeAllListeners();
  });

  it('pre-warms a proxy from an uploaded lockfile', async () => {
    let sent: TaskInput | undefined;
    server.use(
      http.post(`${API}/lockfiles`, ({ request }) => {
        const u = new URL(request.url);
        expect(u.searchParams.get('repository')).toBe('npmjs-proxy');
        expect(u.searchParams.get('filename')).toBe('package-lock.json');
        return HttpResponse.json({ kind: 'package-lock.json', coordinates: ['left-pad@1.2.0', 'lodash@4.17.21'], skipped: 1 });
      }),
      http.post(`${API}/tasks`, async ({ request }) => {
        sent = (await request.json()) as TaskInput;
        return HttpResponse.json({ id: 't_warm', name: sent.name, type: sent.type, config: sent.config }, { status: 201 });
      }),
    );
    const user = userEvent.setup();
    const { history } = renderApp('/tasks/new?kind=prewarm');
    const form = await screen.findByRole('form', { name: 'New task' });
    await choose(user, within(form).getByLabelText('Proxy or group repository'), 'npmjs-proxy');
    const file = new File(['{"lockfileVersion":3}'], 'package-lock.json', { type: 'application/json' });
    await user.upload(within(form).getByLabelText(/Add from lockfile/), file);
    expect(await within(form).findByText('package-lock.json · 2 added, 1 skipped')).toBeInTheDocument();
    expect(within(form).getByLabelText(/^Coordinates/)).toHaveValue('left-pad@1.2.0\nlodash@4.17.21');

    await user.type(within(form).getByLabelText('Task name'), 'warm-npm');
    await user.click(within(form).getByLabelText('Run now'));
    await user.click(within(form).getByRole('button', { name: 'Create task' }));
    await waitFor(() => expect(history.location.pathname).toBe('/tasks/t_warm'));
    expect(sent).toMatchObject({ type: 'proxy_prewarm', config: { repository: 'npmjs-proxy', coordinates: ['left-pad@1.2.0', 'lodash@4.17.21'] } });
  });

  it('offers New task on the tasks page and Import on a hosted repository', async () => {
    const first = renderApp('/tasks');
    expect(await screen.findByRole('link', { name: 'New task' })).toHaveAttribute('href', '/tasks/new');
    first.unmount();
    const second = renderApp('/repositories/npm-internal/settings');
    expect(await screen.findByRole('link', { name: 'Import' })).toHaveAttribute('href', '/tasks/new?kind=import&repository=npm-internal');
    second.unmount();
    renderApp('/repositories/npmjs-proxy/settings');
    expect(await screen.findByRole('link', { name: 'Pre-warm' })).toHaveAttribute('href', '/tasks/new?kind=prewarm&repository=npmjs-proxy');
  });
});
