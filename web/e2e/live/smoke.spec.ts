import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

/**
 * Smoke test of the real UI against a real server (playwright.live.config.ts):
 * sign in, create a repository, create an access token, and the read-only
 * state of a user with the Reader role, whose changes the server refuses.
 */

const env = (name: string) => {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is not set (see playwright.live.config.ts)`);
  return v;
};
const ADMIN = env('E2E_LIVE_USER');
const ADMIN_PASSWORD = env('E2E_LIVE_PASSWORD');
const TOKEN = env('E2E_LIVE_TOKEN');

const sfx = Date.now().toString(36);
const repo = `pw-raw-${sfx}`;
const reader = `pw-reader-${sfx}`;
// Must not contain the username (password policy).
const readerPassword = `Correct-horse-${sfx}-42`;
const bearer = { Authorization: `Bearer ${TOKEN}` };

test.describe.configure({ mode: 'serial' });

async function signIn(page: Page, username: string, password: string) {
  await page.goto('/login');
  await page.getByLabel('Username').fill(username);
  await page.getByLabel('Password', { exact: true }).fill(password);
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page).not.toHaveURL(/\/login/);
}

/** Signs in through the API; when the password must be replaced first, replaces it. */
async function ensureUsablePassword(request: APIRequestContext, username: string, password: string) {
  const login = await request.post('/api/v1/auth/login', { data: { username, password } });
  expect(login.status(), await login.text()).toBe(200);
  const session = (await login.json()) as { password_change_required?: boolean };
  if (!session.password_change_required) return password;
  const csrf = login.headers()['x-csrf-token'] ?? '';
  const next = `${password}-new`;
  const change = await request.put('/api/v1/me/password', {
    data: { current_password: password, new_password: next },
    headers: { 'X-CSRF-Token': csrf },
  });
  expect(change.status(), await change.text()).toBe(204);
  return next;
}

test.afterAll(async ({ request }) => {
  await request.delete(`/api/v1/repositories/${repo}`, { headers: bearer });
  const users = await request.get('/api/v1/users', { headers: bearer, params: { q: reader } });
  if (users.ok()) {
    const body = (await users.json()) as { items?: { id: string; username: string }[] };
    for (const u of body.items ?? []) {
      if (u.username === reader) await request.delete(`/api/v1/users/${u.id}`, { headers: bearer });
    }
  }
});

test('an administrator signs in, creates a repository and an access token', async ({ page, request }) => {
  await signIn(page, ADMIN, ADMIN_PASSWORD);

  await page.goto('/repositories/new');
  await page.getByRole('radio', { name: /Raw/ }).check({ force: true });
  await page.getByRole('radio', { name: /Hosted/ }).check({ force: true });
  await page.getByLabel('Name', { exact: true }).fill(repo);
  await page.getByRole('button', { name: 'Create repository' }).click();
  await expect(page).toHaveURL(/\/repositories(\?|$)/);
  await expect(page.getByRole('link', { name: repo, exact: true })).toBeVisible();
  const created = await request.get(`/api/v1/repositories/${repo}`, { headers: bearer });
  expect(created.status()).toBe(200);
  expect(await created.json()).toMatchObject({ name: repo, format: 'raw', type: 'hosted' });

  await page.goto('/access?tab=tokens');
  await page.getByRole('button', { name: 'New token' }).click();
  const dialog = page.getByRole('dialog', { name: 'New personal access token' });
  await dialog.getByLabel('Name', { exact: true }).fill(`pw ${sfx}`);
  await dialog.getByRole('button', { name: 'Create token' }).click();
  const secretDialog = page.getByRole('dialog', { name: 'Copy your token now' });
  const secret = (await secretDialog.getByTestId('token-secret').textContent())?.trim() ?? '';
  expect(secret).toMatch(/^hgr_/);

  // The new token authenticates against the API.
  const session = await request.get('/api/v1/auth/session', { headers: { Authorization: `Bearer ${secret}` } });
  expect(session.status()).toBe(200);
  expect(await session.json()).toMatchObject({ user: { username: ADMIN } });
});

test('a reader sees the repository settings read-only and cannot change them', async ({ page, request, playwright }) => {
  const res = await request.post('/api/v1/users', {
    headers: bearer,
    data: { username: reader, kind: 'user', password: readerPassword, roles: ['Reader'] },
  });
  expect(res.status(), await res.text()).toBe(201);
  const api = await playwright.request.newContext({ baseURL: test.info().project.use.baseURL });
  const password = await ensureUsablePassword(api, reader, readerPassword);
  await api.dispose();

  await signIn(page, reader, password);
  await page.goto(`/repositories/${repo}/settings`);
  await expect(page.getByText('Changing them needs admin rights on this repository.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'Save changes' })).toHaveCount(0);

  // The server refuses the change too, whatever the UI shows.
  const csrf = (await page.context().cookies()).find((c) => c.name === 'hangar_csrf')?.value ?? '';
  const put = await page.request.put(`/api/v1/repositories/${repo}`, {
    headers: { 'X-CSRF-Token': csrf },
    data: { name: repo, format: 'raw', type: 'hosted', blob_store: 'default', online: false, config: {} },
  });
  expect(put.status(), await put.text()).toBe(403);
});
