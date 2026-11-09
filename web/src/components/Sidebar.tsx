import { useId, useState } from 'react';
import { Link, useRouterState } from '@tanstack/react-router';
import { useSession, useSystemStatus } from '../api/hooks';
import { versionText } from '../lib/format';
import { activeNav, visibleNav } from '../nav';
import { IconClose, IconLogo, IconMenu } from './icons';

const STATUS_TONE = { ok: 'ok', degraded: 'warn', down: 'bad' } as const;

export function Sidebar() {
  const bodyId = useId();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  // The mobile menu is open for the page it was opened on, so navigating closes it.
  const [openOn, setOpenOn] = useState<string | null>(null);
  const open = openOn === pathname;
  const session = useSession();
  const status = useSystemStatus();

  const sys = status.data;
  const active = activeNav(pathname);

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

        <div className="sidebar__nav">
          {visibleNav(session.data?.privileges).map((section) => (
            <div className="nav-section" key={section.title}>
              <h2 className="nav-section__title">{section.title}</h2>
              <ul className="nav-section__list">
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

        <Link to="/system/status" className={`system-card system-card--${sys ? STATUS_TONE[sys.state] : 'neutral'}`}>
          <span className="system-card__body" role="status" aria-live="polite">
            {sys ? (
              <>
                <span className="system-card__state">
                  <span className={`status__dot dot-${STATUS_TONE[sys.state]} status__dot--halo`} aria-hidden="true" />
                  {sys.message}
                </span>
                <span className="system-card__meta">
                  {sys.nodes_up} of {sys.nodes_total} nodes · {versionText(sys.version)}
                </span>
              </>
            ) : status.isError ? (
              <span className="system-card__state">
                <span className="status__dot dot-neutral" aria-hidden="true" />
                Status unavailable
              </span>
            ) : (
              <span className="system-card__meta">Checking system status…</span>
            )}
          </span>
        </Link>
      </div>
    </nav>
  );
}

