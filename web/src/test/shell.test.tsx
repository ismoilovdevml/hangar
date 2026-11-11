import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import * as data from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

describe('app shell', () => {
  it('renders sidebar, top bar and system status', async () => {
    renderApp('/');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    for (const label of ['Overview', 'Packages', 'Search', 'Repositories', 'Blob stores', 'Cleanup policies', 'Vulnerabilities', 'Access & audit', 'Scheduled tasks', 'Cluster & replication']) {
      expect(within(nav).getByRole('link', { name: label })).toBeInTheDocument();
    }
    // The mock cluster has a degraded and a silent node: the card says what System status says.
    const card = (await within(nav).findByText('1 check needs attention')).closest('a')!;
    expect(card).toHaveClass('system-card--warn');
    expect(within(nav).getByText('2 of 3 nodes · v1.0.0')).toBeInTheDocument();
    const crumbs = screen.getByRole('navigation', { name: 'Breadcrumb' });
    expect(await within(crumbs).findByText('acme-platform')).toBeInTheDocument();
    expect(within(crumbs).getByText('Overview')).toHaveAttribute('aria-current', 'page');
    // The Overview's header shows the same state instead of the search button.
    const pill = await screen.findByRole('link', { name: '1 check needs attention' });
    expect(pill).toHaveAttribute('href', '/system/status');
    expect(pill).toHaveClass('status-pill--warn');
    expect(screen.queryByText('All systems normal')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Search packages, digests, CVEs/ })).not.toBeInTheDocument();
  });

  it('lists the To do items in the bell', async () => {
    const user = userEvent.setup();
    renderApp('/');
    const todo = await screen.findByRole('region', { name: 'To do' });
    const titles = (root: HTMLElement) => [...root.querySelectorAll('.todo__item-title')].map((e) => e.textContent);
    await waitFor(() => expect(titles(todo)).toContain('Node hangar-3 is not responding'));
    const bell = await screen.findByRole('button', { name: /^Notifications, \d+ waiting$/ });
    await user.click(bell);
    const panel = screen.getByRole('region', { name: 'Notifications' });
    expect(titles(panel)).toEqual(titles(todo));
  });

  it('is grey, not green, while the status loads', async () => {
    server.use(http.get('*/api/v1/system/status', () => new Promise<never>(() => {})));
    renderApp('/');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    const card = (await within(nav).findByText('Checking status…')).closest('a')!;
    expect(card).toHaveClass('system-card--neutral');
    const header = screen.getByRole('banner');
    expect(await within(header).findByRole('link', { name: 'Checking status…' })).toHaveClass('status-pill--neutral');
  });

  it('keeps the search button and status card on other pages, and shows a degraded system', async () => {
    server.use(
      http.get('*/api/v1/system/status', () =>
        HttpResponse.json({ ...data.systemStatus, state: 'degraded', message: 'Upstream proxies degraded', nodes_up: 2 }),
      ),
    );
    renderApp('/repositories');
    expect(await screen.findByRole('button', { name: /Search packages, digests, CVEs/ })).toBeInTheDocument();
    const nav = screen.getByRole('navigation', { name: 'Primary' });
    const card = (await within(nav).findByText('Upstream proxies degraded')).closest('a')!;
    expect(card).toHaveAttribute('href', '/system/status');
    expect(card).toHaveClass('system-card--warn');
    expect(within(card).getByText('2 of 3 nodes · v1.0.0')).toBeInTheDocument();
  });

  it('says when the system status cannot be read', async () => {
    server.use(http.get('*/api/v1/system/status', () => HttpResponse.json({ code: 'internal', message: 'boom' }, { status: 500 })));
    renderApp('/');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    expect(await within(nav).findByText('Status unavailable', {}, { timeout: 4000 })).toBeInTheDocument();
    const header = screen.getByRole('banner');
    expect(within(header).getByRole('link', { name: 'Status unavailable' })).toHaveAttribute('href', '/system/status');
  });

  it('keeps the sidebar free of an instance badge; the breadcrumb names the instance', async () => {
    server.use(
      http.get('*/api/v1/auth/session', () =>
        HttpResponse.json({ ...data.session, organization: { slug: 'hangar', name: 'hangar', environment: '', region: '' } }),
      ),
    );
    renderApp('/');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    expect(within(nav).queryByText('acme-platform')).not.toBeInTheDocument();
    expect(nav.querySelector('.instance-badge')).toBeNull();
    expect(await within(screen.getByRole('navigation', { name: 'Breadcrumb' })).findByText('Hangar')).toBeInTheDocument();
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

  it('keeps the working pages in the sidebar; My account lives in Settings', async () => {
    renderApp('/');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    for (const label of ['System status', 'System logs', 'Audit log', 'Settings', 'Authentication', 'Proxies']) {
      expect(within(nav).getByRole('link', { name: label })).toBeInTheDocument();
    }
    expect(within(nav).getByRole('heading', { name: 'Settings' })).toBeInTheDocument();
    expect(within(nav).queryByRole('link', { name: 'My account' })).not.toBeInTheDocument();
  });

  it.each([
    ['/account', 'Settings'],
    ['/api-docs', 'Settings'],
  ])('lights up the area of %s without calling it the current page', async (path, label) => {
    renderApp(path);
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    const item = within(nav).getByRole('link', { name: label });
    await waitFor(() => expect(item).toHaveAttribute('aria-current', 'true'));
    expect(item).toHaveClass('is-active');
    const marked = within(nav)
      .getAllByRole('listitem')
      .map((li) => within(li).getByRole('link'))
      .filter((a) => a.hasAttribute('aria-current'));
    expect(marked).toEqual([item]);
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

describe('versions', () => {
  it('shows a server version that already starts with v once, everywhere', async () => {
    server.use(http.get('*/api/v1/system/status', () => HttpResponse.json({ ...data.systemStatus, version: 'v0.1.6' })));
    renderApp('/settings/system');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    expect(await within(nav).findByText(/nodes · v0\.1\.6$/)).toBeInTheDocument();
    const status = await screen.findByRole('region', { name: 'System status' });
    expect(await within(status).findByText('v0.1.6')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('vv0.1.6');
  });

  it('adds the v only where it is missing', async () => {
    const { versionText } = await import('../lib/format');
    expect(versionText('0.1.6')).toBe('v0.1.6');
    expect(versionText('v0.1.6')).toBe('v0.1.6');
    expect(versionText('V0.1.6')).toBe('v0.1.6');
    expect(versionText('dev')).toBe('dev');
    expect(versionText(' v1.0.0-rc.1 ')).toBe('v1.0.0-rc.1');
  });
});

describe('avatars', () => {
  it('tells prefixed usernames apart', async () => {
    const { initials } = await import('../lib/format');
    expect(['qa-bob', 'qa-help', 'qa-lead'].map(initials)).toEqual(['QB', 'QH', 'QL']);
    expect(initials('Dilshod Ismoilov')).toBe('DI');
    expect(initials('d.ismoilov@acme.example')).toBe('DI');
    expect(initials('admin')).toBe('AD');
  });
});

