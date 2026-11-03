import { expect, test, type Page } from '@playwright/test';

const DESKTOP = { width: 1440, height: 900 };
const PHONE = { width: 390, height: 844 };
const shot = (name: string) => `test-results/screens/${name}.png`;

// A fixed morning so the greeting, date and relative times are stable.
const NOW = new Date('2026-11-03T09:00:00Z');

async function collectErrors(page: Page) {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => m.type() === 'error' && errors.push(m.text()));
  return errors;
}

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

test('login renders and signs in', async ({ page }) => {
  const errors = await collectErrors(page);
  for (const [name, size] of [['login-1440', DESKTOP], ['login-390', PHONE]] as const) {
    await page.setViewportSize(size);
    await page.goto('/login');
    await expect(page.getByRole('link', { name: 'Continue with company SSO' })).toBeVisible();
    await expect(page.getByText('All systems normal')).toBeVisible();
    await page.getByLabel('Username').fill('d.ismoilov');
    await page.getByLabel('Password').fill('password1234');
    await page.getByLabel('Remember me').check();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(name) });
  }
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { level: 1, name: /Good morning, Dilshod/ })).toBeVisible();
  expect(errors).toEqual([]);
});

test('overview renders live data at desktop and phone width', async ({ page }) => {
  const errors = await collectErrors(page);
  for (const [name, size] of [['overview-1440', DESKTOP], ['overview-390', PHONE]] as const) {
    await page.setViewportSize(size);
    await page.goto('/');
    await expect(page.getByText('4.82', { exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: /payments-api/ })).toBeVisible();
    await expect(page.getByText('docker login cr.acme.io')).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(name), fullPage: true });
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, `horizontal overflow at ${size.width}px`).toBeLessThanOrEqual(0);
  }
  expect(errors).toEqual([]);
});

test('phone navigation opens from the menu button', async ({ page }) => {
  await page.setViewportSize(PHONE);
  await page.goto('/');
  const nav = page.getByRole('navigation', { name: 'Primary' });
  await expect(nav.getByRole('link', { name: 'Blob stores' })).toBeHidden();
  await page.getByRole('button', { name: 'Open navigation' }).click();
  await page.screenshot({ path: shot('sidebar-open-390'), fullPage: true });
  await nav.getByRole('link', { name: 'Blob stores' }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Blob stores' })).toBeVisible();
  await expect(nav.getByRole('link', { name: 'Blob stores' })).toBeHidden();
});

test('command palette opens with the keyboard shortcut', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  await page.goto('/');
  await expect(page.getByRole('navigation', { name: 'Primary' })).toBeVisible();
  await page.keyboard.press('ControlOrMeta+k');
  const dialog = page.getByRole('dialog', { name: 'Command palette' });
  await expect(dialog).toBeVisible();
  await page.keyboard.type('tasks');
  await page.screenshot({ path: shot('palette-1440') });
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { level: 1, name: 'Scheduled tasks' })).toBeVisible();
});

const SCREENS: [path: string, title: string, nav: string][] = [
  ['/repositories', 'Repositories', 'Repositories'],
  ['/repositories/new', 'Create a repository', 'Repositories'],
  ['/browse', 'Packages', 'Packages'],
  ['/search', 'Find any package', 'Search'],
  ['/blob-stores', 'Blob stores', 'Blob stores'],
  ['/cleanup', 'Cleanup policies', 'Cleanup policies'],
  ['/vulnerabilities', 'Vulnerabilities', 'Vulnerabilities'],
  ['/access', 'Access & audit', 'Access & audit'],
  ['/tasks', 'Scheduled tasks', 'Scheduled tasks'],
  ['/cluster', 'Cluster & replication', 'Cluster & replication'],
];

test('every screen has a page frame and an active nav item', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  const nav = page.getByRole('navigation', { name: 'Primary' });
  for (const [path, title, navLabel] of SCREENS) {
    await page.goto(path);
    await expect(page.getByRole('heading', { level: 1, name: title })).toBeVisible();
    await expect(nav.getByRole('link', { name: navLabel, exact: true })).toHaveAttribute('aria-current', 'page');
  }
});
