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

test('cleanup policy editor previews before saving at desktop and phone width', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/cleanup?policy=docker-lean');
    await expect(page.getByRole('heading', { level: 2, name: 'docker-lean' })).toBeVisible();
    await expect(page.getByLabel('Days since published')).toHaveValue('90');
    if (w === '1440') {
      // Full-page shots stretch the sticky sidebar; keep a plain viewport shot too.
      await page.evaluate(() => document.fonts.ready);
      await page.screenshot({ path: shot('cleanup-viewport-1440') });
    }
    await page.getByRole('button', { name: 'Preview', exact: true }).click();
    await expect(page.getByRole('heading', { name: /would be freed/ })).toBeVisible();
    await checkFrame(page, `cleanup-${w}`);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByRole('button', { name: /Run now/ }).click();
  const dialog = page.getByRole('alertdialog', { name: 'Run docker-lean now?' });
  await expect(dialog).toBeVisible();
  await page.screenshot({ path: shot('cleanup-run-confirm-1440') });
  await page.keyboard.press('Escape');
  await expect(dialog).toBeHidden();
  expect(errors).toEqual([]);
});

test('new cleanup policy shows the schedule in plain words', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  await page.goto('/cleanup?create=true');
  await page.getByLabel('How often').click();
  await page.getByRole('option', { name: 'Every week' }).click();
  await page.getByLabel('Name').fill('weekly-prune');
  await expect(page.getByText(/Every Sunday at 03:00 UTC/).first()).toBeVisible();
});

test('scheduled tasks run with live progress at desktop and phone width', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/tasks');
    await expect(page.getByRole('region', { name: /Running now: Rebuild search index/ })).toBeVisible();
    await expect(page.getByRole('link', { name: 'Backup database' })).toBeVisible();
    await checkFrame(page, `tasks-${w}`);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByRole('button', { name: 'Run Backup database now' }).click();
  const banner = page.getByRole('region', { name: 'Running now: Backup database' });
  await expect(banner).toBeVisible();
  await expect(banner.getByRole('meter')).not.toHaveAttribute('aria-valuenow', '0', { timeout: 5_000 });
  await expect(banner).toBeHidden({ timeout: 15_000 });
  await page.getByRole('link', { name: 'Backup database' }).click();
  await expect(page.getByRole('heading', { level: 1, name: 'Backup database' })).toBeVisible();
  await expect(page.getByText('done: Completed')).toBeVisible();
  expect(errors).toEqual([]);
});

test('task run detail shows the log at desktop and phone width', async ({ page }) => {
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/tasks/t_reindex');
    await expect(page.getByText('maven-central: 12 components without metadata, skipped')).toBeVisible();
    await checkFrame(page, `task-detail-${w}`);
  }
});

test('audit log filters through the URL at desktop and phone width', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/audit');
    await expect(page.getByRole('cell', { name: 'cleanup.policy.update' }).first()).toBeVisible();
    await page.getByRole('button', { name: 'Show details of event 5000' }).click();
    await expect(page.getByText('criteria.older_than_days')).toBeVisible();
    await checkFrame(page, `audit-${w}`);
  }
  await page.setViewportSize(DESKTOP);
  await page.getByLabel('Outcome').click();
  await page.getByRole('option', { name: 'Denied' }).click();
  await expect(page).toHaveURL(/outcome=denied/);
  await expect(page.getByRole('cell', { name: 'Success' })).toHaveCount(0);
  await expect(page.getByRole('link', { name: 'Export CSV' })).toHaveAttribute('href', /outcome=denied/);
  expect(errors).toEqual([]);
});

test('system page at desktop and phone width', async ({ page }) => {
  const errors = collectErrors(page);
  for (const [w, size] of SIZES) {
    await page.setViewportSize(size);
    await page.goto('/settings');
    await expect(page.getByText('Operational')).toBeVisible();
    await expect(page.getByRole('cell', { name: 'hangar-1' })).toBeVisible();
    await expect(page.getByRole('region', { name: 'database settings' })).toBeVisible();
    await checkFrame(page, `system-${w}`);
  }
  expect(errors).toEqual([]);
});

test('audit log and system are in the navigation', async ({ page }) => {
  await page.setViewportSize(DESKTOP);
  const nav = page.getByRole('navigation', { name: 'Primary' });
  for (const [path, title, label] of [
    ['/audit', 'Audit log', 'Audit log'],
    ['/settings', 'System', 'System'],
    ['/tasks/t_backup', 'Backup database', 'Scheduled tasks'],
  ] as const) {
    await page.goto(path);
    await expect(page.getByRole('heading', { level: 1, name: title })).toBeVisible();
    await expect(nav.getByRole('link', { name: label, exact: true })).toHaveAttribute('aria-current', 'page');
  }
});
