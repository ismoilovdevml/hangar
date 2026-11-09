import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

/** What each role holds; the admin is the mock session as it is. */
const READER = ['repo:*:read'];
const NPM_ADMIN = ['repo:*:read', 'repo:npm-*:admin'];
const OPERATOR = ['repo:*:read', 'system:tasks:read', 'system:blobstores:read'];

const signInWith = (privileges: string[]) =>
  server.use(
    http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges }, { headers: { 'X-CSRF-Token': 'mock-csrf-0' } })),
  );

/** Paths the app asked the API for, without the /api/v1 prefix. */
function recordRequests(): string[] {
  const asked: string[] = [];
  server.events.on('request:start', ({ request }) => void asked.push(new URL(request.url).pathname.replace(/^\/api\/v1/, '')));
  return asked;
}

afterEach(() => server.events.removeAllListeners());

const sidebarLinks = async () => {
  const nav = await screen.findByRole('navigation', { name: 'Primary' });
  await within(nav).findByRole('link', { name: 'Overview' });
  return within(nav)
    .getAllByRole('link')
    .map((a) => a.textContent?.trim())
    .filter((t) => t && t !== 'HangarPackage registry' && !t.includes('nodes'));
};

describe('reader (repo:*:read)', () => {
  it('sees only the pages it can open in the sidebar, with no empty sections', async () => {
    signInWith(READER);
    renderApp('/repositories');
    expect(await sidebarLinks()).toEqual(['Overview', 'Packages', 'Search', 'Repositories', 'Vulnerabilities', 'Settings']);
    const nav = screen.getByRole('navigation', { name: 'Primary' });
    expect(within(nav).queryByRole('heading', { name: 'Storage' })).not.toBeInTheDocument();
    expect(within(nav).queryByRole('heading', { name: 'Operations' })).not.toBeInTheDocument();
  });

  it.each([
    ['/tasks', 'Scheduled tasks', 'scheduled tasks', 'system:tasks:read', '/tasks'],
    ['/tasks/t_cleanup', 'Task', 'scheduled tasks', 'system:tasks:read', '/tasks'],
    ['/blob-stores', 'Blob stores', 'blob stores', 'system:blobstores:read', '/blob-stores'],
    ['/blob-stores/fs-local', 'Blob store', 'blob stores', 'system:blobstores:read', '/blob-stores'],
    ['/cleanup', 'Cleanup policies', 'cleanup policies', 'system:cleanup:read', '/cleanup-policies'],
    ['/audit', 'Audit log', 'the audit log', 'system:audit:read', '/audit'],
    ['/access', 'Access & audit', 'people, tokens and roles', 'system:users:read', '/users'],
    ['/cluster', 'Cluster & replication', 'cluster nodes', 'system:cluster:read', '/nodes'],
    ['/system/status', 'System status', 'cluster nodes', 'system:cluster:read', '/system/nodes'],
    ['/system/logs', 'System logs', 'system logs', 'system:cluster:admin', '/system/logs'],
    ['/repositories/new', 'New repository', 'creating repositories', 'system:repos:admin', '/blob-stores'],
    ['/settings/proxies', 'Proxies', 'these settings', 'system:settings:read', '/proxies'],
  ])('%s says who to ask, without calling the API or offering Retry', async (path, title, what, privilege, endpoint) => {
    signInWith(READER);
    const asked = recordRequests();
    renderApp(path);
    expect(await screen.findByRole('heading', { level: 1, name: title })).toBeInTheDocument();
    const note = within(screen.getByRole('main')).getByRole('note');
    expect(note).toHaveTextContent(`You don’t have access to ${what}. Ask an administrator for ${privilege}.`);
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
    expect(screen.queryByText(/not available on this server/)).not.toBeInTheDocument();
    expect(asked.filter((p) => p === endpoint || p.startsWith(`${endpoint}/`))).toEqual([]);
  });

  it('is not offered repository creation anywhere', async () => {
    signInWith(READER);
    const user = userEvent.setup();
    renderApp('/repositories');
    expect(await screen.findByRole('table', { name: 'Repositories' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /New repository/ })).not.toBeInTheDocument();

    await user.keyboard('{Meta>}k{/Meta}');
    const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
    await user.type(within(dialog).getByRole('combobox'), 'create');
    expect(within(dialog).queryByRole('option', { name: 'Create a repository' })).not.toBeInTheDocument();
  });

  it('has no repository creation shortcut on the overview', async () => {
    signInWith(READER);
    renderApp('/');
    const actions = await screen.findByRole('list', { name: 'Quick actions' });
    expect(within(actions).getByText('Connect a client')).toBeInTheDocument();
    expect(within(actions).queryByText('New repository')).not.toBeInTheDocument();
  });

  it('sees a repository’s settings without the actions it cannot take', async () => {
    signInWith(READER);
    renderApp('/repositories/docker-prod/settings');
    expect(await screen.findByText(/Changing them needs admin rights/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Take offline' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Delete / })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save changes' })).not.toBeInTheDocument();
  });

  it('sees only the Settings rows it can open', async () => {
    signInWith(READER);
    renderApp('/settings');
    const main = await screen.findByRole('main');
    expect(await within(main).findByRole('link', { name: /My account/ })).toBeInTheDocument();
    expect(within(main).queryByRole('link', { name: /Sign-in/ })).not.toBeInTheDocument();
    expect(within(main).queryByRole('link', { name: /Storage/ })).not.toBeInTheDocument();
  });
});

