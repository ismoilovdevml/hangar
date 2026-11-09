import { defineConfig, devices } from '@playwright/test';

/**
 * Smoke tests against a real Hangar (web UI embedded, `make build-embed`),
 * not the mocks: e2e/live. The server must already run.
 *
 *   E2E_LIVE_URL       server, e.g. http://localhost:8080
 *   E2E_LIVE_USER      an administrator who can sign in with a password
 *   E2E_LIVE_PASSWORD  (that no longer needs to be changed)
 *   E2E_LIVE_TOKEN     an access token of an administrator (repo:*:admin,
 *                      system:*:admin) for setup and clean-up through the API
 *
 * The tests create a raw repository, a token and a reader user with a
 * per-run suffix and delete the repository and the user afterwards.
 */
const BASE = process.env.E2E_LIVE_URL ?? 'http://localhost:8080';

export default defineConfig({
  testDir: './e2e/live',
  outputDir: './test-results/live-artifacts',
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'playwright-report/live' }]],
  use: {
    baseURL: BASE,
    timezoneId: 'UTC',
    locale: 'en-GB',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
