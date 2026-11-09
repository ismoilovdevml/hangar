import { expect, test, type Page } from '@playwright/test';

const DESKTOP = { width: 1440, height: 900 };
const PHONE = { width: 390, height: 844 };
const SIZES = [
  ['1440', DESKTOP],
  ['390', PHONE],
] as const;
const shot = (name: string) => `test-results/screens/${name}.png`;
const NOW = new Date('2026-11-03T09:00:00Z');

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

async function frame(page: Page, name: string) {
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: shot(name) });
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow, `horizontal overflow on ${name}`).toBeLessThanOrEqual(0);
}

/** The mock server's "reader" holds only repo:*:read. */
async function signInAsReader(page: Page) {
  await page.goto('/login');
  await page.getByLabel('Username').fill('reader');
  await page.getByLabel('Password').fill('password1234');
  await page.getByRole('button', { name: 'Sign in' }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'What do you need today?' })).toBeVisible();
}

/**
 * Navigates inside the app without reloading: the mock server runs in the
 * page, so a reload would forget who signed in.
 */
async function go(page: Page, path: string) {
  await page.evaluate((p) => {
    window.history.pushState({}, '', p);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }, path);
}

/** Visible controls smaller than 24 px (links inside running text are exempt, as in WCAG 2.5.8). */
const smallTargets = (page: Page) =>
  page.evaluate(() => {
    const out: string[] = [];
    for (const el of document.querySelectorAll<HTMLElement>('a[href], button, input:not([type=hidden]), select, textarea, [role=button], [role=radio], [role=tab]')) {
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0 || el.closest('.visually-hidden')) continue;
      const cs = getComputedStyle(el);
      if (cs.visibility === 'hidden') continue;
      const parentText = (el.parentElement?.textContent ?? '').replace(el.textContent ?? '', '');
      if (el.tagName === 'A' && cs.display === 'inline' && /\S/.test(parentText)) continue;
      if (r.width < 24 || r.height < 24) out.push(`${el.tagName} "${(el.getAttribute('aria-label') ?? el.textContent ?? '').trim().slice(0, 30)}" ${Math.round(r.width)}x${Math.round(r.height)}`);
    }
    return out;
  });

/**
 * WCAG contrast of an element's text colour against the backgrounds it is
 * painted on (alpha-composited). `under` names the element behind it when that
 * is a sibling (a copy button floating over its code block).
 */
const contrastOf = (page: Page, selector: string, under?: string) =>
  page.locator(selector).first().evaluate((el, under) => {
    const parse = (c: string) => (c.match(/[\d.]+/g) ?? ['0', '0', '0', '0']).map(Number) as number[];
    const layers: number[][] = [];
    const own = parse(getComputedStyle(el).backgroundColor);
    if ((own[3] ?? 1) > 0 && under) layers.push([own[0]!, own[1]!, own[2]!, own[3] ?? 1]);
    const start = under ? el.parentElement!.querySelector(under) : el;
    for (let n: Element | null = start; n; n = n.parentElement) {
      const [r, g, b, a = 1] = parse(getComputedStyle(n).backgroundColor);
      if (a > 0) layers.push([r!, g!, b!, a]);
      if (a >= 1) break;
    }
    let bg = [255, 255, 255];
    // Paint from the back: the outermost background first, the element's own last.
    const ordered = under ? [...layers.slice(1).reverse(), layers[0]!] : layers.reverse();
    for (const [r, g, b, a] of ordered) bg = [r! * a! + bg[0]! * (1 - a!), g! * a! + bg[1]! * (1 - a!), b! * a! + bg[2]! * (1 - a!)];
    const [fr, fg, fb] = parse(getComputedStyle(el).color);
    const lum = ([r, g, b]: number[]) =>
      [r!, g!, b!].map((v) => ((v /= 255) <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4)).reduce((s, v, i) => s + v * [0.2126, 0.7152, 0.0722][i]!, 0);
    const [l1, l2] = [lum([fr!, fg!, fb!]), lum(bg)].sort((a, b) => b - a);
    return (l1! + 0.05) / (l2! + 0.05);
  }, under);

test('the token copy button is readable on the dark secret box', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  await page.goto('/account');
  await page.getByRole('region', { name: 'Personal access tokens' }).getByRole('button', { name: 'New token' }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByLabel('Name').fill('contrast check');
  await dialog.getByRole('button', { name: /^Create token/ }).click();
  await expect(page.getByTestId('token-secret')).toBeVisible();
  expect(await contrastOf(page, '.secret-box .copy-button')).toBeGreaterThanOrEqual(4.5);
  await page.getByTestId('token-secret').scrollIntoViewIfNeeded();
  await page.screenshot({ path: shot('token-secret-1440') });
  // Every other copy button sits on a light surface.
  await page.keyboard.press('Escape');
  await page.goto('/repositories');
  expect(await contrastOf(page, '.copy-button')).toBeGreaterThanOrEqual(4.5);
  await page.goto('/repositories/docker-prod/settings');
  expect(await contrastOf(page, '.code-block__copy', 'pre')).toBeGreaterThanOrEqual(4.5);
});

