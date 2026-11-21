// Builds the docs of the Hangar website: one HTML page per markdown file, with the
// sidebar, heading anchors, highlighted code, a search index, the sitemap, robots.txt
// and docs.html (which sends the old docs.html#page links to the new pages).
//
// Input:  .cache/docs (docs/ of main, written by sync.mjs) and content/ (pages that
//         exist only on the website: introduction, FAQ, the web UI walkthroughs).
// Output: ../docs/**/index.html, ../docs.html, ../sitemap.xml, ../robots.txt,
//         ../assets/search-index.json, ../assets/fonts/*.woff2
import { copyFileSync, existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, posix } from 'node:path';
import { fileURLToPath } from 'node:url';
import MarkdownIt from 'markdown-it';
import hljs from 'highlight.js';
import GithubSlugger from 'github-slugger';
import { SECTIONS } from './nav.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const ROOT = join(here, '..');
const SITE_URL = 'https://ismoilovdevml.github.io/hangar/';
const REPO = 'https://github.com/ismoilovdevml/hangar';
const SITE_NAME = 'Hangar — self-hosted package registry';
const OG_IMAGE = { url: SITE_URL + 'assets/og.png', width: 1200, height: 630, alt: 'Hangar, the self-hosted package registry for Docker, npm, Maven, PyPI, NuGet, Go, Helm, Cargo, APT, RPM and raw files' };

const warnings = [];
const warn = (msg) => warnings.push(msg);

/* ---------- sources ---------- */

const sourcePath = join(here, '.cache', 'source.json');
if (!existsSync(sourcePath)) {
  console.error('build: no docs in .cache. Run "npm run sync" (or "npm run build", which syncs first).');
  process.exit(1);
}
const SOURCE = JSON.parse(readFileSync(sourcePath, 'utf8'));

function walk(dir, base = '') {
  return readdirSync(join(dir, base), { withFileTypes: true }).flatMap((e) => {
    const rel = posix.join(base, e.name);
    if (e.isDirectory()) return walk(dir, rel);
    return e.name.endsWith('.md') ? [rel] : [];
  }).sort();
}

// Output directory of a source file, relative to the site root, ending in "/".
function outDir(src) {
  if (src.startsWith('content/')) {
    const rel = src.slice('content/'.length).replace(/\.md$/, '');
    const dir = rel === 'index' ? '' : rel.endsWith('/index') ? rel.slice(0, -'/index'.length) : rel;
    return posix.join('docs', dir) + '/';
  }
  const rel = src.slice('docs/'.length).replace(/\.md$/, '');
  const parts = rel.split('/');
  const name = parts.pop();
  const dir = name === 'README' ? parts : [...parts, name.toLowerCase().replace(/_/g, '-')];
  return posix.join('docs', ...dir) + '/';
}

const pages = [];
const bySrc = new Map();
const byDir = new Map();
for (const rel of walk(join(here, '.cache', 'docs'))) addPage(`docs/${rel}`, join(here, '.cache', 'docs', rel));
for (const rel of walk(join(here, 'content'))) addPage(`content/${rel}`, join(here, 'content', rel));

function addPage(src, file) {
  const dir = outDir(src);
  if (byDir.has(dir)) throw new Error(`${src} and ${byDir.get(dir).src} both build ${dir}`);
  const page = { src, file, dir, markdown: readFileSync(file, 'utf8') };
  pages.push(page);
  bySrc.set(src, page);
  byDir.set(dir, page);
}

/* ---------- navigation ---------- */

const nav = [];   // [{ title, items: [page] }]
const order = []; // pages in reading order (pager)
for (const [title, items] of SECTIONS) {
  const section = { title, items: [] };
  for (const [src, label, opts = {}] of items) {
    const page = bySrc.get(src);
    if (!page) { warn(`nav.mjs lists ${src}, which does not exist`); continue; }
    Object.assign(page, { label, section: title });
    section.items.push(page);
    order.push(page);
    if (opts.children) {
      for (const child of pages.filter((p) => p.src.startsWith(opts.children) && p !== page && !p.section)) {
        Object.assign(child, { section: title, parent: page });
        order.push(child);
      }
    }
  }
  nav.push(section);
}
const unlisted = pages.filter((p) => !p.section);
if (unlisted.length) {
  const section = { title: 'More', items: [] };
  for (const page of unlisted) {
    warn(`${page.src} is not in nav.mjs; listed under "More"`);
    Object.assign(page, { section: 'More' });
    section.items.push(page);
    order.push(page);
  }
  nav.push(section);
}

