import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { ApiRequestError } from '../api/client';
import { cleanServerMessage, describeError, explainConnectionFailure } from '../lib/apiError';
import { server } from '../mocks/node';
import { notFoundSuggestion } from '../lib/notFound';
import { renderApp } from './render';

describe('error text', () => {
  it('tidies Go error chains from older servers', () => {
    expect(cleanServerMessage('create user "bob": already exists: already exists')).toBe('Create user "bob": already exists.');
    expect(cleanServerMessage('qualifier format: empty value: invalid argument')).toBe('Qualifier format: empty value.');
    expect(cleanServerMessage('Name is taken.')).toBe('Name is taken.');
  });

  it('leads with a plain sentence and keeps the raw answer for Details', () => {
    const conflict = describeError(new ApiRequestError(409, { code: 'already_exists', message: 'proxy "corp": already exists' }));
    expect(conflict).toEqual({ message: 'Proxy "corp": already exists.', detail: 'HTTP 409 already_exists: proxy "corp": already exists' });
    expect(describeError(new ApiRequestError(500, { code: 'internal', message: 'internal error' })).message).toMatch(/^The server answered with an error \(500\)/);
    expect(describeError(new ApiRequestError(503, { code: 'unavailable', message: 'database is unavailable' })).message).toBe('Database is unavailable.');
    expect(describeError(new ApiRequestError(403, { code: 'forbidden', message: 'forbidden' })).message).toBe('You do not have permission to do this.');
    expect(describeError(new ApiRequestError(502, null)).message).toMatch(/could not reach a service/);
    expect(describeError(new ApiRequestError(502, { code: 'unavailable', message: 'node "n3" does not answer' })).message).toBe('Node "n3" does not answer.');
    expect(describeError(new TypeError('Failed to fetch')).message).toMatch(/Cannot reach the server/);
  });

  it('explains common connection test failures in one sentence', () => {
    expect(
      explainConnectionFailure(
        'GET https://api.nuget.org/v3/index.json through http://does-not-exist.invalid:3128 failed: Get "https://api.nuget.org/v3/index.json": proxyconnect tcp: dial tcp: lookup does-not-exist.invalid on 127.0.0.11:53: no such host',
      ),
    ).toBe('Can’t resolve the proxy host does-not-exist.invalid: check the address or the server’s DNS.');
    expect(explainConnectionFailure('LDAP Result Code 200 "Network Error": dial tcp: lookup does-not-exist.invalid on 127.0.0.11:53: no such host')).toBe(
      'Can’t resolve the host does-not-exist.invalid: check the address or the server’s DNS.',
    );
    expect(explainConnectionFailure('dial tcp 10.0.0.5:636: connect: connection refused')).toMatch(/^10\.0\.0\.5:636 refused the connection/);
    expect(explainConnectionFailure('LDAP Result Code 49 "Invalid Credentials"')).toMatch(/refused the bind account/);
    expect(explainConnectionFailure('something unusual')).toBeNull();
  });

  it('suggests the page behind a guessed address', () => {
    expect(notFoundSuggestion('/packages/')).toEqual({ label: 'Packages', to: '/browse' });
    expect(notFoundSuggestion('/browse/npm-hosted')).toEqual({ label: 'the packages of npm-hosted', to: '/browse', search: { repo: 'npm-hosted' } });
    expect(notFoundSuggestion('/does-not-exist')).toBeNull();
  });
});

describe('error, empty and not-found states', () => {
  it('shows an unknown address inside the shell, with a way on', async () => {
    renderApp('/does-not-exist/abc');
    expect(await screen.findByRole('heading', { level: 1, name: 'Page not found' })).toBeInTheDocument();
    const main = screen.getByRole('main');
    expect(within(main).getByText('/does-not-exist/abc')).toBeInTheDocument();
    expect(within(main).getByRole('link', { name: 'Go to the overview' })).toHaveAttribute('href', '/');
    expect(within(main).getByRole('search', { name: 'Search packages' })).toBeInTheDocument();
    // The shell is there: navigation and the top bar.
    expect(screen.getByRole('navigation', { name: /main|primary|sidebar/i })).toBeInTheDocument();
    await waitFor(() => expect(document.title).toBe('Page not found · Hangar'));
  });

  it('sends a repository address to its page and /packages to Packages', async () => {
    const { history } = renderApp('/repositories/npmjs-proxy');
    await waitFor(() => expect(history.location.pathname).toBe('/repositories/npmjs-proxy/settings'));
    const other = renderApp('/packages');
    await waitFor(() => expect(other.history.location.pathname).toBe('/browse'));
  });

  it('keeps the shell when a malformed answer breaks a page, and Retry recovers', async () => {
    let broken = true;
    server.use(
      http.get('*/api/v1/repositories', ({ request }) => {
        if (!broken) return undefined;
        void request;
        return HttpResponse.json({ items: 'not a list', next_cursor: null });
      }),
    );
    renderApp('/repositories');
    expect(await screen.findByRole('heading', { level: 1, name: 'This page could not be shown' })).toBeInTheDocument();
    const alert = screen.getByRole('alert');
    expect(within(alert).getByText('Details')).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: /main|primary|sidebar/i })).toBeInTheDocument();
    broken = false;
    await userEvent.click(within(alert).getByRole('button', { name: 'Retry' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Repositories' })).toBeInTheDocument();
  });

  it('says why a load failed, with Retry and the raw answer under Details', async () => {
    server.use(http.get('*/api/v1/tokens', () => HttpResponse.json({ code: 'unavailable', message: 'database is unavailable' }, { status: 503 })));
    renderApp('/account');
    const note = await screen.findByText(/^Could not load your tokens\./);
    const alert = note.closest('[role="alert"]') as HTMLElement;
    expect(alert).toHaveTextContent('Could not load your tokens. Database is unavailable.');
    expect(within(alert).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    expect(within(alert).getByText('HTTP 503 unavailable: database is unavailable')).toBeInTheDocument();
  });
});
