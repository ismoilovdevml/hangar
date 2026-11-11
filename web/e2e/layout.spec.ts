import { expect, test, type Page } from '@playwright/test';

const NOW = new Date('2026-11-03T09:00:00Z');
const shot = (name: string) => `test-results/screens/${name}.png`;

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

/** Header cells of visible tables that end past the right edge of their scroll box. */
async function clippedHeaders(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll<HTMLElement>('.table-scroll')]
      .filter((box) => box.offsetParent)
      .flatMap((box) => {
        const right = box.getBoundingClientRect().right;
        return [...box.querySelectorAll<HTMLElement>('thead th')]
          .filter((th) => th.offsetParent && th.getBoundingClientRect().right > right + 1)
          .map((th) => `${box.getAttribute('aria-label')}: ${th.textContent?.trim() || '(actions)'}`);
      }),
  );
}

/** Whether the current page's sidebar entry lies inside the visible part of the list. */
async function activeEntryInView(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const nav = document.querySelector('.sidebar__nav')!;
    const item = nav.querySelector('.nav-item.is-active')!;
    const box = nav.getBoundingClientRect();
    const at = item.getBoundingClientRect();
    return at.top >= box.top && at.bottom <= box.bottom;
  });
}

test('every sidebar entry stays reachable on a laptop screen', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.goto('/repositories');
  const nav = page.getByRole('navigation', { name: 'Primary' });
  await expect(nav.getByRole('link', { name: 'Repositories' })).toHaveAttribute('aria-current', 'page');
  // The list runs past the card: the bottom fades and a button scrolls on.
  await expect(page.locator('.sidebar__nav')).toHaveAttribute('data-more-below', '');
  await expect(page.locator('.sidebar__more')).toHaveCSS('opacity', '1');
  for (const label of ['System logs', 'Settings', 'Authentication', 'Proxies']) {
    await expect(nav.getByRole('link', { name: label, exact: true })).toHaveCount(1);
  }

  // Pages at the bottom of the list scroll their entry into view.
  for (const path of ['/system/logs', '/settings/proxies', '/account']) {
    await page.goto(path);
    await expect(page.locator('.nav-item.is-active')).toBeVisible();
    await expect.poll(() => activeEntryInView(page), { message: path }).toBe(true);
  }
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: shot('layout-sidebar-1280') });

  // Folding a section is remembered.
  await page.getByRole('button', { name: 'Storage' }).click();
  await expect(nav.getByRole('link', { name: 'Blob stores' })).toBeHidden();
  await page.reload();
  await expect(page.getByRole('button', { name: 'Storage' })).toHaveAttribute('aria-expanded', 'false');
  await page.getByRole('button', { name: 'Storage' }).click();
  await expect(nav.getByRole('link', { name: 'Blob stores' })).toBeVisible();
});

test('wide tables keep their key columns on screen at 1024 px', async ({ page }) => {
  await page.setViewportSize({ width: 1024, height: 768 });
  for (const [path, keep] of [
    ['/repositories', ['Status']],
    ['/tasks', ['Last run']],
    ['/vulnerabilities', ['Findings', 'Action']],
    ['/access?tab=tokens', ['Expires']],
    ['/audit', ['Outcome']],
  ] as const) {
    await page.goto(path);
    await expect(page.locator('.table-scroll tbody tr').first()).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    expect(await clippedHeaders(page), path).toEqual([]);
    for (const name of keep) await expect(page.getByRole('columnheader', { name, exact: true }).first(), `${path} ${name}`).toBeVisible();
  }
  await page.goto('/repositories');
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: shot('layout-repositories-1024') });
});

test('tables turn into cards on a phone', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto('/tasks');
  const run = page.getByRole('button', { name: /^Run / }).first();
  await expect(run).toBeVisible();
  const box = await run.boundingBox();
  expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  const overflow = await page.locator('.table-scroll').first().evaluate((e) => e.scrollWidth - e.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
  await page.screenshot({ path: shot('layout-tasks-390'), fullPage: true });
});

test('a package gets the width when the tree folds away', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto('/browse?repo=docker-prod&name=payments-api');
  const tabs = page.getByRole('tablist', { name: 'Package details' });
  await expect(tabs).toBeVisible();
  // No tab label is cut off.
  expect(await tabs.evaluate((e) => e.scrollWidth - e.clientWidth)).toBeLessThanOrEqual(0);
  const width = () => page.locator('.pkg-layout__main').evaluate((e) => e.getBoundingClientRect().width);
  expect(await width()).toBeGreaterThan(700);

  await page.getByRole('button', { name: 'Hide the repository tree' }).click();
  await expect(page.getByRole('navigation', { name: 'Repositories and packages' })).toBeHidden();
  expect(await width()).toBeGreaterThan(700);
  await page.reload();
  await expect(page.getByRole('button', { name: 'Show the repository tree' })).toHaveAttribute('aria-expanded', 'false');
  await page.getByRole('button', { name: 'Show the repository tree' }).click();
  await expect(page.getByRole('navigation', { name: 'Repositories and packages' })).toBeVisible();
});
