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
    await expect(page.getByText('All systems normal')).toHaveCount(0);
    await page.getByLabel('Username').fill('d.ismoilov');
    await page.getByLabel('Password').fill('password1234');
    await page.getByLabel('Remember me').check();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(name) });
  }
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'What do you need today?' })).toBeVisible();
  expect(errors).toEqual([]);
});

test('menus and listboxes sit on a layer above the glass panels', async ({ page }) => {
  // Every glass panel is its own stacking context (backdrop-filter), and browsers
  // hit-test those differently from how they paint them, so this checks the layers
  // themselves: the top bar (with its menus) and portalled listboxes need a z-index
  // above the page's panels, which have none.
  await page.setViewportSize(DESKTOP);
  await page.goto('/');
  await expect(page.getByRole('radiogroup', { name: 'Time range' })).toBeVisible();
  const z = (selector: string) => page.locator(selector).first().evaluate((el) => getComputedStyle(el).zIndex);
  expect(Number(await z('.topbar'))).toBeGreaterThan(0);
  expect(await z('.segmented')).toBe('auto');
  await page.getByRole('button', { name: /^Account/ }).click();
  const signOut = page.getByRole('menuitem', { name: 'Sign out' });
  await expect(signOut).toBeVisible();
  // The menu covers the hero search box: what is hit there must be the menu, not the panel underneath.
  const box = (await signOut.boundingBox())!;
  const hit = await page.evaluate(([x, y]) => document.elementFromPoint(x!, y!)?.closest('[role="menu"]') !== null, [
    box.x + box.width / 2,
    box.y + box.height / 2,
  ]);
  expect(hit).toBe(true);

  await page.goto('/repositories');
  await page.getByLabel('Format').click();
  await expect(page.getByRole('listbox')).toBeVisible();
  expect(Number(await z('.select-popup'))).toBeGreaterThan(Number(await z('.topbar')));
});

test('overview renders live data at desktop and phone width', async ({ page }) => {
  const errors = await collectErrors(page);
  for (const [name, size] of [['overview-1440', DESKTOP], ['overview-390', PHONE]] as const) {
    await page.setViewportSize(size);
    await page.goto('/');
    await expect(page.getByText('4.82 TB', { exact: true })).toBeVisible();
    await expect(page.getByText('98.3%', { exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: /Review 7 critical vulnerabilities/ })).toBeVisible();
    await expect(page.getByRole('link', { name: /log4j-core/ })).toBeVisible();
    await expect(page.getByRole('link', { name: /payments-api/ }).first()).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(name), fullPage: true });
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, `horizontal overflow at ${size.width}px`).toBeLessThanOrEqual(0);
  }
  expect(errors).toEqual([]);
});

test('account menu opens from the avatar at desktop and phone width', async ({ page }) => {
  const errors = await collectErrors(page);
  for (const [name, size] of [['account-menu-1440', DESKTOP], ['account-menu-390', PHONE]] as const) {
    await page.setViewportSize(size);
    await page.goto('/repositories');
    const avatar = page.getByRole('button', { name: /^Account/ });
    await avatar.focus();
    await page.keyboard.press('ArrowDown');
    const menu = page.getByRole('menu');
    await expect(menu).toBeVisible();
    await expect(menu.getByRole('menuitem', { name: 'My account' })).toBeFocused();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(name) });
    // Fully on screen, even on a phone.
    const box = (await menu.boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(size.width);
    await page.keyboard.press('Escape');
    await expect(menu).toBeHidden();
    await expect(avatar).toBeFocused();
  }
  expect(errors).toEqual([]);
});

test('repositories list filters through the URL at desktop and phone width', async ({ page }) => {
  const errors = await collectErrors(page);
  for (const [name, size] of [['repositories-1440', DESKTOP], ['repositories-390', PHONE]] as const) {
    await page.setViewportSize(size);
    await page.goto('/repositories');
    await expect(page.getByRole('link', { name: 'docker-prod', exact: true })).toBeVisible();
    await expect(page.getByText('Slow upstream')).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(name), fullPage: true });
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, `horizontal overflow at ${size.width}px`).toBeLessThanOrEqual(0);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByRole('navigation', { name: 'Repository type' }).getByRole('link', { name: /^Proxy/ }).click();
  await page.getByLabel('Format').click();
  await page.getByRole('option', { name: 'npm', exact: true }).click();
  await expect(page).toHaveURL(/type=proxy/);
  await expect(page).toHaveURL(/format=npm/);
  await expect(page.getByRole('link', { name: 'npmjs-proxy', exact: true })).toBeVisible();
  await expect(page.getByRole('link', { name: 'docker-prod', exact: true })).toBeHidden();
  expect(errors).toEqual([]);
});

test('new repository form previews YAML and creates the repository', async ({ page }) => {
  const errors = await collectErrors(page);
  for (const [name, size] of [['repo-create-1440', DESKTOP], ['repo-create-390', PHONE]] as const) {
    await page.setViewportSize(size);
    await page.goto('/repositories/new');
    await page.getByRole('radio', { name: /npm/ }).check({ force: true });
    await page.getByRole('radio', { name: /Proxy/ }).check();
    await expect(page.getByLabel('Name')).toHaveValue('npm-proxy');
    await expect(page.getByText('remote_url: https://registry.npmjs.org')).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(name), fullPage: true });
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, `horizontal overflow at ${size.width}px`).toBeLessThanOrEqual(0);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByLabel('Name').fill('npm-mirror');
  await page.getByRole('button', { name: 'Create repository' }).click();
  // The new repository's page, with a confirmation and the next step.
  await expect(page).toHaveURL(/\/repositories\/npm-mirror\/settings$/);
  await expect(page.getByRole('status').filter({ hasText: 'npm-mirror is ready.' })).toBeVisible();
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