/* ---------- links ---------- */

// Relative URL from one output directory to a site-root path ("" is the home page).
function rel(fromDir, target) {
  const [path, hash = ''] = target.split('#');
  let r = posix.relative(fromDir || '.', path || '.');
  if ((path === '' || path.endsWith('/')) && r !== '') r += '/';
  if (r === '') r = './';
  return r + (hash ? '#' + hash : '');
}

const esc = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');

// Site-root path or absolute URL of a link written in a page's markdown.
function resolveHref(href, page) {
  if (!href || /^[a-z][a-z0-9+.-]*:/i.test(href) || href.startsWith('//')) return { url: href };
  if (href.startsWith('#')) return { url: href };
  const [path, hash] = href.split('#');
  const frag = hash ? '#' + hash : '';
  if (page.src.startsWith('content/')) {
    // Website pages link by site-root paths: /docs/ui/, /demo/, /assets/img/...
    if (!path.startsWith('/')) { warn(`${page.src}: relative link ${href}; use a site-root path`); return { url: href }; }
    return { site: path.slice(1) + frag };
  }
  // Pages from main link relative to their file in the repository.
  const repoPath = posix.normalize(posix.join(posix.dirname(page.src), path)).replace(/\/$/, '');
  const target = bySrc.get(repoPath) || bySrc.get(posix.join(repoPath, 'README.md'));
  if (target) return { site: target.dir + frag };
  if (repoPath.startsWith('..')) { warn(`${page.src}: link ${href} leaves the repository`); return { url: href }; }
  if (repoPath.startsWith('docs/') && repoPath.endsWith('.md')) warn(`${page.src}: link ${href} points to a missing page; linked on GitHub`);
  const kind = /\.[a-z0-9]+$/i.test(posix.basename(repoPath)) ? 'blob' : 'tree';
  return { url: `${REPO}/${kind}/main/${repoPath}${frag}` };
}

/* ---------- markdown ---------- */

const md = new MarkdownIt({ html: true, linkify: false, typographer: false });
const LANG_LABEL = { sh: 'shell', bash: 'shell', shell: 'shell', console: 'shell', '': 'text' };

md.renderer.rules.fence = (tokens, idx) => {
  const info = tokens[idx].info.trim().split(/\s+/)[0].toLowerCase();
  const code = tokens[idx].content.replace(/\n$/, '');
  const lang = info && hljs.getLanguage(info) ? info : '';
  const body = lang ? hljs.highlight(code, { language: lang, ignoreIllegals: true }).value : md.utils.escapeHtml(code);
  const label = LANG_LABEL[info] ?? info;
  return `<div class="code"><div class="code-head"><span>${esc(label)}</span><button class="code-copy" type="button">Copy</button></div>` +
    `<pre tabindex="0"${lang ? ` data-lang="${esc(lang)}"` : ''}><code>${body}</code></pre></div>\n`;
};
md.renderer.rules.table_open = () => '<div class="tbl-wrap"><table class="tbl">\n';
md.renderer.rules.table_close = () => '</table></div>\n';
md.renderer.rules.blockquote_open = () => '<blockquote class="call">\n';
md.renderer.rules.heading_close = (tokens, idx) => {
  const open = tokens[idx - 2];
  const id = open.attrGet('id');
  const anchor = id && (open.tag === 'h2' || open.tag === 'h3')
    ? `<a class="anchor" href="#${esc(id)}" aria-label="Link to this section">#</a>` : '';
  return `${anchor}</${open.tag}>\n`;
};

const inlineText = (tok) => (tok.children || []).map((c) => (c.type === 'text' || c.type === 'code_inline' ? c.content : c.type === 'softbreak' ? ' ' : '')).join('');

// Inline children without the empty text tokens markdown-it leaves around emphasis.
const solid = (children) => (children || []).filter((c) => !(c.type === 'text' && c.content === ''));

// Matching close token of the open token at index i.
function closing(tokens, i) {
  const type = tokens[i].type.replace(/_open$/, '_close');
  for (let j = i + 1, depth = 0; j < tokens.length; j++) {
    if (tokens[j].type === tokens[i].type) depth++;
    else if (tokens[j].type === type) { if (depth === 0) return j; depth--; }
  }
  return -1;
}

