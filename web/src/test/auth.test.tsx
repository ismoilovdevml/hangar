import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { apiFetch, ApiRequestError, resetCsrfToken } from '../api/client';
import { leaveAfterLogout, type LogoutResult } from '../api/hooks';
import { session as mockSession } from '../mocks/data';
import { mockState } from '../mocks/handlers';
import { server } from '../mocks/node';
import { renderApp } from './render';

interface Seen {
  method: string;
  path: string;
  csrf: string | null;
  contentType: string | null;
  body: string | null;
}

/** Records every request the app makes (after MSW has matched it). */
function recordRequests(): Seen[] {
  const seen: Seen[] = [];
  server.events.on('request:start', async ({ request }) => {
    const body = request.method === 'GET' ? null : await request.clone().text();
    seen.push({
      method: request.method,
      path: new URL(request.url).pathname,
      csrf: request.headers.get('X-CSRF-Token'),
      contentType: request.headers.get('Content-Type'),
      body,
    });
  });
  return seen;
}

beforeEach(() => resetCsrfToken());
afterEach(() => {
  server.events.removeAllListeners();
  document.cookie = 'hangar_csrf=; Max-Age=0; Path=/';
});

async function signIn(user: ReturnType<typeof userEvent.setup>, username = 'd.ismoilov', password = 'correct-horse') {
  await user.type(await screen.findByLabelText('Username'), username);
  await user.type(screen.getByLabelText('Password'), password);
  await user.click(screen.getByRole('button', { name: 'Sign in' }));
}

