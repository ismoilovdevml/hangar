// Checks the built site without a browser, reading the files the way GitHub Pages serves
// them under /hangar/:
//  - every internal link, image, stylesheet, script and frame resolves to a file, and every
//    #fragment exists on its target page;
//  - every page (demo/ aside) has lang, a title and one h1, no skipped heading levels and
//    valid JSON-LD; indexable pages also a description, a canonical URL, og:url, og:title,
//    og:description, og:image and a twitter:card;
//  - docs.html, run in a sandbox with every old #id, lands on an existing page and fragment;
//  - every sitemap.xml URL exists, and every docs page is in it;
//  - the site's scripts parse.
// What it cannot see: runtime errors in the browser console.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, posix } from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import { parse } from 'node-html-parser';

const here = dirname(fileURLToPath(import.meta.url));
const ROOT = join(here, '..');
const SITE_URL = 'https://ismoilovdevml.github.io/hangar/';
const failures = [];
const fail = (where, msg) => failures.push(`${where}: ${msg}`);

function walk(dir, base = '') {
  return readdirSync(join(dir, base), { withFileTypes: true }).flatMap((e) => {
    const rel = posix.join(base, e.name);
    if (e.isDirectory()) return ['site', 'demo', 'node_modules'].includes(rel) || e.name.startsWith('.') ? [] : walk(dir, rel);
    return e.name.endsWith('.html') ? [rel] : [];
  });
}

// Site path ("docs/npm/") of an HTML file ("docs/npm/index.html").
const urlPath = (file) => file.replace(/(^|\/)index\.html$/, '$1');
// File behind a site path, or null.
function fileFor(path) {
  const p = join(ROOT, path);
  if (path === '' || path.endsWith('/')) return existsSync(join(p, 'index.html')) ? join(p, 'index.html') : null;
  if (existsSync(p) && statSync(p).isFile()) return p;
  return null;
}

const docs = new Map(); // file -> parsed document
const load = (file) => {
  if (!docs.has(file)) docs.set(file, parse(readFileSync(file, 'utf8')));
  return docs.get(file);
};
const hasId = (file, id) => !!load(file).querySelector(`[id="${id.replace(/"/g, '\\"')}"]`);

const pages = walk(ROOT).sort();
let refs = 0, fragments = 0;

for (const file of pages) {
  const doc = load(join(ROOT, file));
  const path = urlPath(file);
  const where = '/' + path;
  const meta = (sel) => doc.querySelector(sel)?.getAttribute('content') || '';
  const robots = meta('meta[name="robots"]');
  const redirectOnly = !doc.querySelector('h1');

  if (doc.querySelector('html')?.getAttribute('lang') !== 'en') fail(where, 'no lang="en"');
  if (!doc.querySelector('title')?.text.trim()) fail(where, 'no <title>');
  for (const s of doc.querySelectorAll('script[type="application/ld+json"]')) {
    try { JSON.parse(s.text); } catch { fail(where, 'invalid JSON-LD'); }
  }
  if (!redirectOnly) {
    if (doc.querySelectorAll('h1').length !== 1) fail(where, `${doc.querySelectorAll('h1').length} <h1> elements`);
    const levels = (doc.querySelector('main') || doc).querySelectorAll('h1, h2, h3, h4, h5, h6').map((h) => +h.tagName[1]);
    for (let i = 1; i < levels.length; i++) if (levels[i] > levels[i - 1] + 1) { fail(where, `heading jumps from h${levels[i - 1]} to h${levels[i]}`); break; }
  }
  if (!robots.includes('noindex')) {
    const expected = SITE_URL + path;
    const desc = meta('meta[name="description"]');
    if (desc.length < 50 || desc.length > 200) fail(where, `description length ${desc.length}`);
    if (doc.querySelector('link[rel="canonical"]')?.getAttribute('href') !== expected) fail(where, 'canonical is not ' + expected);
    if (meta('meta[property="og:url"]') !== expected) fail(where, 'og:url is not ' + expected);
    if (!meta('meta[property="og:title"]') || !meta('meta[property="og:description"]')) fail(where, 'no og:title or og:description');
    if (meta('meta[property="og:image"]') !== SITE_URL + 'assets/og.png') fail(where, 'og:image is not assets/og.png');
    if (meta('meta[name="twitter:card"]') !== 'summary_large_image') fail(where, 'twitter:card is not summary_large_image');
    if (!doc.querySelector('script[type="application/ld+json"]')) fail(where, 'no JSON-LD');
  }

  // Internal references. Absolute paths are written as /hangar/...; relative ones resolve from the page.
  const targets = [
    ...doc.querySelectorAll('a[href]').map((a) => a.getAttribute('href')),
    ...doc.querySelectorAll('img[src], script[src]').map((e) => e.getAttribute('src')),
    ...doc.querySelectorAll('link[href]').filter((l) => /stylesheet|preload|icon/.test(l.getAttribute('rel'))).map((l) => l.getAttribute('href')),
    ...doc.querySelectorAll('iframe[data-src]').map((f) => f.getAttribute('data-src')),
  ];
  for (const ref of targets) {
    if (/^(https?:|mailto:|data:)/.test(ref)) continue;
    refs++;
    const [rawPath, hash = ''] = ref.split('#');
    let target;
    if (rawPath.startsWith('/')) {
      if (!rawPath.startsWith('/hangar/')) { fail(where, `link ${ref} is outside /hangar/`); continue; }
      target = rawPath.slice('/hangar/'.length);
    } else {
      target = rawPath === '' ? path : posix.normalize(posix.join(posix.dirname(path + 'x'), rawPath));
      if (rawPath.endsWith('/') && !target.endsWith('/')) target += '/';
      if (target === '.' || target === './') target = '';
    }
    if (target.startsWith('..')) { fail(where, `link ${ref} leaves the site`); continue; }
    const tfile = fileFor(decodeURIComponent(target.split('?')[0]));
    if (!tfile) { fail(where, `link ${ref} -> /${target} does not exist`); continue; }
    if (hash && !hash.startsWith('/') && tfile.endsWith('.html')) {
      fragments++;
      if (!hasId(tfile, decodeURIComponent(hash))) fail(where, `link ${ref}: no element with id "${decodeURIComponent(hash)}"`);
    }
  }
}