md.core.ruler.push('hangar', (state) => {
  const { page } = state.env;
  const tokens = state.tokens;
  const slugger = new GithubSlugger();
  page.headings = [];

  // Title and lead: the first h1 leaves the body, the paragraph after it becomes the lead.
  const h1 = tokens.findIndex((t) => t.type === 'heading_open' && t.tag === 'h1');
  if (h1 === -1) throw new Error(`${page.src}: no "# Title"`);
  page.title = inlineText(tokens[h1 + 1]).trim();
  page.titleHtml = md.renderer.renderInline(tokens[h1 + 1].children, md.options, state.env);
  slugger.slug(page.title);
  tokens.splice(h1, 3);
  if (tokens[h1] && tokens[h1].type === 'paragraph_open' && h1 === 0) {
    tokens[h1].attrJoin('class', 'lead');
    page.lead = inlineText(tokens[h1 + 1]).replace(/\s+/g, ' ').trim();
  }

  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    if (t.type === 'heading_open') {
      const text = inlineText(tokens[i + 1]).trim();
      const id = slugger.slug(text);
      t.attrSet('id', id);
      if (t.tag === 'h2' || t.tag === 'h3') page.headings.push({ level: +t.tag[1], id, text });
    }
    if (t.type === 'inline') {
      for (const c of t.children || []) {
        if (c.type === 'link_open') {
          const r = resolveHref(c.attrGet('href'), page);
          c.attrSet('href', r.site !== undefined ? rel(page.dir, r.site) : r.url);
        }
        if (c.type === 'image') {
          const r = resolveHref(c.attrGet('src'), page);
          c.attrSet('src', r.site !== undefined ? rel(page.dir, r.site) : r.url);
        }
      }
    }
  }

  for (let i = 0; i < tokens.length; i++) {
    const t = tokens[i];
    // A paragraph holding only an image is a figure that enlarges on click.
    if (t.type === 'paragraph_open' && tokens[i + 1].children?.length === 1 && tokens[i + 1].children[0].type === 'image') {
      const img = tokens[i + 1].children[0];
      const alt = inlineText(img);
      const cap = img.attrGet('title') || alt;
      const html = new state.Token('html_block', '', 0);
      html.content = `<figure class="fig"><button type="button" class="zoom" aria-label="Enlarge screenshot: ${esc(alt)}">` +
        `<img src="${esc(img.attrGet('src'))}" alt="${esc(alt)}" loading="lazy" decoding="async" width="1440" height="900"></button>` +
        `<figcaption>${esc(cap)}</figcaption></figure>\n`;
      tokens.splice(i, 3, html);
      continue;
    }
    // A paragraph holding only a link into the demo is a "Try it in the demo" button.
    if (t.type === 'paragraph_open') {
      const ch = solid(tokens[i + 1].children);
      if (ch.length === 3 && ch[0].type === 'link_open' && /(^|\/)demo\//.test(ch[0].attrGet('href'))) {
        t.attrJoin('class', 'try-row');
        ch[0].attrJoin('class', 'try');
        ch[0].attrSet('target', '_blank');
        ch[0].attrSet('rel', 'noopener');
      }
    }
    // An ordered list whose items each start with a bold-only line renders as numbered steps.
    if (t.type === 'ordered_list_open') {
      const end = closing(tokens, i);
      const items = [];
      for (let j = i + 1; j < end; j++) if (tokens[j].type === 'list_item_open' && tokens[j].level === t.level + 1) items.push(j);
      const isStep = (j) => {
        const ch = solid(tokens[j + 2]?.children);
        return tokens[j + 1].type === 'paragraph_open' && ch.length === 3 && ch[0].type === 'strong_open' && ch[2].type === 'strong_close';
      };
      if (items.length && items.every(isStep)) t.attrJoin('class', 'steps');
    }
    // A bullet list of "[**Title**](link): text" items renders as cards.
    if (t.type === 'bullet_list_open') {
      const end = closing(tokens, i);
      const cards = [];
      for (let j = i + 1; j < end; j++) {
        if (tokens[j].type !== 'inline' || tokens[j].level !== t.level + 3) continue;
        const ch = solid(tokens[j].children);
        if (ch.length < 6 || ch[0].type !== 'link_open' || ch[1].type !== 'strong_open' || ch[3].type !== 'strong_close' || ch[4].type !== 'link_close' || ch[5].type !== 'text' || !ch[5].content.startsWith(': ')) { cards.length = 0; break; }
        const rest = [Object.assign(new state.Token('text', '', 0), { content: ch[5].content.slice(2) }), ...ch.slice(6)];
        cards.push(`<a class="card" href="${esc(ch[0].attrGet('href'))}"><b>${esc(ch[2].content)}</b><span>${md.renderer.renderInline(rest, md.options, state.env)}</span></a>`);
      }
      if (cards.length) {
        const html = new state.Token('html_block', '', 0);
        html.content = `<div class="cards">${cards.join('')}</div>\n`;
        tokens.splice(i, end - i + 1, html);
      }
    }
  }

  // Plain text per section, for the search index (code blocks left out).
  // Top-level paragraphs other than the lead, for descriptions of pages whose lead is short.
  page.paras = tokens.flatMap((t, i) => (t.type === 'paragraph_open' && t.level === 0 && !(t.attrGet('class') || '').includes('lead')
    ? [inlineText(tokens[i + 1]).replace(/\s+/g, ' ').trim()] : [])).filter((p) => p.length >= 40);

  page.sections = [{ id: '', text: '' }];
  for (const t of tokens) {
    if (t.type === 'heading_open' && t.tag !== 'h1') page.sections.push({ id: t.attrGet('id'), heading: '', text: '', pending: true });
    else if (t.type === 'inline') {
      const s = page.sections[page.sections.length - 1];
      if (s.pending) { s.heading = inlineText(t).trim(); s.pending = false; } else s.text += inlineText(t) + ' ';
    }
  }
});

