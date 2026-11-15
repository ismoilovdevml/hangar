/**
 * Screenshots of the demo build for the project website's UI guides.
 *
 *   npm run site:screenshots -- <site-dir> [shot-name ...]
 *
 * Builds the demo (`vite build --mode demo`), serves dist-demo/ on a local
 * port, walks through the pages below in Chromium and writes JPEGs (1440x900
 * unless a shot says otherwise) to <site-dir>/assets/img/ui/. Shot names after the directory take only
 * those. Run on demand after UI changes; it is not part of CI.
 */
import { readFile } from 'node:fs/promises';
import { mkdirSync } from 'node:fs';
import { createServer } from 'node:http';
import { dirname, extname, join, normalize, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium, type Locator, type Page } from '@playwright/test';

const WEB = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const DIST = join(WEB, 'dist-demo');
const PREFIX = '/hangar/demo/';
const PORT = Number(process.env.SCREENSHOT_PORT ?? 4175);
const ORIGIN = `http://localhost:${PORT}`;
const BASE = `${ORIGIN}${PREFIX.replace(/\/$/, '')}`;
/** The sample data's server address, shown where the UI derives an address from its own origin. */
const SAMPLE_HOST = 'pkg.acme.io';
const VIEWPORT = { width: 1440, height: 900 };
const QUALITY = 78;

interface Shot {
  name: string;
  /** Router path, as in the address bar after /hangar/demo. */
  path: string;
  /** Clicks and typing after the page loaded. */
  act?: (page: Page) => Promise<void>;
  /** Defaults to 1440x900. */
  viewport?: { width: number; height: number };
}

const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const radio = (page: Page, name: string | RegExp) => page.getByRole('radio', { name }).first().check({ force: true });
const button = (scope: Page | Locator, name: string | RegExp) => scope.getByRole('button', { name, exact: typeof name === 'string' }).first().click();
const dialog = (page: Page) => page.getByRole('dialog').first();
/** Picks `option` in the listbox of the combobox labelled `name`. */
async function choose(page: Page, name: string, option: string) {
  await page.getByRole('combobox', { name, exact: true }).first().click();
  await page.getByRole('option', { name: new RegExp(`^${escape(option)}\\b`) }).first().click();
}
/** Scrolls the page so `target` sits near the top of the content area. */
const scrollTo = (target: Locator) =>
  target.evaluate((el) => {
    const scroller = el.closest('.app__content') ?? document.scrollingElement!;
    const top = el.getBoundingClientRect().top - scroller.getBoundingClientRect().top + scroller.scrollTop - 24;
    scroller.scrollTo({ top });
    window.scrollTo({ top: el.getBoundingClientRect().top + window.scrollY - 96 });
  });

