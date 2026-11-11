import { defineConfig, devices } from '@playwright/test';

// E2E_PORT lets several checkouts run the suite side by side.
const PORT = Number(process.env.E2E_PORT ?? 4174);

/**
 * Smoke test of the public demo (`npm run build:demo`): the mocked UI built
 * for, and served from, the project website's /hangar/demo/ path.
 */
export default defineConfig({
  testDir: './e2e/demo',
  outputDir: './test-results/demo',
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: [['list']],
  use: {
    baseURL: `http://localhost:${PORT}`,
    timezoneId: 'UTC',
    locale: 'en-GB',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } } }],
  webServer: {
    command: `npx vite build --mode demo && npx vite preview --mode demo --port ${PORT} --strictPort`,
    url: `http://localhost:${PORT}/hangar/demo/`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