/* ---------- templates ---------- */

const ICON = `data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='8' fill='%23111'/%3E%3Cpath d='M8 24V13l8-5 8 5v11' fill='none' stroke='white' stroke-width='2.4' stroke-linejoin='round'/%3E%3C/svg%3E`;
const LOGO = '<svg width="22" height="22" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="8" fill="#111"/><path d="M8 24V13l8-5 8 5v11" fill="none" stroke="#fff" stroke-width="2.4" stroke-linejoin="round"/></svg>';
const SEARCH_ICON = (n) => `<svg width="${n}" height="${n}" viewBox="0 0 24 24" fill="none" aria-hidden="true"><circle cx="11" cy="11" r="7" stroke="currentColor" stroke-width="2"/><path d="M20 20l-3.5-3.5" stroke="currentColor" stroke-width="2"/></svg>`;

function describe(page) {
  // The lead, or "Title: the first paragraphs" when there is no lead or it is too short.
  let text = page.lead || '';
  if (text.length < 80) text = [`${page.title}:`, text, ...page.paras].filter(Boolean).join(' ');
  if (text.length < 50) text = `${page.title}, in the documentation of Hangar, the self-hosted package registry.`;
  if (text.length <= 160) return text;
  const cut = text.slice(0, 157);
  return cut.slice(0, cut.lastIndexOf(' ')).replace(/[,;:.]$/, '') + '…';
}

function sidebar(page) {
  const current = page.parent || page;
  return nav.map((s) => `<p class="side-h">${esc(s.title)}</p><ul>` + s.items.map((p) =>
    `<li><a href="${rel(page.dir, p.dir)}"${p === current ? ' aria-current="page"' : ''}>${esc(p.label)}</a></li>`).join('') + '</ul>').join('\n');
}

function toc(page) {
  if (page.headings.length < 2) return '';
  return '<p class="toc-h">On this page</p><ul>' + page.headings.map((h) =>
    `<li class="l${h.level}"><a href="#${esc(h.id)}">${esc(h.text)}</a></li>`).join('') + '</ul>';
}

function pager(page) {
  const i = order.indexOf(page);
  const prev = order[i - 1], next = order[i + 1];
  const link = (p, cls, word) => p ? `<a class="${cls}" href="${rel(page.dir, p.dir)}" rel="${cls}"><small>${word}</small><b>${esc(p.label || p.title)}</b></a>` : '';
  return `<nav class="pager" aria-label="Previous and next page">${link(prev, 'prev', 'Previous')}${link(next, 'next', 'Next')}</nav>`;
}