describe('sign-in against the API', () => {
  it('posts JSON credentials, then sends the CSRF token on mutating requests only', async () => {
    mockState.signedIn = false;
    const seen = recordRequests();
    const user = userEvent.setup();
    const { history } = renderApp('/login');

    await user.click(await screen.findByLabelText('Remember me'));
    await signIn(user);
    await waitFor(() => expect(history.location.pathname).toBe('/'));

    const login = seen.find((r) => r.path === '/api/v1/auth/login')!;
    expect(login.method).toBe('POST');
    expect(login.contentType).toBe('application/json');
    expect(JSON.parse(login.body!)).toEqual({ username: 'd.ismoilov', password: 'correct-horse', remember: true, realm: 'local' });

    // Sign out from the account menu: a POST that must carry the token the login answer gave.
    await user.click(await screen.findByRole('button', { name: /^Account/ }));
    await user.click(screen.getByRole('menuitem', { name: 'Sign out' }));
    await waitFor(() => expect(history.location.pathname).toBe('/login'));

    const logout = seen.find((r) => r.path === '/api/v1/auth/logout')!;
    expect(logout.csrf).toBe('mock-csrf-1');
    expect(mockState.signedIn).toBe(false);
    expect(seen.filter((r) => r.method === 'GET').every((r) => r.csrf === null)).toBe(true);
  });

  it('sends a single sign-on session to the provider logout page, others to /login', async () => {
    server.use(
      http.post('*/api/v1/auth/logout', () =>
        HttpResponse.json({ logout_url: 'https://sso.example.com/realms/acme/protocol/openid-connect/logout?client_id=hangar' }),
      ),
    );
    const res = await apiFetch<LogoutResult>('/auth/logout', { method: 'POST', allowUnauthorized: true });
    const assign = vi.fn();
    const toLogin = vi.fn();
    leaveAfterLogout(res, toLogin, assign);
    expect(assign).toHaveBeenCalledWith('https://sso.example.com/realms/acme/protocol/openid-connect/logout?client_id=hangar');
    expect(toLogin).not.toHaveBeenCalled();

    leaveAfterLogout(undefined, toLogin, assign);
    leaveAfterLogout({ logout_url: 'javascript:alert(1)' }, toLogin, assign);
    expect(toLogin).toHaveBeenCalledTimes(2);
    expect(assign).toHaveBeenCalledTimes(1);
  });

  it('returns to the page (and query) that asked for sign-in', async () => {
    mockState.signedIn = false;
    const user = userEvent.setup();
    const { history } = renderApp('/repositories?type=proxy');
    await waitFor(() => expect(history.location.pathname).toBe('/login'));
    await signIn(user);
    await waitFor(() => expect(history.location.pathname).toBe('/repositories'));
    expect(history.location.search).toBe('?type=proxy');
    expect(await screen.findByText('dockerhub-proxy')).toBeInTheDocument();
  });

  it('explains a lockout without blaming the address, and keeps only that name waiting', async () => {
    server.use(
      http.post('*/api/v1/auth/login', async ({ request }) => {
        const body = (await request.json()) as { username: string };
        if (body.username !== 'locked-user') return HttpResponse.json(mockSession);
        return HttpResponse.json({ code: 'locked', message: 'too many failed sign-ins from this address; try again later', retry_after: 900 }, { status: 423 });
      }),
    );
    const user = userEvent.setup();
    const { history } = renderApp('/login');
    await signIn(user, 'locked-user');
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('This sign-in is locked for 15 min after too many failed attempts. An administrator can unlock it sooner.');
    expect(alert).not.toHaveTextContent(/from this address|account is locked/i);
    expect(screen.getByRole('button', { name: 'Try again in 15 min' })).toBeDisabled();

    // Another name is not locked: editing the name clears the message and the wait.
    await user.clear(screen.getByLabelText('Username'));
    await user.type(screen.getByLabelText('Username'), 'someone-else');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled();
    // Back to the locked name: the wait is still there.
    await user.clear(screen.getByLabelText('Username'));
    await user.type(screen.getByLabelText('Username'), 'locked-user');
    expect(screen.getByRole('button', { name: 'Try again in 15 min' })).toBeDisabled();
    await user.clear(screen.getByLabelText('Username'));
    await user.type(screen.getByLabelText('Username'), 'someone-else');
    await user.type(screen.getByLabelText('Password'), 'correct-horse');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() => expect(history.location.pathname).toBe('/'));
  });

  it('warns before the lock, clears a wrong password and focuses it', async () => {
    mockState.signedIn = false;
    const user = userEvent.setup();
    renderApp('/login');
    await signIn(user, 'd.ismoilov', 'wrong');
    expect(await screen.findByRole('alert')).toHaveTextContent(/^Wrong username or password\.$/);
    for (let i = 2; i <= 3; i++) {
      await user.type(screen.getByLabelText('Password'), 'wrong');
      await user.click(screen.getByRole('button', { name: 'Sign in' }));
      await waitFor(() => expect(screen.getByLabelText('Password')).toHaveValue(''));
    }
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('Wrong username or password. 2 attempts left before a 15-minute lock.');
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(screen.getByLabelText('Password')).toHaveFocus();
  });

  it('names the empty field and focuses it', async () => {
    const user = userEvent.setup();
    renderApp('/login');
    expect(await screen.findByLabelText('Username')).toHaveFocus();
    await user.type(screen.getByLabelText('Username'), 'd.ismoilov');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter your password.');
    expect(screen.getByLabelText('Username')).not.toHaveAttribute('aria-invalid');
    expect(screen.getByLabelText('Password')).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByLabelText('Password')).toHaveFocus();
  });

  it('reads the rate-limit wait from Retry-After', async () => {
    server.use(
      http.post('*/api/v1/auth/login', () =>
        HttpResponse.json({ code: 'rate_limited', message: 'too many login attempts' }, { status: 429, headers: { 'Retry-After': '30' } }),
      ),
    );
    const user = userEvent.setup();
    renderApp('/login');
    await signIn(user);
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many sign-in attempts from this address. Try again in 30 s.');
    expect(screen.getByRole('button', { name: /^Try again in \d+ s$/ })).toBeDisabled();
  });

  it('asks for the password over the page when the session ends, and keeps the page', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories/npmjs-proxy/settings');
    const form = await screen.findByRole('form', { name: 'Settings of npmjs-proxy' });
    await user.type(within(form).getByLabelText(/Description/), 'typed before the session ended');
    server.use(http.put('*/api/v1/repositories/npmjs-proxy', () => HttpResponse.json({ code: 'unauthenticated', message: 'sign in required' }, { status: 401 })));
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    const dialog = await screen.findByRole('dialog', { name: 'Your session ended' });
    expect(history.location.pathname).toBe('/repositories/npmjs-proxy/settings');
    expect(within(dialog).getByLabelText('Username')).toHaveValue(mockSession.user.username);
    expect(within(dialog).queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();
    // Escape does not throw the question away.
    await user.keyboard('{Escape}');
    expect(screen.getByRole('dialog', { name: 'Your session ended' })).toBeInTheDocument();

    server.resetHandlers();
    await user.type(within(dialog).getByLabelText('Password'), 'correct-horse');
    await user.click(within(dialog).getByRole('button', { name: 'Sign in' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Your session ended' })).not.toBeInTheDocument());
    expect(within(form).getByLabelText(/Description/)).toHaveValue('typed before the session ended');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    expect(await screen.findByRole('button', { name: 'Saved' })).toBeInTheDocument();
  });

  it('sends a tab without a session to sign-in and says why', async () => {
    mockState.signedIn = false;
    const { history } = renderApp('/repositories?format=npm');
    expect(await screen.findByRole('heading', { name: 'Welcome back' })).toBeInTheDocument();
    expect(history.location.pathname).toBe('/login');
    expect(new URLSearchParams(history.location.search).get('redirect')).toBe('/repositories?format=npm');
    history.push('/login?reason=expired');
    expect(await screen.findByText('Your session ended. Sign in again to continue.')).toBeInTheDocument();
  });
});

describe('CSRF token handling in apiFetch', () => {
  const echo = () =>
    server.use(
      http.post('*/api/v1/echo', ({ request }) => HttpResponse.json({ csrf: request.headers.get('X-CSRF-Token') })),
      http.get('*/api/v1/echo', ({ request }) => HttpResponse.json({ csrf: request.headers.get('X-CSRF-Token') })),
    );

  it('prefers the hangar_csrf cookie and never sends the token on GET', async () => {
    echo();
    document.cookie = 'hangar_csrf=from-cookie; Path=/';
    expect(await apiFetch<{ csrf: string }>('/echo', { method: 'POST', body: {} })).toEqual({ csrf: 'from-cookie' });
    expect(await apiFetch<{ csrf: string | null }>('/echo')).toEqual({ csrf: null });
  });

  it('falls back to the token echoed in the X-CSRF-Token response header', async () => {
    echo();
    await apiFetch('/auth/session'); // the mock answers with X-CSRF-Token: mock-csrf-0
    expect(await apiFetch<{ csrf: string }>('/echo', { method: 'POST', body: {} })).toEqual({ csrf: 'mock-csrf-0' });
  });

  it('refreshes a stale token once on csrf_failed and retries', async () => {
    let calls = 0;
    server.use(
      http.post('*/api/v1/echo', ({ request }) => {
        calls++;
        return request.headers.get('X-CSRF-Token') === 'mock-csrf-0'
          ? HttpResponse.json({ ok: true })
          : HttpResponse.json({ code: 'csrf_failed', message: 'missing or invalid X-CSRF-Token header' }, { status: 403 });
      }),
    );
    // No token known yet (no cookie, no header seen): the first attempt is rejected.
    expect(await apiFetch('/echo', { method: 'POST', body: {} })).toEqual({ ok: true });
    expect(calls).toBe(2);
  });

  it('exposes retry_after and the error code on failures', async () => {
    server.use(
      http.delete('*/api/v1/echo', () =>
        HttpResponse.json({ code: 'rate_limited', message: 'slow down' }, { status: 429, headers: { 'Retry-After': '12' } }),
      ),
    );
    const err = await apiFetch('/echo', { method: 'DELETE' }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiRequestError);
    expect((err as ApiRequestError).retryAfter).toBe(12);
    expect((err as ApiRequestError).code).toBe('rate_limited');
  });
});

describe('account menu', () => {
  it('shows who is signed in and links to real account pages', async () => {
    const user = userEvent.setup();
    renderApp('/');
    const avatar = await screen.findByRole('button', { name: 'Account: Dilshod Ismoilov' });
    expect(avatar).toHaveAttribute('aria-haspopup', 'menu');
    expect(avatar).toHaveAttribute('aria-expanded', 'false');
    await user.click(avatar);
    expect(avatar).toHaveAttribute('aria-expanded', 'true');
    const menu = screen.getByRole('menu', { name: 'Dilshod Ismoilov' });
    expect(within(menu).getByText('d.ismoilov@acme.example')).toBeInTheDocument();
    expect(within(menu).getByText('Admin')).toBeInTheDocument();
    expect(within(menu).getAllByRole('menuitem').map((i) => i.textContent)).toEqual([
      'My account',
      'Access tokens',
      'Password & sessions',
      'Settings',
      'API reference',
      'Sign out',
    ]);
    expect(within(menu).getByRole('menuitem', { name: 'My account' })).toHaveAttribute('href', '/account');
    expect(within(menu).getByRole('menuitem', { name: 'Access tokens' })).toHaveAttribute('href', '/account#my-tokens-title');
    expect(within(menu).getByRole('menuitem', { name: 'Password & sessions' })).toHaveAttribute('href', '/account#password-title');
    expect(within(menu).getByRole('menuitem', { name: 'Settings' })).toHaveAttribute('href', '/settings');
    expect(within(menu).getByRole('menuitem', { name: 'API reference' })).toHaveAttribute('href', '/api-docs');
    // No promise of a second factor the server does not have.
    expect(menu).not.toHaveTextContent(/MFA|two-factor/i);

    await user.click(within(menu).getByRole('menuitem', { name: 'My account' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'My account' })).toBeInTheDocument();
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('works from the keyboard and returns focus to the avatar', async () => {
    const user = userEvent.setup();
    renderApp('/repositories');
    const avatar = await screen.findByRole('button', { name: 'Account: Dilshod Ismoilov' });
    avatar.focus();
    await user.keyboard('{ArrowDown}');
    const items = within(screen.getByRole('menu')).getAllByRole('menuitem');
    expect(items[0]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(items[1]).toHaveFocus();
    await user.keyboard('{End}');
    expect(items[items.length - 1]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(items[0]).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(items[items.length - 1]).toHaveFocus();
    await user.keyboard('{Home}');
    expect(items[0]).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
    expect(avatar).toHaveFocus();

    // ArrowUp opens on the last item; Tab closes and moves on.
    await user.keyboard('{ArrowUp}');
    expect(within(screen.getByRole('menu')).getByRole('menuitem', { name: 'Sign out' })).toHaveFocus();
    await user.tab();
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();

    // A click outside closes it too.
    await user.click(avatar);
    expect(screen.getByRole('menu')).toBeInTheDocument();
    await user.click(screen.getByRole('heading', { level: 1, name: 'Repositories' }));
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('sends single sign-on users to their signed-in devices instead of a password form', async () => {
    server.use(
      http.get('*/api/v1/auth/session', () =>
        HttpResponse.json({ ...mockSession, user: { ...mockSession.user, source: 'oidc', roles: [] } }),
      ),
    );
    const user = userEvent.setup();
    renderApp('/');
    await user.click(await screen.findByRole('button', { name: /^Account/ }));
    const menu = screen.getByRole('menu');
    expect(within(menu).getByRole('menuitem', { name: 'Signed-in devices' })).toHaveAttribute('href', '/account#my-sessions-title');
    expect(within(menu).queryByRole('menuitem', { name: 'Password & sessions' })).not.toBeInTheDocument();
    expect(within(menu).queryByText('Admin')).not.toBeInTheDocument();
  });
});
