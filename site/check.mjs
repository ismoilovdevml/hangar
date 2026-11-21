// Checks the built site the way GitHub Pages serves it (under /hangar/):
//  - every page reachable from the home page and the docs renders without console
//    errors, page errors or failed same-site requests;
//  - every internal link, image, stylesheet and script answers 200, and every #fragment
//    exists on its target page;
//  - every page has lang, a title, a description, a canonical URL, Open Graph and
//    Twitter tags, valid JSON-LD, one h1 and no skipped heading levels;
//  - every docs.html#old-id redirect lands on an existing page and fragment;
//  - every sitemap.xml URL exists.
// Serves the branch root with python3 -m http.server, like the README's preview.
import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { launch } from './browser.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const ROOT = join(here, '..');
const SITE_URL = 'https://ismoilovdevml.github.io/hangar/';
const PORT = 8000 + Math.floor(Math.random() * 900);
const BASE = `http://127.0.0.1:${PORT}/hangar/`;

const failures = [];
const fail = (where, msg) => failures.push(`${where}: ${msg}`);

/* ---------- server ---------- */
const serveDir = mkdtempSync(join(tmpdir(), 'hangar-site-'));
symlinkSync(ROOT, join(serveDir, 'hangar'));
const server = spawn('python3', ['-m', 'http.server', String(PORT), '--bind', '127.0.0.1', '--directory', serveDir], { stdio: 'ignore' });
const stop = () => { server.kill(); rmSync(serveDir, { recursive: true, force: true }); };
process.on('exit', stop);
for (let i = 0; ; i++) {
  try { if ((await fetch(BASE)).ok) break; } catch { /* not up yet */ }
  if (i > 50) throw new Error('python3 -m http.server did not start');
  await new Promise((r) => setTimeout(r, 100));
}

/* ---------- fetch cache ---------- */
const cache = new Map();
function get(url) {
  const u = url.split('#')[0];
  if (!cache.has(u)) {
    cache.set(u, fetch(u, { redirect: 'follow' }).then(async (r) => ({
      status: r.status, url: r.url, type: r.headers.get('content-type') || '',
      body: (r.headers.get('content-type') || '').includes('html') ? await r.text() : (await r.arrayBuffer(), ''),
    })));
  }
  return cache.get(u);
}
const isInternal = (u) => u.startsWith(BASE);
const isPage = (u) => { const p = new URL(u).pathname; return p.endsWith('/') || p.endsWith('.html'); };
const isDemo = (u) => new URL(u).pathname.startsWith('/hangar/demo/');

/* ---------- crawl ---------- */
const browser = await launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const queue = [BASE, BASE + 'docs/', BASE + '404.html', BASE + 'demo/'];
const seen = new Set(queue);
const links = new Map(); // target -> first page that links it
const pagesChecked = [];