const SHOTS: Shot[] = [
  // Overview
  { name: 'overview', path: '/' },
  { name: 'overview-phone', path: '/', viewport: { width: 390, height: 844 } },
  {
    name: 'command-palette',
    path: '/',
    act: async (p) => {
      await p.keyboard.press('ControlOrMeta+k');
      await p.keyboard.type('npm');
    },
  },
  { name: 'connect-client', path: '/', act: (p) => p.getByText('Connect a client', { exact: true }).first().click() },

  // Repositories
  { name: 'repositories', path: '/repositories' },
  {
    name: 'repo-new-hosted',
    path: '/repositories/new',
    act: async (p) => {
      await radio(p, /^Docker/);
      await radio(p, /^Hosted/);
      await p.getByRole('textbox', { name: 'Name', exact: true }).fill('docker-hosted');
    },
  },
  {
    name: 'repo-new-proxy',
    path: '/repositories/new',
    act: async (p) => {
      await radio(p, /^npm/);
      await radio(p, /^Proxy/);
      await scrollTo(p.getByRole('heading', { name: 'Step 3: Settings' }));
    },
  },
  {
    name: 'repo-new-group',
    path: '/repositories/new',
    act: async (p) => {
      await radio(p, /^npm/);
      await radio(p, /^Group/);
      await p.getByRole('textbox', { name: 'Name', exact: true }).fill('npm-group');
      await scrollTo(p.getByRole('heading', { name: 'Step 3: Settings' }));
    },
  },
  { name: 'repo-settings', path: '/repositories/npm-all/settings' },
  {
    name: 'routing-rules',
    path: '/repositories/npm-all/settings',
    act: async (p) => {
      await p.getByRole('textbox', { name: 'Names only npm-internal serves' }).fill('@acme/*');
      await scrollTo(p.getByRole('heading', { name: 'Routing rules' }));
    },
  },
  { name: 'cleanup', path: '/cleanup' },
  {
    name: 'cleanup-new',
    path: '/cleanup?create=true',
    act: async (p) => {
      await p.getByRole('textbox', { name: 'Name', exact: true }).fill('docker-dev-weekly');
      await choose(p, 'Add repository', 'docker-dev');
      await button(p, 'Preview');
    },
  },

  // Packages and search
  { name: 'browse-files', path: '/browse?repo=docker-prod&files=%2F' },
  { name: 'browse-package', path: '/browse?repo=npm-internal&ns=%40acme&name=ui-kit&version=5.3.1' },
  {
    name: 'browse-versions',
    path: '/browse?repo=npm-internal&ns=%40acme&name=ui-kit&version=5.3.1',
    act: (p) => p.getByRole('tab', { name: /^Versions/ }).click(),
  },
  { name: 'search', path: '/search?q=format%3Anpm' },

  // Vulnerabilities
  { name: 'vulnerabilities', path: '/vulnerabilities' },
  { name: 'vulnerability-details', path: '/vulnerabilities', act: (p) => button(p, 'Show the 2 findings of System.Security.Cryptography.Xml') },
  { name: 'vulnerability-waive', path: '/vulnerabilities', act: (p) => button(p, 'Waive @acme/ui-kit') },

  // Access
  { name: 'access-members', path: '/access' },
  { name: 'access-add-member', path: '/access', act: (p) => button(p, 'Add member') },
  { name: 'access-member', path: '/access', act: (p) => p.getByText('Jasur Karimov', { exact: true }).first().click() },
  { name: 'access-new-role', path: '/access?tab=roles', act: (p) => button(p, 'New role') },
  {
    name: 'access-new-token',
    path: '/access?tab=tokens',
    act: async (p) => {
      await button(p, 'New token');
      await dialog(p).getByRole('textbox', { name: 'Name', exact: true }).fill('gitlab-ci');
    },
  },
  {
    name: 'access-token-created',
    path: '/access?tab=tokens',
    act: async (p) => {
      await button(p, 'New token');
      await dialog(p).getByRole('textbox', { name: 'Name', exact: true }).fill('gitlab-ci');
      await button(dialog(p), 'Create token');
      await p.getByRole('dialog', { name: 'Copy your token now' }).waitFor();
    },
  },

  // Tasks
  { name: 'tasks', path: '/tasks' },
  {
    name: 'task-import',
    path: '/tasks/new',
    act: async (p) => {
      await p.getByRole('textbox', { name: 'Task name' }).fill('import-npm');
      await choose(p, 'Hosted repository', 'npm-internal');
      await p.getByRole('textbox', { name: 'Source URL' }).fill('https://npm.old-registry.example.com/');
    },
  },
  {
    name: 'task-prewarm',
    path: '/tasks/new?kind=prewarm',
    act: async (p) => {
      await p.getByRole('textbox', { name: 'Task name' }).fill('prewarm-npm');
      await choose(p, 'Proxy or group repository', 'npmjs-proxy');
      await p.getByRole('textbox', { name: 'Coordinates' }).fill('react@18.3.1\nlodash@4.17.21\ntypescript@5.9.3');
    },
  },
  { name: 'task-detail', path: '/tasks/t_cleanup_docker' },

  // Blob stores
  { name: 'blob-stores', path: '/blob-stores' },
  {
    name: 'blob-store-add',
    path: '/blob-stores?add=true',
    act: async (p) => {
      await radio(p, 'S3 / MinIO');
      await p.getByRole('textbox', { name: 'Name', exact: true }).fill('s3-backup');
    },
  },
  { name: 'blob-store-detail', path: '/blob-stores/s3-eu-primary' },

  // System
  { name: 'system-status', path: '/system/status' },
  { name: 'system-logs', path: '/system/logs' },
  { name: 'cluster', path: '/cluster' },
  { name: 'audit', path: '/audit' },

  // Settings
  { name: 'settings', path: '/settings' },
  { name: 'authentication', path: '/settings/authentication' },
  { name: 'proxies', path: '/settings/proxies' },
  { name: 'proxies-add', path: '/settings/proxies', act: (p) => button(p, 'Add another proxy') },
  { name: 'account', path: '/account' },
  { name: 'sign-in', path: '/login' },
];

