import { expect, test, type Page } from '@playwright/test';

const SIZES = [
  ['1440', { width: 1440, height: 900 }],
  ['390', { width: 390, height: 844 }],
] as const;
const shot = (name: string) => `test-results/screens/${name}.png`;
const NOW = new Date('2026-11-03T09:00:00Z');

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

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

test('authentication opens on the provider in use and switches presets', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/settings/authentication');
    const form = page.getByRole('form', { name: 'Keycloak' });
    await expect(form).toBeVisible();
    await expect(page.getByRole('button', { name: /^Keycloak/ })).toHaveAttribute('aria-pressed', 'true');
    await expect(form.getByText('https://hangar.acme.example/api/v1/auth/oidc/callback')).toBeVisible();
    await checkFrame(page, `auth-keycloak-${w}`);

    await page.getByRole('button', { name: /^Okta/ }).click();
    const okta = page.getByRole('form', { name: 'Okta' });
    await expect(okta.getByRole('button', { name: 'Switch to Okta' })).toBeVisible();
    await expect(okta.getByRole('list', { name: 'Preset values' })).toContainText('openid profile email groups');
    await checkFrame(page, `auth-okta-${w}`);
  }
  expect(errors).toEqual([]);
});

test('proxies list, short form and inline test', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/settings/proxies');
    await expect(page.getByRole('listitem', { name: 'corp-proxy' })).toContainText('Platform default');
    await checkFrame(page, `proxies-list-${w}`);

    await page.getByRole('button', { name: 'Add another proxy' }).click();
    const form = page.getByRole('form', { name: 'New proxy' });
    await form.getByLabel('Proxy address').fill('http://proxy.example.com:3128');
    await expect(form.getByText('proxy.example.com', { exact: true })).toBeVisible();
    await form.getByRole('button', { name: 'Test' }).click();
    await expect(form.getByText(/api\.nuget\.org answered 200/)).toBeVisible();
    await checkFrame(page, `proxies-form-${w}`);
  }
  expect(errors).toEqual([]);
});

test('routing rules panel on a group and a proxy', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/repositories/npm-all/settings');
    const panel = page.getByRole('region', { name: 'Routing rules' });
    await expect(panel.getByText(/dependency confusion/)).toBeVisible();
    await panel.getByLabel('Names only npm-internal serves').fill('@acme/*');
    await panel.getByRole('button', { name: 'Save routing rules' }).click();
    await expect(panel.getByRole('button', { name: 'Saved' })).toBeVisible();
    await panel.getByLabel('Test a name').fill('@acme/ui');
    await panel.getByRole('button', { name: 'Test', exact: true }).click();
    await expect(panel.getByRole('status', { name: 'Routing test result' })).toContainText('Served by npm-internal');
    await panel.scrollIntoViewIfNeeded();
    await checkFrame(page, `routing-group-${w}`);

    await page.goto('/repositories/nuget-proxy/settings');
    const proxyPanel = page.getByRole('region', { name: 'Routing rules' });
    await expect(proxyPanel.getByLabel('Block')).toBeVisible();
    await checkFrame(page, `routing-proxy-${w}`);
  }
  expect(errors).toEqual([]);
});