function sourceLinks(page) {
  if (page.src.startsWith('docs/')) {
    const at = SOURCE.commit ? ` at <code>${SOURCE.commit.slice(0, 7)}</code>` : '';
    return `<p class="edit-row">Source: <a href="${REPO}/blob/main/${page.src}"><code>${esc(page.src)}</code></a> on main${at} · <a href="${REPO}/edit/main/${page.src}">Edit this page</a> · <a href="${REPO}/issues/new">Report a problem</a></p>`;
  }
  return `<p class="edit-row">Source: <a href="${REPO}/blob/gh-pages/site/${page.src}"><code>site/${esc(page.src)}</code></a> on gh-pages · <a href="${REPO}/issues/new">Report a problem</a></p>`;
}

function jsonLd(page) {
  const crumbs = [{ name: 'Hangar', item: SITE_URL }, { name: 'Docs', item: SITE_URL + 'docs/' }];
  if (page.dir !== 'docs/') crumbs.push({ name: page.label || page.title, item: SITE_URL + page.dir });
  const data = {
    '@context': 'https://schema.org',
    '@type': 'BreadcrumbList',
    itemListElement: crumbs.map((c, i) => ({ '@type': 'ListItem', position: i + 1, name: c.name, item: c.item })),
  };
  return JSON.stringify(data).replace(/</g, '\\u003c');
}