test('a reader sees only what it can use, and who to ask for the rest', async ({ page }) => {
  await signInAsReader(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await go(page, '/tasks');
    await expect(page.getByRole('heading', { level: 1, name: 'Scheduled tasks' })).toBeVisible();
    const note = page.getByRole('main').getByRole('note');
    await expect(note).toHaveText('You don’t have access to scheduled tasks. Ask an administrator for system:tasks:read.');
    await expect(page.getByRole('button', { name: 'Retry' })).toHaveCount(0);
    await frame(page, `reader-tasks-${w}`);
  }
  await page.setViewportSize(DESKTOP);
  const nav = page.getByRole('navigation', { name: 'Primary' });
  for (const hidden of ['Blob stores', 'Cleanup policies', 'Access & audit', 'Scheduled tasks', 'Cluster & replication']) {
    await expect(nav.getByRole('link', { name: hidden, exact: true })).toHaveCount(0);
  }
  await go(page, '/repositories');
  await expect(page.getByRole('table', { name: 'Repositories' })).toBeVisible();
  await expect(page.getByRole('link', { name: /New repository/ })).toHaveCount(0);
  await go(page, '/repositories/new');
  await expect(page.getByRole('main').getByRole('note')).toContainText('system:repos:admin');
  await expect(page.getByRole('form', { name: 'New repository' })).toHaveCount(0);
  await frame(page, 'reader-new-repo-1440');
});

test('the overview status follows its own alerts', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  await page.goto('/');
  const todo = page.getByRole('region', { name: 'To do' });
  await expect(todo.getByText('ghcr-proxy is blocked')).toBeVisible();
  const pill = page.getByRole('link', { name: 'ghcr-proxy upstream is blocked' });
  await expect(pill).toHaveClass(/status-pill--bad/);
  await expect(page.getByText('All systems normal')).toHaveCount(0);
  await frame(page, 'overview-status-1440');
});

test('repositories are cards on a phone, with totals that ignore the filter', async ({ page }) => {
  await page.setViewportSize(PHONE);
  await page.goto('/repositories');
  const cards = page.getByRole('list', { name: 'Repositories' });
  await expect(cards).toBeVisible();
  await expect(page.getByRole('table', { name: 'Repositories' })).toHaveCount(0);
  const first = cards.getByRole('listitem').first();
  for (const text of ['docker-all', 'Docker', 'Group', 'Online']) await expect(first).toContainText(text);
  const box = (await first.boundingBox())!;
  expect(box.x + box.width).toBeLessThanOrEqual(PHONE.width);
  await frame(page, 'repositories-cards-390');

  const totals = page.getByRole('region', { name: 'Repository counts' });
  const before = await totals.textContent();
  await page.goto('/repositories?q=zzz-none');
  await expect(page.getByText('No repositories match these filters.')).toBeVisible();
  await expect(totals).toHaveText(before!);
});

test('tap targets are at least 24 px on a phone', async ({ page }) => {
  await page.setViewportSize(PHONE);
  for (const path of ['/blob-stores/fs-local', '/repositories', '/repositories/new', '/tasks', '/cleanup?policy=docker-lean']) {
    await page.goto(path);
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible();
    await page.waitForLoadState('load');
    await expect.poll(() => smallTargets(page), { message: path }).toEqual([]);
  }
});

test('creating a repository confirms it and offers the next step', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  await page.goto('/repositories/new');
  await expect(page.locator('.summary__code pre').first()).toContainText('format: <format>');
  await page.getByRole('radiogroup', { name: /What will it store/ }).getByText('Raw', { exact: true }).click();
  await page.getByRole('radio', { name: /^Hosted/ }).check();
  await expect(page.locator('.summary__code pre').first()).toContainText('format: raw');
  await page.getByRole('button', { name: 'Create repository' }).click();
  await expect(page).toHaveURL(/\/repositories\/raw-hosted\/settings$/);
  const toast = page.getByRole('status').filter({ hasText: 'is ready' });
  await expect(toast).toContainText('raw-hosted is ready.');
  await frame(page, 'repo-created-1440');
  await toast.getByRole('button', { name: 'Connect a client' }).click();
  await expect(page.locator('#connect-client')).toBeFocused();
});

test('the sign-in card does not move when an error appears', async ({ page }) => {
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/login');
    const card = page.locator('.login-card');
    const before = (await card.boundingBox())!;
    await page.getByLabel('Username').fill('d.ismoilov');
    await page.getByLabel('Password').fill('wrong');
    await page.getByRole('button', { name: 'Sign in' }).click();
    await expect(page.getByRole('alert')).toHaveText('Wrong username or password.');
    const after = (await card.boundingBox())!;
    expect(after.y).toBe(before.y);
    await frame(page, `login-error-${w}`);
  }
});

test('cluster, account and advisory pages at both widths', async ({ page }) => {
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/cluster');
    await expect(page.getByRole('list', { name: 'Cluster nodes' }).getByRole('listitem')).toHaveCount(3);
    await expect(page.getByRole('region', { name: /Replication/ })).toContainText('Planned');
    await frame(page, `cluster-${w}`);

    await page.goto('/account');
    await expect(page.getByRole('region', { name: 'What you can do' })).toBeVisible();
    await frame(page, `account-${w}`);

    await page.goto('/vulnerabilities?finding=f_8');
    const drawer = page.getByRole('dialog');
    await expect(drawer.getByRole('region', { name: 'Summary' })).toBeVisible();
    await expect(drawer).not.toContainText('###');
    await frame(page, `finding-drawer-${w}`);
  }
});

test('versions carry a single v', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  await page.goto('/settings/system');
  await expect(page.getByRole('navigation', { name: 'Primary' })).toContainText(/nodes · v\d/);
  await expect(page.locator('body')).not.toContainText('vv');
});
