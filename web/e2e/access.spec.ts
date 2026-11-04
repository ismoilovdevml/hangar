import { expect, test, type Page } from '@playwright/test';

const DESKTOP = { width: 1440, height: 900 };
const PHONE = { width: 390, height: 844 };
const shot = (name: string) => `test-results/screens/${name}.png`;

async function collectErrors(page: Page) {
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => m.type() === 'error' && errors.push(m.text()));
  return errors;
}

async function noOverflow(page: Page, width: number) {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow, `horizontal overflow at ${width}px`).toBeLessThanOrEqual(0);
}

test('access & audit renders members, roles, tokens and audit at desktop and phone width', async ({ page }) => {
  const errors = await collectErrors(page);
  for (const [name, size] of [['access-1440', DESKTOP], ['access-390', PHONE]] as const) {
    await page.setViewportSize(size);
    await page.goto('/access');
    await expect(page.getByRole('heading', { level: 1, name: 'Access & audit' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Open Jasur Karimov' })).toBeVisible();
    await expect(page.getByRole('region', { name: 'Sign-in providers' })).toBeVisible();
    await page.evaluate(() => document.fonts.ready);
    await page.screenshot({ path: shot(name), fullPage: true });
    await noOverflow(page, size.width);

    for (const [tab, check] of [
      ['tokens', 'gitea deploy'],
      ['roles', 'CI deployer'],
      ['audit', 'created b.yusupov'],
    ] as const) {
      await page.goto(`/access?tab=${tab}`);
      await expect(page.getByText(check).first()).toBeVisible();
      await page.screenshot({ path: shot(`access-${tab}-${size.width}`), fullPage: true });
      await noOverflow(page, size.width);
    }
  }
  expect(errors).toEqual([]);
});

test('member drawer and create-token dialog work on a phone', async ({ page }) => {
  const errors = await collectErrors(page);
  await page.setViewportSize(PHONE);
  await page.goto('/access?user=u_02');
  const drawer = page.getByRole('dialog', { name: 'Jasur Karimov' });
  await expect(drawer).toBeVisible();
  await drawer.getByRole('tab', { name: 'Permissions' }).click();
  await expect(drawer.getByText('docker-prod')).toBeVisible();
  await page.screenshot({ path: shot('access-drawer-390') });
  await page.keyboard.press('Escape');
  await expect(drawer).toBeHidden();

  await page.goto('/account');
  await expect(page.getByRole('heading', { level: 1, name: 'My account' })).toBeVisible();
  await noOverflow(page, PHONE.width);
  await page.screenshot({ path: shot('account-390'), fullPage: true });
  await page.getByRole('region', { name: 'Personal access tokens' }).getByRole('button', { name: 'New token' }).click();
  const dialog = page.getByRole('dialog', { name: 'New personal access token' });
  await dialog.getByLabel('Name').fill('phone test');
  await dialog.getByText('Matching a pattern').click();
  await dialog.getByRole('textbox', { name: 'Pattern' }).fill('docker-*');
  await page.screenshot({ path: shot('token-create-390') });
  await dialog.getByRole('button', { name: 'Create token' }).click();
  const secret = page.getByRole('dialog', { name: 'Copy your token now' });
  await expect(secret.getByTestId('token-secret')).toHaveText(/^hgr_/);
  await page.screenshot({ path: shot('token-secret-390') });
  await secret.getByText('I have stored the token somewhere safe').click();
  await secret.getByRole('button', { name: 'Done' }).click();
  await expect(page.getByRole('table', { name: 'Your access tokens' }).getByText('phone test')).toBeVisible();
  expect(errors).toEqual([]);
});

test('my account at desktop width', async ({ page }) => {
  const errors = await collectErrors(page);
  await page.setViewportSize(DESKTOP);
  await page.goto('/account');
  await expect(page.getByRole('region', { name: 'Signed-in devices' }).getByText('This session')).toBeVisible();
  await page.evaluate(() => document.fonts.ready);
  await page.screenshot({ path: shot('account-1440'), fullPage: true });
  await noOverflow(page, DESKTOP.width);
  expect(errors).toEqual([]);
});
