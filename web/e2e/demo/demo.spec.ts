import { expect, test, type Page } from '@playwright/test';

const DEMO = '/hangar/demo/';

function collectProblems(page: Page) {
  const problems: string[] = [];
  page.on('pageerror', (e) => problems.push(e.message));
  page.on('console', (m) => m.type() === 'error' && problems.push(m.text()));
  page.on('requestfailed', (r) => problems.push(`${r.url()}: ${r.failure()?.errorText}`));
  page.on('response', (r) => r.status() >= 400 && problems.push(`${r.status()} ${r.url()}`));
  return problems;
}

test('the demo boots signed in and navigates', async ({ page }) => {
  const problems = collectProblems(page);
  await page.goto(DEMO);
  await expect(page.getByRole('heading', { level: 1, name: 'What do you need today?' })).toBeVisible();
  await expect(page.getByRole('note')).toContainText('Sample data, changes reset on reload');

  const nav = page.getByRole('navigation', { name: 'Primary' });
  for (const [link, heading, path] of [
    ['Repositories', 'Repositories', 'repositories'],
    ['Vulnerabilities', 'Vulnerabilities', 'vulnerabilities'],
    ['Scheduled tasks', 'Scheduled tasks', 'tasks'],
  ] as const) {
    await nav.getByRole('link', { name: link }).click();
    await expect(page).toHaveURL(`${DEMO}${path}`);
    await expect(page.getByRole('heading', { level: 1, name: heading })).toBeVisible();
  }
  expect(problems).toEqual([]);
});

test('hash links open a page and the router keeps the base path', async ({ page }) => {
  const problems = collectProblems(page);
  await page.goto(`${DEMO}#/access?tab=tokens`);
  await expect(page).toHaveURL(`${DEMO}access?tab=tokens`);
  await expect(page.getByRole('heading', { level: 1, name: 'Access & audit' })).toBeVisible();
  await expect(page.getByRole('navigation', { name: 'Access sections' }).getByRole('link', { name: 'Access tokens' })).toHaveAttribute(
    'aria-current',
    'page',
  );
  expect(problems).toEqual([]);
});
