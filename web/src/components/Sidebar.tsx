import { useId, useState } from 'react';
import { Link, useRouterState } from '@tanstack/react-router';
import { useSession, useSystemStatus } from '../api/hooks';
import { NAV } from '../nav';
import { IconChevronUpDown, IconClose, IconLogo, IconMenu } from './icons';

const STATUS_TONE = { ok: 'ok', degraded: 'warn', down: 'bad' } as const;

export function Sidebar() {
  const bodyId = useId();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  // The mobile menu is open for the page it was opened on, so navigating closes it.
  const [openOn, setOpenOn] = useState<string | null>(null);
  const open = openOn === pathname;
  const session = useSession();
  const status = useSystemStatus();

  const org = session.data?.organization;
  const sys = status.data;

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
        <button type="button" className="org-switcher" aria-label={org ? `Organization: ${org.name}. Switch organization` : 'Switch organization'}>
          <span className="org-switcher__avatar" aria-hidden="true">
            {(org?.name ?? '··').slice(0, 2).toUpperCase()}
          </span>
          <span className="org-switcher__text">
            <span className="org-switcher__name">{org?.name ?? 'Loading…'}</span>
            <span className="org-switcher__meta">{org ? `${org.environment} · ${org.region}` : ' '}</span>
          </span>
          <IconChevronUpDown />
        </button>

        {NAV.map((section) => (
          <div className="nav-section" key={section.title}>
            <h2 className="nav-section__title">{section.title}</h2>
            <ul className="nav-section__list">
              {section.items.map((item) => (
                <li key={item.to}>
                  <Link
                    to={item.to}
                    className="nav-item"
                    activeOptions={{ exact: item.to === '/' }}
                    activeProps={{ className: 'is-active' }}
                  >
                    <item.icon size={18} />
                    {item.label}
                  </Link>
                </li>
              ))}
            </ul>
          </div>
        ))}

        <div className="system-card" role="status" aria-live="polite">
          {sys ? (
            <>
              <span className="system-card__state">
                <span className={`status__dot dot-${STATUS_TONE[sys.state]} status__dot--halo`} aria-hidden="true" />
                {sys.message}
              </span>
              <span className="system-card__meta">
                {sys.nodes_up} of {sys.nodes_total} nodes · v{sys.version}
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
        </div>
      </div>
    </nav>
  );
}
