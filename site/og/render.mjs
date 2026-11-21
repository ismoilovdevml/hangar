// Renders og/template.html to ../assets/og.png (1200x630), the image link previews show.
// Needs the fonts in ../assets/fonts, which build.mjs copies.
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { launch } from '../browser.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, '..', '..', 'assets', 'og.png');

const browser = await launch();
const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
await page.goto(pathToFileURL(join(here, 'template.html')).href);
await page.evaluate(() => document.fonts.ready);
const families = await page.evaluate(() => [...document.fonts].filter((f) => f.status === 'loaded').map((f) => f.family));
if (!families.some((f) => f.includes('Manrope'))) throw new Error('Manrope did not load; run "npm run build:local" first so assets/fonts exists');
await page.screenshot({ path: out, type: 'png' });
await browser.close();
console.log(`wrote ${out}`);
