import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { opsMockState } from '../mocks/ops';
import { server } from '../mocks/node';
import { renderApp } from './render';

const card = (name: RegExp) => screen.getByRole('link', { name });

describe('overview', () => {
  afterEach(() => window.localStorage.clear());
  it('summarises the day from the metrics API', async () => {
    renderApp('/');
    const today = await screen.findByRole('region', { name: /^Today · / });
    expect(await within(today).findByText(/packages cached/)).toHaveTextContent(
      '18.4M requests, 1,120 packages cached, 186 published, 4 pushes rejected.',
    );
    expect(within(today).getByRole('img', { name: /^Requests per hour, busiest around \d\d:\d\d with / })).toBeInTheDocument();
    expect(within(today).getByRole('table', { name: 'Requests per hour' })).toBeInTheDocument();
    expect(within(today).getByRole('radio', { name: 'Today' })).toHaveAttribute('aria-checked', 'true');

    const outcomes = screen.getByRole('region', { name: 'Request outcomes' });
    expect(within(outcomes).getByRole('img', { name: '98.8 percent of requests succeeded' })).toBeInTheDocument();
    expect(within(outcomes).getByText(/^p95/)).toHaveTextContent('p95 41 ms · 173K denied · 38.6K server errors');

    const storage = card(/^Storage/);
    expect(storage).toHaveAttribute('href', '/blob-stores');
    expect(within(storage).getByText('4.82 TB')).toBeInTheDocument();
    expect(within(storage).getByText('Docker 60% · 7 formats · 40% of quota')).toBeInTheDocument();

    const security = card(/^Security/);
    expect(security).toHaveAttribute('href', '/vulnerabilities');
    expect(within(security).getByText('7 critical')).toBeInTheDocument();
    expect(within(security).getByText('In 3 packages · 2 quarantined')).toBeInTheDocument();

    // The worst upstream comes first.
    const upstream = await waitFor(() => card(/^Upstream/));
    expect(upstream).toHaveAttribute('href', '/repositories?type=proxy');
    expect(within(upstream).getByText('ghcr.io')).toBeInTheDocument();
    expect(within(upstream).getByText('authentication failed (401) · cache hit 94% · 2 of 5 healthy')).toBeInTheDocument();
  });

  it('lists what needs attention, most urgent first, linking to where it is fixed', async () => {
    renderApp('/');
    const todo = await screen.findByRole('region', { name: 'To do' });
    const items = await within(todo).findAllByRole('link');
    expect(items.map((a) => a.querySelector('.todo__item-title')?.textContent)).toEqual([
      'ghcr-proxy: authentication failed (401)',
      '4 pushes rejected',
      'Review 7 critical vulnerabilities',
      'Renew the laptop CLI token',
    ]);
    expect(items.map((a) => a.getAttribute('href'))).toEqual([
      '/repositories/ghcr-proxy/settings',
      '/audit?action=component.publish',
      '/vulnerabilities',
      '/account#my-tokens-title',
    ]);
    expect(todo.querySelector('.todo__count')).toHaveTextContent('4 items');
    // The backup task is scheduled and the blob stores have quotas: no reminders for those.
    expect(todo).not.toHaveTextContent(/backup|quota/i);
  });

  it('points at the backup guide when the server has no backup task, until dismissed', async () => {
    server.use(http.get('*/api/v1/tasks', () => HttpResponse.json({ items: opsMockState.tasks.filter((t) => t.type !== 'db_backup'), running: [] })));
    const user = userEvent.setup();
    const first = renderApp('/');
    const todo = await screen.findByRole('region', { name: 'To do' });
    const reminder = (await within(todo).findByText('Back up the database')).closest('a')!;
    expect(reminder).toHaveAttribute('href', expect.stringContaining('docs/deploy/backup-restore.md'));
    expect(reminder).toHaveAttribute('target', '_blank');
    expect(within(todo).queryByText('Schedule a database backup')).not.toBeInTheDocument();

    await user.click(within(todo).getByRole('button', { name: 'Dismiss: Back up the database' }));
    expect(within(todo).queryByText('Back up the database')).not.toBeInTheDocument();

    // Remembered in this browser.
    first.unmount();
    renderApp('/');
    const again = await screen.findByRole('region', { name: 'To do' });
    await within(again).findByText('ghcr-proxy is blocked');
    expect(within(again).queryByText('Back up the database')).not.toBeInTheDocument();
  });

  it('shows the most downloaded packages and fits the overview on one screen', async () => {
    renderApp('/');
    const top = await screen.findByRole('region', { name: 'Most downloaded' });
    const chips = await within(top).findAllByRole('link');
    expect(chips[0]).toHaveTextContent('log4j-core84K downloads');
    expect(chips.length).toBeLessThanOrEqual(6);

    // Recent publications are on the Packages page; the overview stays one screen.
    expect(screen.queryByRole('table', { name: 'Recently published packages' })).not.toBeInTheDocument();
  });

  it('searches from the box and offers qualifier examples', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/');
    const hero = await screen.findByRole('region', { name: 'What do you need today?' });
    expect(await within(hero).findByRole('link', { name: 'format:docker' })).toHaveAttribute('href', '/search?q=format%3Adocker');
    await user.type(within(hero).getByRole('searchbox', { name: 'Search packages' }), 'serilog{Enter}');
    await waitFor(() => expect(history.location.pathname).toBe('/search'));
    expect(history.location.search).toBe('?q=serilog');
  });

  it('offers quick actions that lead somewhere real', async () => {
    const user = userEvent.setup();
    renderApp('/');
    const actions = await screen.findByRole('list', { name: 'Quick actions' });
    expect(within(actions).getByRole('link', { name: /Create a token/ })).toHaveAttribute('href', '/account#my-tokens-title');
    expect(within(actions).getByRole('link', { name: /New repository/ })).toHaveAttribute('href', '/repositories/new');
    expect(within(actions).getByRole('link', { name: /Browse packages/ })).toHaveAttribute('href', '/browse');

    await user.click(within(actions).getByRole('button', { name: /Connect a client/ }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a client' });
    expect(await within(dialog).findByText("echo '<token>' | docker login cr.acme.io -u d.ismoilov --password-stdin")).toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: 'npm' }));
    expect(within(dialog).getByText('registry=https://pkg.acme.io/repository/npm-group/')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(within(actions).getByRole('button', { name: /Connect a client/ })).toHaveFocus();
  });

  it('keeps the selected range in the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/');
    const group = await screen.findByRole('radiogroup', { name: 'Time range' });
    await user.click(within(group).getByRole('radio', { name: '7d' }));
    await waitFor(() => expect(history.location.search).toContain('range=7d'));
    const range = await screen.findByRole('region', { name: 'Last 7 days' });
    expect(await within(range).findByText(/packages cached/)).toHaveTextContent(
      '121M requests, 7,400 packages cached, 1,310 published, 9 pushes rejected.',
    );
    expect(within(range).getByRole('img', { name: /^Requests per 6 hours/ })).toBeInTheDocument();
    // The to-do list counts rejected pushes over the same range.
    expect(await within(screen.getByRole('region', { name: 'To do' })).findByText('9 pushes rejected')).toBeInTheDocument();
  });
});