const TYPES: Record<string, string> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript',
  '.css': 'text/css',
  '.svg': 'image/svg+xml',
  '.jpg': 'image/jpeg',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
};

/** Serves dist-demo/ under PREFIX, as the website does. */
function serveDemo() {
  const server = createServer((req, res) => {
    const path = decodeURIComponent(new URL(req.url ?? '/', 'http://x').pathname);
    if (!path.startsWith(PREFIX)) return void res.writeHead(404).end();
    const file = normalize(join(DIST, path.slice(PREFIX.length) || 'index.html'));
    if (!file.startsWith(DIST)) return void res.writeHead(403).end();
    readFile(file).then(
      (body) => res.writeHead(200, { 'Content-Type': TYPES[extname(file)] ?? 'application/octet-stream' }).end(body),
      () => res.writeHead(404).end(),
    );
  });
  return new Promise<typeof server>((ok, fail) => server.once('error', fail).listen(PORT, '127.0.0.1', () => ok(server)));
}

/** Replaces the local server's address in visible text with the sample installation's. */
function retouch(page: Page) {
  return page.evaluate(
    ([origin, host]) => {
      const local = origin.replace(/^http:\/\//, '');
      const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        if (n.nodeValue?.includes(local)) n.nodeValue = n.nodeValue.replaceAll(origin, `https://${host}`).replaceAll(local, host);
      }
    },
    [ORIGIN, SAMPLE_HOST] as const,
  );
}

async function settle(page: Page) {
  await page.evaluate(() => document.fonts.ready);
  // Queries answer from the in-page mock within a few milliseconds; give charts and fades time to finish.
  await page.waitForTimeout(700);
}

async function main() {
  const [siteDir, ...only] = process.argv.slice(2);
  if (!siteDir) {
    console.error('usage: npm run site:screenshots -- <site-dir> [shot-name ...]');
    process.exit(2);
  }
  const out = join(resolve(siteDir), 'assets/img/ui');
  mkdirSync(out, { recursive: true });
  const shots = only.length ? SHOTS.filter((s) => only.includes(s.name)) : SHOTS;
  const unknown = only.filter((n) => !SHOTS.some((s) => s.name === n));
  if (unknown.length) throw new Error(`unknown shots: ${unknown.join(', ')}`);

  const server = await serveDemo();
  const browser = await chromium.launch();
  const failed: string[] = [];
  try {
    const context = await browser.newContext({ viewport: VIEWPORT, reducedMotion: 'reduce', locale: 'en-GB', timezoneId: 'UTC' });
    for (const shot of shots) {
      const page = await context.newPage();
      if (shot.viewport) await page.setViewportSize(shot.viewport);
      const problems: string[] = [];
      page.on('pageerror', (e) => problems.push(e.message));
      page.on('console', (m) => m.type() === 'error' && problems.push(m.text()));
      page.on('response', (r) => r.status() >= 400 && problems.push(`${r.status()} ${r.url()}`));
      try {
        await page.goto(`${BASE}/#${shot.path}`);
        await page.getByRole('heading', { level: 1 }).first().waitFor();
        await page.addStyleTag({ content: '.demo-note { display: none !important; }' });
        await settle(page);
        await shot.act?.(page);
        await settle(page);
        await retouch(page);
        await page.screenshot({ path: join(out, `${shot.name}.jpg`), type: 'jpeg', quality: QUALITY });
        if (problems.length) throw new Error(problems.join('; '));
        console.log(`ok   ${shot.name}`);
      } catch (err) {
        failed.push(shot.name);
        console.error(`FAIL ${shot.name}: ${err instanceof Error ? err.message.split('\n')[0] : String(err)}`);
      } finally {
        await page.close();
      }
    }
  } finally {
    await browser.close();
    server.close();
  }
  console.log(`${shots.length - failed.length} of ${shots.length} screenshots in ${out}`);
  if (failed.length) process.exit(1);
}

await main();
