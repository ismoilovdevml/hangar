import { choose, valueOf } from './choose';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { systemStatus } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);

describe('audit log', () => {
  it('lists events newest first and expands details', async () => {
    const user = userEvent.setup();
    renderApp('/audit');
    const table = await screen.findByRole('table', { name: 'Audit events, newest first' });
    await within(table).findAllByText('cleanup.policy.update');
    const first = within(table).getAllByRole('row')[1]!;
    expect(within(first).getByText('d.ismoilov')).toBeInTheDocument();
    expect(within(first).getByText('Success')).toBeInTheDocument();
    expect(within(first).getByText('10.4.2.18')).toBeInTheDocument();

    const toggle = within(first).getByRole('button', { name: /Show details of event 5000/ });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const details = document.getElementById(toggle.getAttribute('aria-controls')!)!;
    expect(within(details).getByText('changed')).toBeInTheDocument();
    expect(within(details).getByText('criteria.older_than_days')).toBeInTheDocument();
    expect(within(details).getByText('90')).toBeInTheDocument();
  });

  it('filters by outcome, actor and action through the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/audit');
    const table = await screen.findByRole('table', { name: 'Audit events, newest first' });
    await within(table).findAllByText('d.ismoilov');

    await choose(user, screen.getByLabelText('Outcome'), 'denied');
    await waitFor(() => expect(search(history).get('outcome')).toBe('denied'));
    await waitFor(() => expect(within(table).queryByText('Success')).not.toBeInTheDocument());
    expect(within(table).getAllByText('Denied').length).toBeGreaterThan(0);

    await user.type(screen.getByLabelText('Actor'), 'unknown');
    await user.type(screen.getByLabelText('Action'), 'auth.*');
    await user.click(screen.getByRole('button', { name: 'Apply filters' }));
    await waitFor(() => expect(search(history).get('actor')).toBe('unknown'));
    expect(search(history).get('action')).toBe('auth.*');
    await waitFor(() => expect(within(table).queryByText('component.delete')).not.toBeInTheDocument());
    expect(within(table).getAllByText('auth.login.failed').length).toBeGreaterThan(0);

    // Exports carry the same filters.
    const csv = new URL(screen.getByRole('link', { name: 'Export CSV' }).getAttribute('href')!, 'http://x');
    expect(csv.pathname).toBe('/api/v1/audit/export');
    expect(Object.fromEntries(csv.searchParams)).toMatchObject({ format: 'csv', actor: 'unknown', action: 'auth.*', outcome: 'denied' });
    const ndjson = new URL(screen.getByRole('link', { name: 'Export NDJSON' }).getAttribute('href')!, 'http://x');
    expect(ndjson.searchParams.get('format')).toBe('json');

    await user.click(screen.getByRole('link', { name: 'Clear' }));
    await waitFor(() => expect(history.location.search).toBe(''));
    expect(screen.getByLabelText('Actor')).toHaveValue('');
  });

  it('restores a custom range from a shared link', async () => {
    renderApp('/audit?range=custom&from=2026-01-01T00:00:00.000Z&to=2026-01-02T00:00:00.000Z');
    expect(await screen.findByText('No events match these filters.', { exact: false })).toBeInTheDocument();
    expect(valueOf(screen.getByLabelText('Time range'))).toBe('custom');
    expect(screen.getByLabelText('From (UTC)')).toHaveValue('2026-01-01T00:00');
  });

  it('pages with the server cursor', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/audit?range=90d');
    const table = await screen.findByRole('table', { name: 'Audit events, newest first' });
    await waitFor(() => expect(within(table).getAllByRole('row').length).toBeGreaterThan(50));
    await user.click(screen.getByRole('button', { name: 'Next' }));
    await waitFor(() => expect(search(history).get('cursor')).toBe('o50'));
    await user.click(screen.getByRole('button', { name: 'Previous' }));
    await waitFor(() => expect(search(history).get('cursor')).toBeNull());
  });

  it('explains a missing privilege', async () => {
    server.use(http.get('*/api/v1/audit', () => HttpResponse.json({ code: 'forbidden', message: 'missing system:audit:read' }, { status: 403 })));
    renderApp('/audit');
    expect(await screen.findByText(/needs the/)).toHaveTextContent('system:audit:read');
    expect(screen.queryByRole('link', { name: 'Export CSV' })).not.toBeInTheDocument();
  });
});

