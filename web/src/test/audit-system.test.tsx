import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '../mocks/node';
import { opsMockState } from '../mocks/ops';
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

    await user.selectOptions(screen.getByLabelText('Outcome'), 'denied');
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
    expect(screen.getByLabelText('Time range')).toHaveValue('custom');
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
  it('shows status, endpoints, dependencies, nodes and redacted configuration', async () => {
    renderApp('/settings');
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

    const config = await screen.findByRole('region', { name: 'database settings' });
    expect(within(config).getByText('postgres://hangar:••••••@db.acme.internal:5432/hangar')).toBeInTheDocument();
    const storage = screen.getByRole('region', { name: 'storage settings' });
    expect(within(storage).getAllByText('secret').length).toBe(2);
    expect(document.body.textContent).not.toContain('REDACTED');
  });

  it('filters configuration keys', async () => {
    const user = userEvent.setup();
    renderApp('/settings');
    await screen.findByRole('region', { name: 'server settings' });
    await user.type(screen.getByLabelText('Filter settings'), 'base_url');
    await waitFor(() => expect(screen.queryByRole('region', { name: 'database settings' })).not.toBeInTheDocument());
    expect(screen.getByText('server.base_url')).toBeInTheDocument();
  });

  it('copes with a server that does not expose configuration or nodes', async () => {
    opsMockState.configAvailable = false;
    server.use(http.get('*/api/v1/nodes', () => HttpResponse.json({ code: 'not_implemented', message: 'cluster' }, { status: 501 })));
    renderApp('/settings');
    expect(await screen.findByText(/does not expose its effective configuration/)).toBeInTheDocument();
    expect(await screen.findByText(/does not list cluster nodes yet; it reports 3 of 3 up/)).toBeInTheDocument();
  });
});
