import { defineConfig, devices } from '@playwright/test';

const PORT = 4173;

/**
 * Smoke tests run against `vite preview` of a build made with VITE_API_MOCKS=true,
 * so no backend is needed. Screenshots land in test-results/ (git-ignored).
 */
export default defineConfig({
  testDir: './e2e',
  outputDir: './test-results/artifacts',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]],
  use: {
    baseURL: `http://localhost:${PORT}`,
    timezoneId: 'UTC',
    locale: 'en-GB',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `npx vite build --outDir dist-mock --emptyOutDir && npx vite preview --outDir dist-mock --port ${PORT} --strictPort`,
    env: { VITE_API_MOCKS: 'true' },
    url: `http://localhost:${PORT}/login`,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