/* docs.html: run its redirect script for every old id. */
const redirects = JSON.parse(readFileSync(join(here, 'redirects.json'), 'utf8'));
const script = load(join(ROOT, 'docs.html')).querySelectorAll('script').map((s) => s.text).join('\n');
function landing(hash) {
  let to = null;
  vm.runInNewContext(script, { location: { hash, replace: (u) => { to = u; } }, decodeURIComponent });
  return to;
}
const cases = [...Object.keys(redirects).map((id) => ['#' + id, redirects[id]]), ['#npm:publish', 'docs/guides/npm/'], ['', 'docs/'], ['#no-such-page', 'docs/']];
for (const [hash, expected] of cases) {
  const to = landing(hash);
  if (to !== expected) { fail('docs.html', `${hash || '(no hash)'} lands on ${to}, expected ${expected}`); continue; }
  const [p, frag] = to.split('#');
  const tfile = fileFor(p);
  if (!tfile) fail('docs.html', `${hash} -> ${to} does not exist`);
  else if (frag && !hasId(tfile, frag)) fail('docs.html', `${hash} -> ${to}: no element with id "${frag}"`);
}

/* Sitemap and robots.txt. */
const locs = [...readFileSync(join(ROOT, 'sitemap.xml'), 'utf8').matchAll(/<loc>([^<]+)<\/loc>/g)].map((m) => m[1]);
for (const loc of locs) {
  if (!loc.startsWith(SITE_URL) || !fileFor(loc.slice(SITE_URL.length))) fail('sitemap.xml', `${loc} does not exist`);
}
const docsPages = pages.filter((f) => f.startsWith('docs/')).map((f) => SITE_URL + urlPath(f));
for (const u of docsPages) if (!locs.includes(u)) fail('sitemap.xml', `${u} is missing`);
if (!readFileSync(join(ROOT, 'robots.txt'), 'utf8').includes(`Sitemap: ${SITE_URL}sitemap.xml`)) fail('robots.txt', 'no Sitemap line');

/* Scripts parse. */
for (const js of ['assets/site.js', 'assets/docs-ui.js']) {
  try { new vm.Script(readFileSync(join(ROOT, js), 'utf8'), { filename: js }); } catch (e) { fail(js, e.message); }
}
const search = JSON.parse(readFileSync(join(ROOT, 'assets', 'search-index.json'), 'utf8'));
for (const p of search.pages) if (!fileFor(p.u)) fail('search-index.json', `${p.u} does not exist`);

console.log(`pages checked:       ${pages.length} (${docsPages.length} under docs/)`);
console.log(`internal references: ${refs} (${fragments} with a #fragment)`);
console.log(`docs.html redirects: ${cases.length}`);
console.log(`sitemap URLs:        ${locs.length}`);
if (failures.length) {
  console.log(`FAILED: ${failures.length} problem(s)`);
  for (const f of failures.slice(0, 40)) console.log('  - ' + f);
  process.exit(1);
}
console.log('OK');
