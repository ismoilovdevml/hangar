import { useId, useState } from 'react';
import { Link, useRouterState } from '@tanstack/react-router';
import { useSession, useSystemStatus } from '../api/hooks';
import { activeNav, canSee, NAV } from '../nav';
import { initials, instanceName } from '../lib/format';
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

  const org = session.data?.organization;
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
        <InstanceBadge name={org?.name} environment={org?.environment} region={org?.region} loading={session.isPending} />

        <div className="sidebar__nav">
          {NAV.filter((section) => canSee(session.data?.privileges, section.requires)).map((section) => (
            <div className="nav-section" key={section.title}>
              <h2 className="nav-section__title">{section.title}</h2>
              <ul className="nav-section__list">
                {section.items.filter((item) => canSee(session.data?.privileges, item.requires)).map((item) => {
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
          </span>
        </Link>
      </div>
    </nav>
  );
}

interface InstanceBadgeProps {
  name?: string;
  environment?: string;
  region?: string;
  loading: boolean;
}

/** Which instance this is: a single install has nothing to switch, so it is a label, not a control. */
function InstanceBadge({ name, environment, region, loading }: InstanceBadgeProps) {
  if (loading) {
    return (
      <div className="instance-badge" aria-hidden="true">
        <span className="instance-badge__avatar instance-badge__avatar--loading" />
        <span className="skeleton" style={{ width: 96, height: 12 }} />
      </div>
    );
  }
  const display = instanceName(name);
  const meta = [environment, region].map((v) => v?.trim()).filter(Boolean).join(' · ');
  return (
    <div className="instance-badge">
      <span className="instance-badge__avatar" aria-hidden="true">
        {initials(display)}
      </span>
      <span className="instance-badge__text">
        <span className="visually-hidden">Instance: </span>
        <span className="instance-badge__name" title={display}>
          {display}
        </span>
        {meta && <span className="instance-badge__meta">{meta}</span>}
      </span>
    </div>
  );
}
