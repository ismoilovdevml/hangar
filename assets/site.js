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
    var el = btn.parentElement.querySelector('code, pre'); if (!el) return;
    var r = document.createRange(); r.selectNodeContents(el); var s = getSelection(); s.removeAllRanges(); s.addRange(r);
    btn.textContent = 'Press ⌘C';
  }
  $$('[data-copy]').forEach(function (b) { b.addEventListener('click', function () { copyText($(b.dataset.copy).textContent.trim(), b); }); });

  /* install tabs: click or arrow keys, one panel visible */
  var tabs = $$('.tabs [role="tab"]');
  function pick(t) {
    tabs.forEach(function (x) {
      var on = x === t;
      x.setAttribute('aria-selected', on ? 'true' : 'false');
      x.tabIndex = on ? 0 : -1;
      document.getElementById(x.getAttribute('aria-controls')).hidden = !on;
    });
  }
  tabs.forEach(function (t, i) {
    t.addEventListener('click', function () { pick(t); });
    t.addEventListener('keydown', function (e) {
      var d = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
      if (!d) return;
      e.preventDefault();
      var n = tabs[(i + d + tabs.length) % tabs.length];
      pick(n);
      n.focus();
    });
  });
  $$('.panels .copy').forEach(function (b) { b.addEventListener('click', function () { copyText(b.parentElement.querySelector('pre').textContent.trim(), b); }); });


  /* live demo: load the frame when it comes near, fit its 1440x900 page to the frame, mirror its address */
  var demoFrame = $('#app-frame'), view = $('#app-view'), pathEl = $('#app-path');
  if (demoFrame && view) {
    var wide = window.matchMedia('(min-width: 761px)');
    var fit = function () { view.style.setProperty('--k', (view.clientWidth / 1440).toFixed(4)); };
    var load = function () { if (!demoFrame.getAttribute('src') && wide.matches) demoFrame.src = demoFrame.dataset.src; };
    fit();
    addEventListener('resize', fit);
    if ('IntersectionObserver' in window) {
      var fo = new IntersectionObserver(function (es) { if (es[0].isIntersecting && wide.matches) { load(); fo.disconnect(); } }, { rootMargin: '600px 0px' });
      fo.observe(view);
    } else load();
    if (wide.addEventListener) wide.addEventListener('change', function () { fit(); load(); });
    setInterval(function () {
      if (!demoFrame.getAttribute('src')) return;
      try { pathEl.textContent = demoFrame.contentWindow.location.pathname.replace(/^.*?\/demo\/?/, ''); } catch (e) { /* not loaded yet */ }
    }, 500);
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
