import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { mockState } from '../mocks/handlers';
import { renderApp } from './render';

describe('API reference', () => {
  it('renders the OpenAPI document grouped by tag, with schemas and curl for this server', async () => {
    renderApp('/api-docs');
    expect(await screen.findByRole('heading', { level: 2, name: 'repositories' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'Hangar API' })).toBeInTheDocument();
    // Signed in, it is a page of the app under Settings.
    const crumbs = screen.getByRole('navigation', { name: 'Breadcrumb' });
    expect(within(crumbs).getByRole('link', { name: 'Settings' })).toHaveAttribute('href', '/settings');
    expect(within(crumbs).getByText('API reference')).toBeInTheDocument();
    expect(screen.getByText('https://pkg.acme.io/api/v1')).toBeInTheDocument();
    expect(screen.getByText('7 endpoints')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '/llms.txt' })).toHaveAttribute('href', '/llms.txt');
    const toc = screen.getByRole('complementary', { name: 'Endpoints' });
    expect(within(toc).getAllByRole('link').map((a) => a.getAttribute('href'))).toEqual([
      '#start',
      '#tag-auth',
      '#tag-docs',
      '#tag-repositories',
      '#tag-access',
      '#tag-events',
      '#schemas',
    ]);

    const user = userEvent.setup();
    const create = document.getElementById('op-createRepository')!;
    await user.click(within(create).getByText('Create a repository'));
    expect(within(create).getByText('system:repos:admin')).toBeInTheDocument();
    expect(within(create).getByRole('cell', { name: /^name/ })).toHaveTextContent('required');
    const curl = within(create).getByRole('button', { name: 'Copy curl command for createRepository' }).closest('.code-block')!;
    expect(curl.textContent).toContain('curl -fsS');
    expect(curl.textContent).toContain('-X POST');
    expect(curl.textContent).toContain('"https://pkg.acme.io/api/v1/repositories"');
    expect(curl.textContent).toContain('"name": "npm-team"');

    // Schema links open the schema card.
    expect(within(create).getAllByRole('link', { name: 'Repository' })[0]).toHaveAttribute('href', '#schema-Repository');
    expect(document.getElementById('schema-Repository')).toBeInTheDocument();
  });

  it('searches endpoints', async () => {
    const user = userEvent.setup();
    renderApp('/api-docs');
    const search = await screen.findByRole('searchbox', { name: 'Search endpoints' });
    await screen.findByRole('heading', { level: 2, name: 'repositories' });
    await user.type(search, 'rotate');
    expect(screen.getByText('1 of 7 endpoints')).toBeInTheDocument();
    expect(screen.queryByRole('heading', { level: 2, name: 'repositories' })).not.toBeInTheDocument();
    // The only match opens by itself.
    const op = document.getElementById('op-rotateToken')!;
    expect(op).toHaveAttribute('open');
    expect(within(op).getByText('Try it')).toBeInTheDocument();
    await user.clear(search);
    await user.type(search, 'no such endpoint');
    expect(screen.getByText('No endpoint matches “no such endpoint”.')).toBeInTheDocument();
  });

  it('tries a call with the current session and shows the answer', async () => {
    const user = userEvent.setup();
    renderApp('/api-docs');
    await screen.findByRole('heading', { level: 2, name: 'auth' });
    expect(await screen.findByRole('radio', { name: 'Your session (d.ismoilov)' })).toHaveAttribute('aria-checked', 'true');
    const op = document.getElementById('op-getSession')!;
    await user.click(within(op).getByText('Current user and organization'));
    await user.click(within(op).getByRole('button', { name: 'Send request' }));
    const out = await within(op).findByText(/"username": "d.ismoilov"/);
    expect(out).toBeInTheDocument();
    expect(out.closest('.try__result')).toHaveTextContent(/^200 /);

    // Streams cannot be tried from the page.
    const events = document.getElementById('op-streamEvents')!;
    await user.click(within(events).getByText('Live updates'));
    expect(within(events).getByText(/streams server-sent events/)).toBeInTheDocument();
  });

  it('is public: signed out, it offers sign-in and a pasted token', async () => {
    mockState.signedIn = false;
    const user = userEvent.setup();
    renderApp('/api-docs');
    expect(await screen.findByRole('heading', { level: 2, name: 'repositories' })).toBeInTheDocument();
    expect(await screen.findByRole('link', { name: 'Sign in' })).toBeInTheDocument();
    expect(screen.queryByRole('navigation', { name: 'Breadcrumb' })).not.toBeInTheDocument();
    expect(screen.getByRole('radio', { name: 'An access token' })).toHaveAttribute('aria-checked', 'true');
    const token = screen.getByLabelText('Access token');
    expect(token).toHaveAttribute('type', 'password');
    const op = document.getElementById('op-getRepository')!;
    await user.click(within(op).getByText('One repository'));
    expect(within(op).getByRole('button', { name: 'Send request' })).toBeDisabled();
    expect(within(op).getByText('Fill in name.')).toBeInTheDocument();
  });
});
