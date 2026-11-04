import { expect, test, type Page } from '@playwright/test';

const SIZES = [
  ['1440', { width: 1440, height: 900 }],
  ['390', { width: 390, height: 844 }],
] as const;
const shot = (name: string) => `test-results/screens/${name}.png`;

function collectErrors(page: Page) {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => m.type() === 'error' && errors.push(m.text()));
  return errors;
}

async function checkFrame(page: Page, name: string) {
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: shot(name), fullPage: true });
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow, `horizontal overflow on ${name}`).toBeLessThanOrEqual(0);
}

test('system status checklist at desktop and phone width', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/system/status');
    await expect(page.getByRole('heading', { level: 1, name: '1 check needs attention' })).toBeVisible();
    await expect(page.getByText('13 of 14 checks passing · 2 of 3 nodes up · v1.0.0')).toBeVisible();

    // The degraded check on hangar-2 starts open; a healthy one opens on click.
    const two = page.getByRole('region', { name: 'hangar-2' });
    await expect(two.getByRole('button', { name: /^Upstream proxies \(degraded\)/ })).toHaveAttribute('aria-expanded', 'true');
    const db = page.getByRole('region', { name: 'hangar-1' }).getByRole('button', { name: /^Database \(healthy\)/ });
    await db.click();
    await expect(db).toHaveAttribute('aria-expanded', 'true');
    await expect(page.getByText('db.hangar.example.com:5432/hangar').first()).toBeVisible();
    await checkFrame(page, `system-status-${w}`);
  }

  const live = page.getByRole('group', { name: 'Refresh status' });
  await live.getByRole('button', { name: 'Live · every 15 s – pause' }).click();
  await expect(live.getByRole('button', { name: 'Paused – resume' })).toBeVisible();
  await live.getByRole('button', { name: 'Refresh now' }).click();
  await expect(page.getByRole('heading', { level: 1, name: '1 check needs attention' })).toBeVisible();
  expect(errors).toEqual([]);
});

test('system logs still open from a status row', async ({ page }) => {
  const errors = collectErrors(page);
  await page.setViewportSize(SIZES[0][1]);
  await page.goto('/system/status');
  const db = page.getByRole('region', { name: 'hangar-2' }).getByRole('button', { name: /^Event bus \(healthy\)/ });
  await db.click();
  await page.getByRole('link', { name: 'Logs of hangar-2' }).click();
  await expect(page).toHaveURL(/\/system\/logs\?node=hangar-2/);
  await expect(page.getByRole('region', { name: 'Log records of hangar-2' })).toBeVisible();
  expect(errors).toEqual([]);
});
