import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { Link, useRouterState } from '@tanstack/react-router';
import { useSystemHealth } from '../api/health';
import { useSession } from '../api/hooks';
import { versionText } from '../lib/format';
import { activeNav, visibleNav } from '../nav';
import { IconClose, IconLogo, IconMenu } from './icons';
import { IconChevronRight } from './opsIcons';

const COLLAPSED_KEY = 'hangar.nav.collapsed';

/** Sidebar sections this browser keeps folded. Storage may be missing or blocked: folding then lasts until a reload. */
function readCollapsed(): string[] {
  try {
    const v: unknown = JSON.parse(window.localStorage.getItem(COLLAPSED_KEY) ?? '[]');
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

function writeCollapsed(titles: string[]) {
  try {
    window.localStorage.setItem(COLLAPSED_KEY, JSON.stringify(titles));
  } catch {
    // not persisted; folded for now
  }
}

/** Room kept clear of the fades at the ends of the list when scrolling the current page into view. */
const FADE = 32;

/** Scrolls the list just enough to show the current page's entry clear of the fades. */
function revealActive(nav: HTMLElement) {
  const item = nav.querySelector<HTMLElement>('.nav-item.is-active');
  if (!item || nav.scrollHeight <= nav.clientHeight) return;
  const box = nav.getBoundingClientRect();
  const at = item.getBoundingClientRect();
  if (at.top < box.top + FADE) nav.scrollTop -= box.top + FADE - at.top;
  else if (at.bottom > box.bottom - FADE) nav.scrollTop += at.bottom - (box.bottom - FADE);
}

/** Marks the ends of the list that have more entries beyond them. */
function markOverflow(nav: HTMLElement) {
  const below = nav.scrollHeight - nav.clientHeight - nav.scrollTop;
  nav.toggleAttribute('data-more-above', nav.scrollTop > 1);
  nav.toggleAttribute('data-more-below', below > 1);
}

export function Sidebar() {
  const bodyId = useId();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  // The mobile menu is open for the page it was opened on, so navigating closes it.
  const [openOn, setOpenOn] = useState<string | null>(null);
  const open = openOn === pathname;
  const session = useSession();
  const health = useSystemHealth();
  const sys = health.status;
  const active = activeNav(pathname);
  const sections = visibleNav(session.data?.privileges);
  const activeSection = sections.find((sec) => sec.items.some((i) => i.to === active?.to))?.title;

  // Folded sections still show the current page, unless its section was folded on this page.
  const [collapsed, setCollapsed] = useState<string[]>(readCollapsed);
  const [foldedOn, setFoldedOn] = useState<string | null>(null);
  const isOpen = (title: string) => !collapsed.includes(title) || (title === activeSection && foldedOn !== pathname);
  const toggle = (title: string) => {
    const open = isOpen(title);
    const next = open ? [...collapsed.filter((t) => t !== title), title] : collapsed.filter((t) => t !== title);
    setCollapsed(next);
    writeCollapsed(next);
    if (title === activeSection) setFoldedOn(open ? pathname : null);
  };

  const navRef = useRef<HTMLDivElement>(null);
  // Entries appear once the session says what the user may see; folding changes the list too.
  const listKey = sections.map((sec) => (isOpen(sec.title) ? sec.items.length : 0)).join(' ');

  // Keep the fades in step with the list: scrolling, a resized window, a section folded or unfolded.
  useLayoutEffect(() => {
    const nav = navRef.current;
    if (!nav) return;
    markOverflow(nav);
    const onScroll = () => markOverflow(nav);
    nav.addEventListener('scroll', onScroll, { passive: true });
    // The list's box changes when the status card below it fills in or the window resizes.
    const ro =
      typeof ResizeObserver === 'undefined'
        ? null
        : new ResizeObserver(() => {
            revealActive(nav);
            markOverflow(nav);
          });
    ro?.observe(nav);
    return () => {
      nav.removeEventListener('scroll', onScroll);
      ro?.disconnect();
    };
  }, [listKey]);

  // The current page's entry is always in view, clear of the fades.
  useEffect(() => {
    if (navRef.current) revealActive(navRef.current);
  }, [active?.to, listKey]);

  const scrollMore = () => {
    const nav = navRef.current;
    nav?.scrollBy({ top: Math.max(120, nav.clientHeight - 2 * FADE), behavior: 'smooth' });
  };

  return (
    <nav aria-label="Primary" className={open ? 'sidebar glass is-open' : 'sidebar glass'}>
      <div className="sidebar__top">
        <Link to="/" className="brand" activeOptions={{ exact: true }}>
          <span className="brand__mark">
            <IconLogo size={18} />
          </span>
          <span className="brand__text">
            <span className="brand__name">Hangar</span>
            <span className="brand__sub">Package registry</span>
          </span>
        </Link>
        <button
          type="button"
          className="icon-button sidebar__toggle"
          aria-expanded={open}
          aria-controls={bodyId}
          aria-label={open ? 'Close navigation' : 'Open navigation'}
          onClick={() => setOpenOn(open ? null : pathname)}
        >
          {open ? <IconClose size={20} /> : <IconMenu size={20} />}
        </button>
      </div>

      <div className="sidebar__body" id={bodyId}>

        <div className="sidebar__scroll">
        <div className="sidebar__nav" ref={navRef}>
          {sections.map((section) => (
            <div className="nav-section" key={section.title}>
              <h2 className="nav-section__title">
                <button
                  type="button"
                  className="nav-section__toggle"
                  aria-expanded={isOpen(section.title)}
                  aria-controls={`${bodyId}-${section.title}`}
                  onClick={() => toggle(section.title)}
                >
                  {section.title}
                  <IconChevronRight size={14} className="nav-section__chevron" />
                </button>
              </h2>
              <ul className="nav-section__list" id={`${bodyId}-${section.title}`} hidden={!isOpen(section.title)}>
                {section.items.map((item) => {
                  const on = active?.to === item.to;
                  return (
                    <li key={item.to}>
                      {/* The current page is marked "page"; a page that only belongs to this area (Settings, System) is marked "true". */}
                      <Link
                        to={item.to}
                        className={on ? 'nav-item is-active' : 'nav-item'}
                        activeOptions={{ exact: true, includeSearch: false, includeHash: false }}
                        activeProps={{}}
                        aria-current={on ? (active.own ? 'page' : 'true') : undefined}
                      >
                        <item.icon size={18} />
                        {item.label}
                      </Link>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))}
        </div>
        {/* A pointer shortcut; keyboards reach every entry with Tab, which scrolls the list. */}
        <button type="button" className="sidebar__more" tabIndex={-1} aria-hidden="true" onClick={scrollMore}>
          <IconChevronRight size={16} />
        </button>
        </div>

        <Link to="/system/status" className={`system-card system-card--${health.tone}`}>
          <span className="system-card__body" role="status" aria-live="polite">
            <span className="system-card__state">
              <span className={`status__dot dot-${health.tone}${health.tone === 'neutral' ? '' : ' status__dot--halo'}`} aria-hidden="true" />
              {health.text}
            </span>
            {sys && !health.error && (
              <span className="system-card__meta">
                {sys.nodes_up} of {sys.nodes_total} {sys.nodes_total === 1 ? 'node' : 'nodes'} · {versionText(sys.version)}
              </span>
            )}
          </span>
        </Link>
      </div>
    </nav>
  );
}

