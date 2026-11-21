// Starts a headless Chromium for og/render.mjs and check.mjs: Playwright's own build when
// it is installed (npx playwright install chromium), otherwise the local Google Chrome.
import { chromium } from 'playwright';

export async function launch() {
  try {
    return await chromium.launch();
  } catch (err) {
    if (!/Executable doesn't exist/.test(String(err))) throw err;
    return chromium.launch({ channel: 'chrome' });
  }
}