function layout(page, body) {
  const root = rel(page.dir, '');
  const url = SITE_URL + page.dir;
  const title = page.dir === 'docs/' ? 'Hangar documentation: self-hosted package registry' : `${page.title} · Hangar docs`;
  const desc = describe(page);
  const ogTitle = page.dir === 'docs/' ? 'Hangar documentation' : page.title;
  const crumb = page.dir === 'docs/' ? '' : `<p class="crumb">${esc(page.section)}${page.parent ? ` › <a href="${rel(page.dir, page.parent.dir)}">${esc(page.parent.label)}</a>` : ''}</p>`;
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>${esc(title)}</title>
<meta name="description" content="${esc(desc)}">
<link rel="canonical" href="${url}">
<meta name="theme-color" content="#ffffff">
<meta property="og:type" content="article">
<meta property="og:site_name" content="${esc(SITE_NAME)}">
<meta property="og:title" content="${esc(ogTitle)}">
<meta property="og:description" content="${esc(desc)}">
<meta property="og:url" content="${url}">
<meta property="og:image" content="${OG_IMAGE.url}">
<meta property="og:image:width" content="${OG_IMAGE.width}">
<meta property="og:image:height" content="${OG_IMAGE.height}">
<meta property="og:image:alt" content="${esc(OG_IMAGE.alt)}">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:title" content="${esc(ogTitle)}">
<meta name="twitter:description" content="${esc(desc)}">
<meta name="twitter:image" content="${OG_IMAGE.url}">
<link rel="icon" href="${ICON}">
<link rel="preload" href="${root}assets/fonts/manrope-latin-wght-normal.woff2" as="font" type="font/woff2" crossorigin>
<link rel="stylesheet" href="${root}assets/docs.css">
<script type="application/ld+json">${jsonLd(page)}</script>
<script src="${root}assets/docs-ui.js" defer></script>
</head>
<body data-root="${root}">
<a class="skip" href="#article">Skip to content</a>
<header class="top">
  <button class="icon-btn menu-btn" type="button" id="menu-btn" aria-label="Open navigation" aria-expanded="false" aria-controls="side">
    <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M4 7h16M4 12h16M4 17h16" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>
  </button>
  <a class="brand" href="${root}">${LOGO} Hangar <small>Docs</small></a>
  <button class="search-btn" type="button" id="search-btn" aria-haspopup="dialog">
    ${SEARCH_ICON(16)}<span>Search docs</span><kbd>⌘K</kbd>
  </button>
  <nav class="top-links" aria-label="Site">
    <a class="hide-sm" href="${root}">Home</a>
    <a class="hide-sm" href="${root}demo/">Demo</a>
    <a class="hide-sm" href="${REPO}">GitHub</a>
  </nav>
</header>
<div class="layout">
  <nav class="side" id="side" aria-label="Documentation">
${sidebar(page)}
  </nav>
  <div class="scrim" id="scrim"></div>
  <main class="content">
    <article class="article" id="article">
      ${crumb}
      <h1>${page.titleHtml}</h1>
${body}
      ${sourceLinks(page)}
      ${pager(page)}
    </article>
  </main>
  <aside class="toc" aria-label="On this page">${toc(page)}</aside>
</div>
<div class="search" id="search" role="dialog" aria-modal="true" aria-label="Search the docs">
  <div class="search-box">
    <label>
      ${SEARCH_ICON(18)}
      <span class="sr-only">Search the docs</span>
      <input id="search-input" type="search" placeholder="Search: proxy, OIDC, npm, backup…" autocomplete="off" role="combobox" aria-expanded="true" aria-controls="results" aria-autocomplete="list">
      <kbd>Esc</kbd>
    </label>
    <ul class="results" id="results" role="listbox" aria-label="Results"></ul>
  </div>
</div>
</body>
</html>
`;
}

/* ---------- write ---------- */

rmSync(join(ROOT, 'docs'), { recursive: true, force: true });

for (const page of pages) {
  // Raw HTML only in the website's own pages (<kbd>); in docs from main, text such as
  // "Settings / <page>" is text, as GitHub shows it, not an element.
  md.set({ html: page.src.startsWith('content/') });
  page.body = md.render(page.markdown, { page });
}
for (const page of pages) {
  const dir = join(ROOT, page.dir);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'index.html'), layout(page, page.body));
}

// Search index: one entry per section; text trimmed so the file stays small.
const index = {
  pages: order.map((p) => ({ u: p.dir, t: p.title, s: p.section })),
  entries: [],
};
order.forEach((p, i) => {
  for (const s of p.sections) {
    const text = s.text.replace(/\s+/g, ' ').trim();
    if (!s.id && !text) continue;
    index.entries.push([i, s.id || '', s.heading || '', text.length > 360 ? text.slice(0, 360) : text]);
  }
});
writeFileSync(join(ROOT, 'assets', 'search-index.json'), JSON.stringify(index));

// docs.html: the old single-page docs. Its #page links open the new pages.
const redirects = JSON.parse(readFileSync(join(here, 'redirects.json'), 'utf8'));
for (const [id, target] of Object.entries(redirects)) {
  if (!byDir.has(target.split('#')[0])) warn(`redirects.json: ${id} -> ${target}, which is not a page`);
}
writeFileSync(join(ROOT, 'docs.html'), `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Hangar docs have moved</title>
<meta name="robots" content="noindex">
<link rel="canonical" href="${SITE_URL}docs/">
<script>
  // The docs used to be one page with the topic in the hash (docs.html#quick-start,
  // docs.html#npm:publish). Each topic is now a page of its own.
  (function () {
    var map = ${JSON.stringify(redirects)};
    var id = decodeURIComponent(location.hash.slice(1)).split(':')[0];
    location.replace(map[id] || 'docs/');
  })();
</script>
<noscript><meta http-equiv="refresh" content="0; url=docs/"></noscript>
</head>
<body>
<p>The documentation has moved to <a href="docs/">${SITE_URL}docs/</a>.</p>
</body>
</html>
`);

// Sitemap and robots.txt.
const urls = [{ loc: SITE_URL }, ...order.map((p) => ({ loc: SITE_URL + p.dir, lastmod: SOURCE.lastmod?.[p.src] }))];
writeFileSync(join(ROOT, 'sitemap.xml'), `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
${urls.map((u) => `  <url><loc>${u.loc}</loc>${u.lastmod ? `<lastmod>${u.lastmod.slice(0, 10)}</lastmod>` : ''}</url>`).join('\n')}
</urlset>
`);
writeFileSync(join(ROOT, 'robots.txt'), `User-agent: *\nAllow: /\n\nSitemap: ${SITE_URL}sitemap.xml\n`);

// Fonts of the web UI (Manrope, JetBrains Mono), served from this site.
mkdirSync(join(ROOT, 'assets', 'fonts'), { recursive: true });
for (const [pkg, file] of [['manrope', 'manrope-latin-wght-normal.woff2'], ['manrope', 'manrope-latin-ext-wght-normal.woff2'],
  ['jetbrains-mono', 'jetbrains-mono-latin-wght-normal.woff2']]) {
  copyFileSync(join(here, 'node_modules', '@fontsource-variable', pkg, 'files', file), join(ROOT, 'assets', 'fonts', file));
}

console.log(`built ${pages.length} pages from ${SOURCE.ref}${SOURCE.commit ? ` @ ${SOURCE.commit.slice(0, 7)}` : ''}; search index ${(JSON.stringify(index).length / 1024).toFixed(0)} KiB`);
for (const w of warnings) console.warn('warning: ' + w);
if (warnings.length && process.env.STRICT) process.exit(1);