describe('system', () => {
  it('shows status, endpoints, dependencies, nodes and where configuration lives', async () => {
    // A calm installation: the mock nodes include a silent one.
    server.use(http.get('*/api/v1/system/status', () => HttpResponse.json(systemStatus)));
    renderApp('/settings/system');
    expect(await screen.findByText('Operational')).toBeInTheDocument();
    expect(screen.getByText('All systems normal', { selector: '.meta' })).toBeInTheDocument();
    const statusPanel = screen.getByRole('region', { name: 'System status' });
    expect(within(statusPanel).getByText('14 d')).toBeInTheDocument();
    expect(within(statusPanel).getByText('3 of 3 up')).toBeInTheDocument();
    expect(within(statusPanel).getByText('Embedded in the binary')).toBeInTheDocument();
    const endpoints = screen.getByRole('region', { name: 'Endpoints' });
    expect(within(endpoints).getByText('https://pkg.acme.io')).toBeInTheDocument();
    expect(within(endpoints).getByText('cr.acme.io')).toBeInTheDocument();
    expect(within(endpoints).getByText('https://pkg.acme.io/api/v1')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy base url' })).toBeInTheDocument();

    const deps = screen.getByRole('table', { name: 'Dependency health' });
    expect(within(deps).getByText('PostgreSQL')).toBeInTheDocument();
    expect(within(deps).getAllByText('Healthy')).toHaveLength(3);

    const nodes = await screen.findByRole('table', { name: 'Cluster nodes' });
    expect(await within(nodes).findByText('hangar-1')).toBeInTheDocument();
    expect(within(nodes).getByText('leader')).toBeInTheDocument();

    const config = screen.getByRole('region', { name: 'Configuration' });
    expect(within(config).getByRole('link', { name: 'Configuration reference' })).toHaveAttribute('href', expect.stringContaining('docs/deploy'));
  });

  it('copes with a server that does not list nodes, and never asks for a configuration endpoint', async () => {
    const asked: string[] = [];
    server.events.on('request:start', ({ request }) => void asked.push(new URL(request.url).pathname));
    server.use(http.get('*/api/v1/nodes', () => HttpResponse.json({ code: 'not_implemented', message: 'cluster' }, { status: 501 })));
    // A calm installation: the mock nodes include a silent one.
    server.use(http.get('*/api/v1/system/status', () => HttpResponse.json(systemStatus)));
    renderApp('/settings/system');
    expect(await screen.findByText(/does not list cluster nodes yet; it reports 3 of 3 up/)).toBeInTheDocument();
    expect(asked).not.toContain('/api/v1/system/config');
    server.events.removeAllListeners();
  });
});

describe('older servers', () => {
  it('says plainly when cleanup policies are not supported yet', async () => {
    server.use(http.get('*/api/v1/cleanup-policies', () => HttpResponse.json({ code: 'not_implemented', message: 'cleanup' }, { status: 501 })));
    renderApp('/cleanup');
    expect(await screen.findByText(/does not support cleanup policies yet/, {}, { timeout: 5000 })).toBeInTheDocument();
  });

  it('says plainly when tasks are not supported yet', async () => {
    server.use(http.get('*/api/v1/tasks', () => HttpResponse.json({ code: 'not_implemented', message: 'tasks' }, { status: 501 })));
    renderApp('/tasks');
    expect(await screen.findByText(/does not report scheduled tasks yet/, {}, { timeout: 5000 })).toBeInTheDocument();
  });
});