describe('repository admin of npm-* (repo:npm-*:admin)', () => {
  it('may create repositories, and is told why cleanup policies cannot be listed', async () => {
    signInWith(NPM_ADMIN);
    server.use(http.get('*/api/v1/cleanup-policies', () => HttpResponse.json({ code: 'forbidden', message: 'forbidden' }, { status: 403 })));
    renderApp('/repositories');
    const add = await screen.findByRole('link', { name: /New repository/ });
    expect(add).toHaveAttribute('href', '/repositories/new');

    renderApp('/repositories/new');
    expect(await screen.findByRole('heading', { level: 1, name: 'Create a repository' })).toBeInTheDocument();
    expect(await screen.findByText('Choosing a policy needs the system:cleanup:read privilege.')).toBeInTheDocument();
    expect(screen.queryByText(/not available on this server/)).not.toBeInTheDocument();
  });
});

describe('operator (tasks and blob stores, read only)', () => {
  it('opens the pages it may read, without the actions it may not take', async () => {
    signInWith(OPERATOR);
    renderApp('/tasks');
    expect(await sidebarLinks()).toEqual([
      'Overview',
      'Packages',
      'Search',
      'Repositories',
      'Blob stores',
      'Vulnerabilities',
      'Scheduled tasks',
      'Settings',
    ]);
    const table = await screen.findByRole('table', { name: 'Scheduled tasks' });
    await waitFor(() => expect(within(table).getAllByRole('row').length).toBeGreaterThan(1));
    expect(within(table).queryByRole('button', { name: /^Run / })).not.toBeInTheDocument();
  });

  it('hides blob store management', async () => {
    signInWith(OPERATOR);
    renderApp('/blob-stores/fs-local');
    expect(await screen.findByRole('heading', { level: 1, name: /fs-local/ })).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Manage' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Delete fs-local/ })).not.toBeInTheDocument();
  });

  it('turns a refusal the session did not predict into a request for access, not a Retry', async () => {
    signInWith(OPERATOR);
    server.use(http.get('*/api/v1/tasks', () => HttpResponse.json({ code: 'forbidden', message: 'forbidden' }, { status: 403 })));
    renderApp('/tasks');
    const note = await within(await screen.findByRole('region', { name: 'Task list' })).findByRole('note');
    expect(note).toHaveTextContent('You don’t have access to scheduled tasks. Ask an administrator for system:tasks:read.');
    expect(screen.queryByRole('button', { name: 'Retry' })).not.toBeInTheDocument();
  });
});

describe('administrator', () => {
  it('sees every page and action', async () => {
    renderApp('/repositories');
    expect(await sidebarLinks()).toEqual([
      'Overview',
      'Packages',
      'Search',
      'Repositories',
      'Blob stores',
      'Cleanup policies',
      'Vulnerabilities',
      'Access & audit',
      'Audit log',
      'Scheduled tasks',
      'Cluster & replication',
      'System status',
      'System logs',
      'Settings',
      'Authentication',
      'Proxies',
    ]);
    expect(await screen.findByRole('link', { name: /New repository/ })).toBeInTheDocument();
  });
});
