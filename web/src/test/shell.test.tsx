import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '../mocks/node';
import { renderApp } from './render';

describe('app shell', () => {
  it('renders sidebar, top bar and system status', async () => {
    renderApp('/');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    for (const label of ['Overview', 'Packages', 'Search', 'Repositories', 'Blob stores', 'Cleanup policies', 'Vulnerabilities', 'Access & audit', 'Scheduled tasks', 'Cluster & replication']) {
      expect(within(nav).getByRole('link', { name: label })).toBeInTheDocument();
    }
    expect(await within(nav).findByText('All systems normal')).toBeInTheDocument();
    expect(within(nav).getByText('3 of 3 nodes · v1.0.0')).toBeInTheDocument();
    const crumbs = screen.getByRole('navigation', { name: 'Breadcrumb' });
    expect(await within(crumbs).findByText('acme-platform')).toBeInTheDocument();
    expect(within(crumbs).getByText('Overview')).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('button', { name: 'Notifications' })).toBeInTheDocument();
  });

  it('marks only the current section as active and follows navigation', async () => {
    const user = userEvent.setup();
    renderApp('/');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    const overview = within(nav).getByRole('link', { name: 'Overview' });
    await waitFor(() => expect(overview).toHaveAttribute('aria-current', 'page'));
    const navLinks = within(nav).getAllByRole('listitem').map((li) => within(li).getByRole('link'));
    expect(navLinks.filter((a) => a.getAttribute('aria-current') === 'page')).toEqual([overview]);

    await user.click(within(nav).getByRole('link', { name: 'Blob stores' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Blob stores' })).toBeInTheDocument();
    expect(within(nav).getByRole('link', { name: 'Blob stores' })).toHaveAttribute('aria-current', 'page');
    expect(overview).not.toHaveAttribute('aria-current');
  });

  it('keeps Repositories active on the create-repository page', async () => {
    renderApp('/repositories/new');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    await waitFor(() => expect(within(nav).getByRole('link', { name: 'Repositories' })).toHaveAttribute('aria-current', 'page'));
  });

  it('sends signed-out visitors to the login page', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ code: 'unauthenticated', message: 'no' }, { status: 401 })));
    const { history } = renderApp('/tasks');
    expect(await screen.findByRole('heading', { name: 'Welcome back' })).toBeInTheDocument();
    expect(history.location.pathname).toBe('/login');
  });
});
