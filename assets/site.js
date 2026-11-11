/* Hangar website: gentle scroll animations, tabs, copy. No dependencies. */
(function () {
  'use strict';
  var root = document.documentElement;
  root.classList.add('js');
  var reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
  var clamp = function (v, a, b) { return Math.max(a, Math.min(b, v)); };

  /* copy */
  function copyText(text, btn) {
    var done = function () { btn.textContent = 'Copied'; setTimeout(function () { btn.textContent = 'Copy'; }, 1400); };
    if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(done, function () { select(btn); });
    else select(btn);
  }
  function select(btn) {
    var el = btn.parentElement.querySelector('code, pre:not([hidden])'); if (!el) return;
    var r = document.createRange(); r.selectNodeContents(el); var s = getSelection(); s.removeAllRanges(); s.addRange(r);
    btn.textContent = 'Press ⌘C';
  }
  $$('[data-copy]').forEach(function (b) { b.addEventListener('click', function () { copyText($(b.dataset.copy).textContent.trim(), b); }); });

  /* install tabs */
  var tabs = $$('.tabs [role="tab"]');
  tabs.forEach(function (t) {
    t.addEventListener('click', function () {
      tabs.forEach(function (x) { x.setAttribute('aria-selected', x === t ? 'true' : 'false'); });
      $$('.code pre').forEach(function (p) { p.hidden = p.dataset.tab !== t.dataset.tab; });
    });
  });
  var cb = $('[data-copy-active]');
  if (cb) cb.addEventListener('click', function () { copyText($$('.code pre').filter(function (p) { return !p.hidden; })[0].textContent.trim(), cb); });


  /* clickable product tour: hotspots in % of the 1600x1000 screenshots */
  var view = $('#app-view'), spots = $('#hotspots'), pathEl = $('#app-path'), hint = $('#app-hint');
  if (view && spots) {
    var box = function (x, y, w, h, go, label) { return { x: x, y: y, w: w, h: h, go: go, label: label }; };
    var SIDEBAR = [
      box(34, 224, 234, 44, 'overview', 'Overview'),
      box(34, 270, 234, 46, 'packages', 'Packages'),
      box(34, 318, 234, 44, 'packages', 'Search'),
      box(34, 600, 234, 44, 'vulnerabilities', 'Vulnerabilities'),
      box(34, 646, 234, 44, 'authentication', 'Access and audit'),
      box(34, 788, 234, 44, 'status', 'Cluster and replication'),
      box(34, 886, 234, 72, 'status', 'System status')
    ];
    var EXTRA = {
      overview: [
        box(1290, 24, 176, 38, 'status', 'All systems normal'),
        box(520, 160, 844, 60, 'packages', 'Search packages'),
        box(627, 652, 307, 168, 'vulnerabilities', 'Security'),
        box(950, 652, 308, 168, 'proxies', 'Upstream'),
        box(304, 388, 630, 248, 'packages', 'Today'),
        box(459, 860, 263, 34, 'packages', 'microsoft.entityframeworkcore')
      ],
      packages: [box(322, 98, 640, 46, 'packages', 'Filter packages')],
      vulnerabilities: [],
      status: [],
      authentication: [],
      proxies: []
    };
    var PATHS = { overview: '', packages: 'browse', vulnerabilities: 'vulnerabilities', status: 'system/status', authentication: 'settings/authentication', proxies: 'settings/proxies' };
    var current = 'overview';
    function show(name) {
      current = name;
      $$('img', view).forEach(function (im) { im.classList.toggle('on', im.dataset.view === name); });
      pathEl.textContent = PATHS[name] || '';
      spots.innerHTML = SIDEBAR.concat(EXTRA[name] || []).filter(function (b) { return !(b.go === name && b.label !== 'Overview' && SIDEBAR.indexOf(b) === -1); }).map(function (b) {
        return '<button type="button" data-go="' + b.go + '" aria-label="' + b.label + '" style="left:' + (b.x / 16) + '%;top:' + (b.y / 10) + '%;width:' + (b.w / 16) + '%;height:' + (b.h / 10) + '%"></button>';
      }).join('');
    }
    spots.addEventListener('click', function (e) {
      var b = e.target.closest('button'); if (!b) return;
      if (hint) hint.classList.add('gone');
      spots.classList.remove('teach');
      show(b.dataset.go);
    });
    show('overview');
    if ('IntersectionObserver' in window && !reduce) {
      var to = new IntersectionObserver(function (es) { if (es[0].isIntersecting) { spots.classList.add('teach'); to.disconnect(); } }, { threshold: 0.6 });
      to.observe(view);
    }
  }

  /* reveal below the fold */
  if ('IntersectionObserver' in window && !reduce) {
    var io = new IntersectionObserver(function (es) {
      es.forEach(function (e) {
        if (!e.isIntersecting) return;
        var el = e.target, i = Array.prototype.indexOf.call(el.parentElement.children, el);
        setTimeout(function () { el.classList.remove('pending'); }, Math.min(i, 4) * 90);
        io.unobserve(el);
      });
    }, { rootMargin: '0px 0px -10% 0px' });
    $$('.reveal').forEach(function (el) { if (el.getBoundingClientRect().top > innerHeight * 0.95) { el.classList.add('pending'); io.observe(el); } });
  }

  /* count-up numbers */
  if ('IntersectionObserver' in window && !reduce) {
    var co = new IntersectionObserver(function (es) {
      es.forEach(function (e) {
        if (!e.isIntersecting) return;
        var el = e.target, to = +el.dataset.count, t0 = performance.now();
        (function step(t) { var k = clamp((t - t0) / 900, 0, 1); el.textContent = Math.round(to * (1 - Math.pow(1 - k, 3))); if (k < 1) requestAnimationFrame(step); })(t0);
        co.unobserve(el);
      });
    }, { threshold: 0.6 });
    $$('[data-count]').forEach(function (el) { co.observe(el); });
  }

  /* scroll-linked: progress, nav border, screenshot tilt, flow lines */
  var nav = $('#nav'), shot = $('#app'), flow = $('#flow');
  var nodes = flow ? $$('.node', flow) : [];
  var ticking = false;
  function frame() {
    ticking = false;
    var vh = innerHeight, max = root.scrollHeight - vh;
    root.style.setProperty('--p', max > 0 ? (scrollY / max).toFixed(4) : 0);
    nav.classList.toggle('scrolled', scrollY > 8);
    if (reduce) { if (flow) { flow.style.setProperty('--fill', 1); flow.style.setProperty('--fill2', 1); } return; }
    if (shot) {
      var r = shot.getBoundingClientRect();
      var k = clamp((r.top - vh * 0.12) / (vh * 0.55), 0, 1);   // 1 = still below, 0 = in place
      shot.style.setProperty('--tilt', k.toFixed(3));
      root.style.setProperty('--sun', clamp(scrollY / (vh * 0.9), 0, 1).toFixed(3));
    }
    if (flow) {
      var f = flow.getBoundingClientRect();
      var t = clamp((vh * 0.85 - f.top) / (vh * 0.5), 0, 1);  // 0 → 1 while the diagram crosses the screen
      flow.style.setProperty('--fill', clamp(t * 2, 0, 1).toFixed(3));
      flow.style.setProperty('--fill2', clamp(t * 2 - 1, 0, 1).toFixed(3));
      nodes[0].classList.toggle('lit', t > 0.02 && t < 0.5);
      nodes[1].classList.toggle('lit', t >= 0.45);
      nodes[2].classList.toggle('lit', t >= 0.98);
    }
  }
  addEventListener('scroll', function () { if (!ticking) { ticking = true; requestAnimationFrame(frame); } }, { passive: true });
  addEventListener('resize', frame);
  frame();
})();
