import { Link, Outlet } from '@tanstack/react-router';
import { useSession } from '../../api/hooks';
import { PageHeader } from '../../components/PageHeader';
import { canSee, SYSTEM_TABS } from '../../nav';
import './system.css';

/**
 * The System area: one header and the tabs the user may open (Status, Nodes,
 * Logs, About), above the tab's page. A user who sees only About gets no tab bar.
 */
export function SystemLayout() {
  const privileges = useSession().data?.privileges;
  const tabs = SYSTEM_TABS.filter((t) => canSee(privileges, t.requires));
  return (
    <div className="screen">
      <PageHeader title="System" description="Health, nodes, logs and version of this Hangar installation." />
      {tabs.length > 1 && (
        <nav aria-label="System sections" className="tabs tabs--glass glass">
          {tabs.map((t) => (
            <Link key={t.to} to={t.to} className="tabs__item" activeOptions={{ includeSearch: false }} activeProps={{ className: 'is-active', 'aria-current': 'page' }}>
              {t.label}
            </Link>
          ))}
        </nav>
      )}
      <Outlet />
    </div>
  );
}