while (queue.length) {
  const url = queue.shift();
  if (process.env.VERBOSE) console.log('render ' + url);
  const page = await context.newPage();
  const problems = [];
  page.on('console', (m) => { if (m.type() === 'error') problems.push(`console error: ${m.text()}`); });
  page.on('pageerror', (e) => problems.push(`page error: ${e.message}`));
  page.on('requestfailed', (r) => { if (r.url().startsWith(`http://127.0.0.1:${PORT}/`)) problems.push(`request failed: ${r.url()} ${r.failure()?.errorText}`); });
  page.on('response', (r) => { if (r.url().startsWith(BASE) && r.status() >= 400 && !url.endsWith('404.html')) problems.push(`HTTP ${r.status()}: ${r.url()}`); });
  const res = await page.goto(url, { waitUntil: 'load' });
  if (!res || res.status() !== 200) fail(url, `HTTP ${res && res.status()}`);
  // Scroll through the page so lazy images and the demo frame load too.
  await page.evaluate(async () => {
    for (let y = 0; y < document.body.scrollHeight; y += 700) { window.scrollTo(0, y); await new Promise((r) => setTimeout(r, 30)); }
  });
  await page.waitForTimeout(isDemo(url) ? 1500 : 300);

  if (!isDemo(url)) {
    const info = await page.evaluate(() => {
      const meta = (sel) => document.querySelector(sel)?.getAttribute('content') || '';
      const heads = [...document.querySelectorAll('main h1, main h2, main h3, main h4, main h5, main h6')].map((h) => +h.tagName[1]);
      const ld = [...document.querySelectorAll('script[type="application/ld+json"]')].map((s) => { try { JSON.parse(s.textContent); return true; } catch { return false; } });
      const refs = [
        ...[...document.querySelectorAll('a[href]')].map((a) => a.href),
        ...[...document.querySelectorAll('img[src]')].map((i) => i.src),
        ...[...document.querySelectorAll('link[href][rel="stylesheet"], link[href][rel="preload"], script[src]')].map((l) => l.href || l.src),
        ...[...document.querySelectorAll('iframe[data-src]')].map((f) => new URL(f.dataset.src, location.href).href),
      ];
      return {
        lang: document.documentElement.lang, title: document.title, desc: meta('meta[name="description"]'),
        canonical: document.querySelector('link[rel="canonical"]')?.href || '', robots: meta('meta[name="robots"]'),
        ogTitle: meta('meta[property="og:title"]'), ogDesc: meta('meta[property="og:description"]'), ogImage: meta('meta[property="og:image"]'),
        ogUrl: meta('meta[property="og:url"]'), twCard: meta('meta[name="twitter:card"]'),
        h1: document.querySelectorAll('h1').length, heads, ld, refs, ids: [...document.querySelectorAll('[id]')].map((e) => e.id),
      };
    });
    const where = url.slice(BASE.length - '/hangar/'.length);
    if (info.lang !== 'en') fail(where, `lang="${info.lang}"`);
    if (!info.title) fail(where, 'no <title>');
    if (info.h1 !== 1) fail(where, `${info.h1} <h1> elements`);
    for (let i = 1; i < info.heads.length; i++) if (info.heads[i] > info.heads[i - 1] + 1) { fail(where, `heading jumps from h${info.heads[i - 1]} to h${info.heads[i]}`); break; }
    if (info.ld.includes(false)) fail(where, 'invalid JSON-LD');
    const indexable = !info.robots.includes('noindex');
    if (indexable) {
      if (info.desc.length < 50 || info.desc.length > 200) fail(where, `description length ${info.desc.length}`);
      const expected = SITE_URL + new URL(url).pathname.replace(/^\/hangar\//, '');
      if (info.canonical !== expected) fail(where, `canonical ${info.canonical}, expected ${expected}`);
      if (info.ogUrl !== expected) fail(where, `og:url ${info.ogUrl}`);
      if (!info.ogTitle || !info.ogDesc) fail(where, 'missing og:title or og:description');
      if (info.ogImage !== SITE_URL + 'assets/og.png') fail(where, `og:image ${info.ogImage}`);
      if (info.twCard !== 'summary_large_image') fail(where, `twitter:card ${info.twCard}`);
      if (!info.ld.length) fail(where, 'no JSON-LD');
    }
    for (const ref of info.refs) {
      if (!isInternal(ref)) continue;
      if (!links.has(ref)) links.set(ref, where);
      const bare = ref.split('#')[0];
      if (isPage(bare) && !isDemo(bare) && !seen.has(bare)) { seen.add(bare); queue.push(bare); }
    }
    pagesChecked.push({ url: where, title: info.title, desc: info.desc.length, problems: problems.length });
  } else {
    pagesChecked.push({ url: url.slice(BASE.length - '/hangar/'.length), title: await page.title(), desc: 0, problems: problems.length });
  }
  for (const p of problems) fail(url.slice(BASE.length - '/hangar/'.length), p);
  await page.close();
}

/* ---------- links and fragments ---------- */
let fragments = 0;
for (const [ref, from] of links) {
  const r = await get(ref);
  if (r.status !== 200) { fail(from, `link ${ref.slice(BASE.length - 8)} answers ${r.status}`); continue; }
  const hash = decodeURIComponent(new URL(ref).hash.slice(1));
  if (hash && !isDemo(ref)) {
    fragments++;
    if (!r.body.includes(`id="${hash}"`)) fail(from, `link ${ref.slice(BASE.length - 8)}: no element with id "${hash}"`);
  }
}

/* ---------- docs.html redirects ---------- */
const redirects = JSON.parse(readFileSync(join(here, 'redirects.json'), 'utf8'));
const rpage = await context.newPage();
for (const [id, target] of Object.entries(redirects)) {
  await rpage.goto(`${BASE}docs.html#${id}`);
  await rpage.waitForURL((u) => !u.pathname.endsWith('/docs.html'), { timeout: 5000 }).catch(() => {});
  const landed = rpage.url();
  if (landed !== BASE + target) { fail('docs.html', `#${id} lands on ${landed}, expected ${BASE + target}`); continue; }
  const r = await get(landed);
  const hash = new URL(landed).hash.slice(1);
  if (r.status !== 200 || (hash && !r.body.includes(`id="${hash}"`))) fail('docs.html', `#${id} -> ${target} does not exist`);
}
// Section links of the old router: docs.html#npm:publish opens the npm page.
await rpage.goto(`${BASE}docs.html#npm:publish`);
await rpage.waitForURL((u) => !u.pathname.endsWith('/docs.html'), { timeout: 5000 }).catch(() => {});
if (rpage.url() !== BASE + 'docs/guides/npm/') fail('docs.html', `#npm:publish lands on ${rpage.url()}`);
await rpage.goto(`${BASE}docs.html`);
await rpage.waitForURL((u) => !u.pathname.endsWith('/docs.html'), { timeout: 5000 }).catch(() => {});
if (rpage.url() !== BASE + 'docs/') fail('docs.html', `no hash lands on ${rpage.url()}`);

/* ---------- sitemap and robots ---------- */
const sitemap = readFileSync(join(ROOT, 'sitemap.xml'), 'utf8');
const locs = [...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map((m) => m[1]);
for (const loc of locs) {
  if (!loc.startsWith(SITE_URL)) { fail('sitemap.xml', `foreign URL ${loc}`); continue; }
  const r = await get(BASE + loc.slice(SITE_URL.length));
  if (r.status !== 200) fail('sitemap.xml', `${loc} answers ${r.status}`);
}
const docsPages = pagesChecked.filter((p) => p.url.startsWith('/hangar/docs/')).length;
if (locs.length !== docsPages + 1) fail('sitemap.xml', `${locs.length} URLs, but ${docsPages} docs pages + the home page were crawled`);
if (!readFileSync(join(ROOT, 'robots.txt'), 'utf8').includes(`Sitemap: ${SITE_URL}sitemap.xml`)) fail('robots.txt', 'no Sitemap line');

await browser.close();

/* ---------- report ---------- */
console.log(`pages rendered:      ${pagesChecked.length} (${docsPages} under /docs/)`);
console.log(`internal references: ${links.size} checked, ${fragments} with a #fragment`);
console.log(`docs.html redirects: ${Object.keys(redirects).length + 2} checked`);
console.log(`sitemap URLs:        ${locs.length} checked`);
if (failures.length) {
  console.log(`\nFAILED: ${failures.length} problem(s)`);
  for (const f of failures) console.log('  - ' + f);
  process.exit(1);
}
console.log('\nOK: no console errors, broken links, missing fragments or missing meta tags');
// The http.server child keeps the event loop alive; exit explicitly (the exit hook stops it).
process.exit(0);
