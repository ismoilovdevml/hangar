/* Hangar docs: navigation drawer, search, copy buttons, screenshots, table of contents.
   The pages themselves are static HTML built by site/build.mjs; this only adds behaviour. */
(function () {
  'use strict';
  var root = document.body.getAttribute('data-root') || './';
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
  var esc = function (s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); };

  /* ---------- navigation drawer (narrow screens) ---------- */
  var menuBtn = $('#menu-btn');
  function setMenu(open) {
    document.body.classList.toggle('menu', open);
    menuBtn.setAttribute('aria-expanded', String(open));
    menuBtn.setAttribute('aria-label', open ? 'Close navigation' : 'Open navigation');
  }
  menuBtn.addEventListener('click', function () { setMenu(!document.body.classList.contains('menu')); });
  $('#scrim').addEventListener('click', function () { setMenu(false); });
  // Keep the current page visible in a long sidebar, without scrolling the page itself.
  var side = $('#side'), current = $('.side a[aria-current="page"]');
  if (current) side.scrollTop = current.offsetTop - side.clientHeight / 2;

  /* ---------- copy buttons on code blocks ---------- */
  $$('.code-copy').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var text = btn.closest('.code').querySelector('pre').innerText.replace(/\n$/, '');
      var done = function (label) { btn.textContent = label; setTimeout(function () { btn.textContent = 'Copy'; }, 1400); };
      if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(function () { done('Copied'); }, function () { done('Select and copy'); });
      else done('Select and copy');
    });
  });

  /* ---------- screenshots open large ---------- */
  var opener = null;
  function closeBox() {
    var box = $('.lightbox');
    if (!box) return;
    box.remove();
    if (opener) opener.focus();
  }
  $$('.zoom').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var img = btn.querySelector('img');
      opener = btn;
      var box = document.createElement('div');
      box.className = 'lightbox';
      box.setAttribute('role', 'dialog');
      box.setAttribute('aria-modal', 'true');
      box.setAttribute('aria-label', img.alt);
      box.tabIndex = -1;
      box.innerHTML = '<img src="' + esc(img.currentSrc || img.src) + '" alt="' + esc(img.alt) + '">';
      box.addEventListener('click', closeBox);
      document.body.appendChild(box);
      box.focus();
    });
  });

  /* ---------- table of contents follows the reading position ---------- */
  var tocLinks = $$('.toc a');
  if (tocLinks.length && 'IntersectionObserver' in window) {
    var byId = {};
    tocLinks.forEach(function (a) { byId[decodeURIComponent(a.hash.slice(1))] = a; });
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (e) {
        if (!e.isIntersecting || !byId[e.target.id]) return;
        tocLinks.forEach(function (a) { a.classList.remove('on'); });
        byId[e.target.id].classList.add('on');
      });
    }, { rootMargin: '-60px 0px -70% 0px' });
    $$('.article h2[id], .article h3[id]').forEach(function (h) { io.observe(h); });
  }

  /* ---------- search ---------- */
  var dlg = $('#search'), input = $('#search-input'), list = $('#results'), searchBtn = $('#search-btn');
  var index = null, loading = null, hits = [], sel = 0, lastFocus = null;

  function load() {
    if (!loading) {
      loading = fetch(root + 'assets/search-index.json').then(function (r) {
        if (!r.ok) throw new Error(r.status);
        return r.json();
      }).then(function (data) {
        index = data.entries.map(function (e) {
          var p = data.pages[e[0]];
          return { url: p.u + (e[1] ? '#' + e[1] : ''), page: p.t, section: p.s, heading: e[2], text: e[3], top: !e[1],
            hay: (p.t + ' ' + e[2]).toLowerCase(), body: e[3].toLowerCase() };
        });
      });
    }
    return loading;
  }
  function openSearch() {
    lastFocus = document.activeElement;
    dlg.classList.add('open');
    input.focus();
    input.select();
    list.innerHTML = '<li class="empty">Loading the index…</li>';
    load().then(function () { find(input.value); }, function () {
      list.innerHTML = '<li class="empty">The search index could not be loaded. Use the navigation on the left.</li>';
    });
  }
  function closeSearch() {
    dlg.classList.remove('open');
    if (lastFocus && lastFocus.focus) lastFocus.focus();
  }
  function mark(text, words) {
    var out = esc(text);
    words.forEach(function (w) {
      if (w.length < 2) return;
      out = out.replace(new RegExp('(' + esc(w).replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + ')', 'ig'), '<mark>$1</mark>');
    });
    return out;
  }
  function find(q) {
    var words = q.toLowerCase().split(/\s+/).filter(Boolean);
    if (!index) return;
    if (!words.length) {
      list.innerHTML = '<li class="empty">Type to search every page: a command, a setting, an error message.</li>';
      hits = [];
      return;
    }
    hits = index.map(function (x) {
      var score = 0;
      for (var i = 0; i < words.length; i++) {
        var w = words[i], inHead = x.hay.indexOf(w);
        if (inHead !== -1) score += inHead === 0 ? 12 : 8;
        else if (x.body.indexOf(w) !== -1) score += 2;
        else return null;
      }
      if (x.top) score += 1;
      return { x: x, score: score };
    }).filter(Boolean).sort(function (a, b) { return b.score - a.score; }).slice(0, 12);
    sel = 0;
    if (!hits.length) {
      list.innerHTML = '<li class="empty">No results for “' + esc(q) + '”. Try “proxy”, “token” or “backup”.</li>';
      return;
    }
    list.innerHTML = hits.map(function (h, i) {
      var x = h.x, at = x.body.indexOf(words[0]);
      var snip = at < 0 ? x.text.slice(0, 110) : (at > 40 ? '…' : '') + x.text.slice(Math.max(0, at - 40), at + 80) + '…';
      var title = x.heading ? x.page + ' › ' + x.heading : x.page;
      return '<li role="presentation"><a role="option" id="hit' + i + '" aria-selected="' + (i === 0) + '" class="' + (i === 0 ? 'sel' : '') + '" href="' + esc(root + x.url) + '">' +
        '<span class="sec">' + esc(x.section) + '</span><br>' + mark(title, words) + (x.text ? '<small>' + mark(snip, words) + '</small>' : '') + '</a></li>';
    }).join('');
    input.setAttribute('aria-activedescendant', 'hit0');
  }
  function move(d) {
    var links = $$('a', list);
    if (!links.length) return;
    links[sel].classList.remove('sel');
    links[sel].setAttribute('aria-selected', 'false');
    sel = (sel + d + links.length) % links.length;
    links[sel].classList.add('sel');
    links[sel].setAttribute('aria-selected', 'true');
    input.setAttribute('aria-activedescendant', links[sel].id);
    links[sel].scrollIntoView({ block: 'nearest' });
  }
  input.addEventListener('input', function () { find(input.value); });
  input.addEventListener('keydown', function (e) {
    if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
    else if (e.key === 'Enter') { var a = $$('a', list)[sel]; if (a) { e.preventDefault(); closeSearch(); location.href = a.href; } }
  });
  list.addEventListener('click', function (e) { if (e.target.closest('a')) closeSearch(); });
  dlg.addEventListener('click', function (e) { if (e.target === dlg) closeSearch(); });
  searchBtn.addEventListener('click', openSearch);

  document.addEventListener('keydown', function (e) {
    var open = dlg.classList.contains('open');
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); if (open) closeSearch(); else openSearch(); }
    else if (e.key === '/' && !open && !/INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName)) { e.preventDefault(); openSearch(); }
    else if (e.key === 'Escape') {
      if (open) closeSearch();
      else if ($('.lightbox')) closeBox();
      else if (document.body.classList.contains('menu')) { setMenu(false); menuBtn.focus(); }
    }
  });
})();
