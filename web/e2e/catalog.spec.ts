import { expect, test, type Page } from '@playwright/test';

const DESKTOP = { width: 1440, height: 900 };
const PHONE = { width: 390, height: 844 };
const SIZES = [
  ['1440', DESKTOP],
  ['390', PHONE],
] as const;
const shot = (name: string) => `test-results/screens/${name}.png`;
const NOW = new Date('2026-11-03T09:00:00Z');

function collectErrors(page: Page) {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  // Dependents and findings answer 501 on purpose (not implemented server-side); the UI hides those tabs.
  page.on('console', (m) => m.type() === 'error' && !/status of 501/.test(m.text()) && errors.push(m.text()));
  return errors;
}

async function noOverflow(page: Page, width: number) {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow, `horizontal overflow at ${width}px`).toBeLessThanOrEqual(0);
}

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(NOW);
});

test('package browser shows a package with install commands, versions and files', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/browse?repo=docker-prod&name=payments-api');
    await expect(page.getByRole('heading', { level: 1, name: 'payments-api' })).toBeVisible();
    await expect(page.getByText('docker pull cr.acme.io/docker-prod/payments-api:2.14.0')).toBeVisible();
    await expect(page.getByText('distroless/java21')).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(`browse-${w}`), fullPage: true });
    await noOverflow(page, size.width);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByRole('tab', { name: /Versions/ }).click();
  await expect(page).toHaveURL(/tab=versions/);
  await expect(page.getByRole('table', { name: 'Versions' }).getByRole('link', { name: '2.13.1' })).toBeVisible();
  await page.getByRole('tab', { name: 'Layers' }).click();
  await expect(page.getByRole('table', { name: 'Layers' })).toBeVisible();
  await page.screenshot({ path: shot('browse-layers-1440'), fullPage: true });
  expect(errors).toEqual([]);
});

test('deleting a version asks first', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  await page.goto('/browse?repo=npm-internal&ns=@acme&name=ui-kit&version=5.3.0');
  await expect(page.getByRole('heading', { level: 1, name: '@acme/ui-kit' })).toBeVisible();
  await page.getByRole('button', { name: 'Delete version', exact: true }).first().click();
  const dialog = page.getByRole('alertdialog', { name: 'Delete @acme/ui-kit 5.3.0?' });
  await expect(dialog).toBeVisible();
  await page.screenshot({ path: shot('browse-delete-1440') });
  await dialog.getByRole('button', { name: 'Delete version' }).click();
  await expect(dialog).toBeHidden();
  await expect(page).not.toHaveURL(/version=5\.3\.0/);
  await expect(page.getByRole('treeitem', { name: /5\.3\.0/ })).toHaveCount(0);
});

test('search with qualifiers, facets and keyboard', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/search?q=a');
    await expect(page.getByRole('list', { name: 'Search results' }).getByRole('listitem').first()).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(`search-${w}`), fullPage: true });
    await noOverflow(page, size.width);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByRole('complementary', { name: 'Filters' }).getByRole('checkbox', { name: /Maven/ }).check();
  await expect(page).toHaveURL(/q=a\+format%3Amaven/);
  const box = page.getByRole('combobox', { name: 'Search query' });
  await box.fill('');
  await box.pressSequentially('repo:docker-p');
  await expect(page.getByRole('option', { name: /repo:docker-prod/ })).toBeVisible();
  await page.screenshot({ path: shot('search-suggest-1440') });
  await box.press('Enter');
  await box.press('Enter');
  await expect(page).toHaveURL(/q=repo%3Adocker-prod/);
  // Wait for the new results before moving focus into them.
  await expect(page.getByRole('list', { name: 'Search results' }).getByRole('link').first()).toHaveText(
    /auth-gateway|payments-api/,
  );
  await box.press('ArrowDown');
  await expect(page.getByRole('list', { name: 'Search results' }).getByRole('link').first()).toBeFocused();
  await page.keyboard.press('Enter');
  await expect(page.getByRole('heading', { level: 1, name: /auth-gateway|payments-api/ })).toBeVisible();

  await page.goto('/search?q=nothing-like-this');
  await expect(page.getByText('Nothing matches “nothing-like-this”.')).toBeVisible();
  expect(errors).toEqual([]);
});

test('blob stores: totals, cards, create and detail', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/blob-stores');
    await expect(page.getByRole('article', { name: 's3-eu-primary' })).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(`blobstores-${w}`), fullPage: true });
    await noOverflow(page, size.width);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByRole('button', { name: 'Add blob store' }).click();
  const form = page.getByRole('form', { name: 'Add a blob store' });
  await form.getByLabel('Name').fill('nvme-two');
  await form.getByLabel('Directory on the server').fill('/srv/hangar/blobs');
  await page.screenshot({ path: shot('blobstore-create-1440'), fullPage: true });
  await form.getByRole('button', { name: 'Add blob store' }).click();
  await expect(page).toHaveURL(/\/blob-stores\/nvme-two$/);
  await expect(page.getByRole('heading', { level: 1, name: /nvme-two/ })).toBeVisible();

  await page.goto('/blob-stores/fs-local');
  await expect(page.getByRole('table', { name: 'Repositories using fs-local' }).getByRole('link', { name: 'maven-releases' })).toBeVisible();
  await page.screenshot({ path: shot('blobstore-detail-1440'), fullPage: true });
  expect(errors).toEqual([]);
});

test('repository settings: edit, offline switch and typed delete', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/repositories/npmjs-proxy/settings');
    await expect(page.getByLabel('Remote URL')).toHaveValue('https://registry.npmjs.org');
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(`repo-settings-${w}`), fullPage: true });
    await noOverflow(page, size.width);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByLabel('Metadata cached for').fill('1h');
  await page.getByRole('button', { name: 'Save changes' }).click();
  await expect(page.getByRole('button', { name: 'Saved' })).toBeVisible();
  await page.getByRole('button', { name: 'Take offline' }).click();
  await expect(page.getByRole('button', { name: 'Bring online' })).toBeVisible();

  await page.goto('/repositories/raw-legacy/settings');
  await page.getByRole('button', { name: 'Delete raw-legacy' }).click();
  const dialog = page.getByRole('alertdialog', { name: 'Delete raw-legacy?' });
  await expect(dialog.getByRole('button', { name: 'Delete repository' })).toBeDisabled();
  await dialog.getByLabel(/to confirm/).fill('raw-legacy');
  await page.screenshot({ path: shot('repo-delete-1440') });
  await dialog.getByRole('button', { name: 'Delete repository' }).click();
  await expect(page).toHaveURL(/\/repositories$/);
  await expect(page.getByRole('link', { name: 'raw-legacy' })).toBeHidden();
  expect(errors).toEqual([]);
});
