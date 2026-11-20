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

  it('adds a scheduled Docker garbage collection for one repository', async () => {
    let sent: TaskInput | undefined;
    server.events.on('request:start', async ({ request }) => {
      if (request.method === 'POST' && new URL(request.url).pathname === `${API}/tasks`) sent = (await request.clone().json()) as TaskInput;
    });
    const user = userEvent.setup();
    const { history } = renderApp('/tasks/new');
    const form = await screen.findByRole('form', { name: 'New task' });
    await user.click(within(form).getByRole('radio', { name: 'Maintenance' }));
    // Maintenance does not start on its own.
    expect(within(form).getByRole('checkbox', { name: 'Run now' })).not.toBeChecked();
    await user.type(within(form).getByLabelText('Task name'), 'docker-gc-prod');
    await choose(user, within(form).getByLabelText('Maintenance'), 'docker_gc');
    await choose(user, within(form).getByLabelText('Docker repository'), 'docker-prod');
    await user.type(within(form).getByLabelText(/^Unreferenced for/), '14');
    await choose(user, within(form).getByLabelText('How often'), 'weekly');
    await user.click(within(form).getByRole('button', { name: 'Create task' }));

    await waitFor(() => expect(history.location.pathname).toBe('/tasks/t_docker_gc_prod'));
    expect(sent).toMatchObject({
      name: 'docker-gc-prod',
      type: 'docker_gc',
      schedule: '0 3 * * 0',
      config: { older_than_days: 14, repositories: ['docker-prod'] },
    });
    server.events.removeAllListeners();
  });

  it('schedules an import from More options', async () => {
    let sent: TaskInput | undefined;
    server.events.on('request:start', async ({ request }) => {
      if (request.method === 'POST' && new URL(request.url).pathname === `${API}/tasks`) sent = (await request.clone().json()) as TaskInput;
    });
    const user = userEvent.setup();
    const { history } = renderApp('/tasks/new?kind=import&repository=npm-internal');
    const form = await screen.findByRole('form', { name: 'New task' });
    await user.type(within(form).getByLabelText('Task name'), 'import-nightly');
    await user.type(within(form).getByLabelText('Source URL'), 'https://old.example.com/repository/npm/');
    await user.click(within(form).getByText('More options'));
    await choose(user, within(form).getByLabelText('How often'), 'daily');
    await user.click(within(form).getByRole('button', { name: /Create and run/ }));
    await waitFor(() => expect(history.location.pathname).toBe('/tasks/t_import_nightly'));
    expect(sent).toMatchObject({ type: 'repository_import', schedule: '0 3 * * *' });
    server.events.removeAllListeners();
  });

  it('shows every problem at its field and opens More options when one is inside it', async () => {
    const user = userEvent.setup();
    renderApp('/tasks/new?kind=import&repository=npm-internal');
    const form = await screen.findByRole('form', { name: 'New task' });
    await user.type(within(form).getByLabelText('Task name'), 'import-nightly');
    await user.type(within(form).getByLabelText('Source URL'), 'https://old.example.com/repository/npm/');
    await user.type(within(form).getByLabelText(/^Newest/), '-2');
    // Set inside More options, which is then closed again.
    const more = within(form).getByText('More options').closest('details')!;
    await user.click(within(form).getByText('More options'));
    await user.type(within(form).getByLabelText('Requests per second'), '-5');
    await user.click(within(form).getByText('More options'));
    expect(more).not.toHaveAttribute('open');

    await user.click(within(form).getByRole('button', { name: /Create and run/ }));
    expect(within(form).getByRole('alert')).toHaveTextContent('2 fields need attention');
    // The closed section opens to show its problem, and each message is tied to its field.
    expect(more).toHaveAttribute('open');
    const rps = within(form).getByLabelText('Requests per second');
    expect(rps).toHaveAttribute('aria-invalid', 'true');
    expect(rps).toHaveAccessibleDescription('A positive number');
    const newest = within(form).getByLabelText(/^Newest/);
    expect(newest).toHaveAccessibleDescription('A positive number');
    // The focus goes to the first field with a problem.
    await waitFor(() => expect(newest).toHaveFocus());
  });

  it('does not start maintenance on its own when opened by link', async () => {
    renderApp('/tasks/new?kind=maintenance');
    const form = await screen.findByRole('form', { name: 'New task' });
    expect(within(form).getByRole('checkbox', { name: 'Run now' })).not.toBeChecked();
    expect(within(form).getByRole('button', { name: 'Create task' })).toBeInTheDocument();
  });
});
