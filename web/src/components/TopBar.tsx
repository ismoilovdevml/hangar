import { Link, useMatches, useRouterState } from '@tanstack/react-router';
import { useSystemHealth } from '../api/health';
import { useSession } from '../api/hooks';
import { instanceName, isRange } from '../lib/format';
import { TodoAnchor } from '../pages/overview/TodoCard';
import { useTodo } from '../pages/overview/useTodo';
import { AccountMenu } from './AccountMenu';
import { isMac, useCommandPalette } from './commandPaletteContext';
import { Disclosure } from './Disclosure';
import { ErrorNote } from './ErrorNote';
import { IconBell, IconSearch } from './icons';

/** The range in the URL (the Overview's), else today: the bell then lists what the To do card does. */
function useRange() {
  const range = useRouterState({ select: (s) => (s.location.search as { range?: unknown }).range });
  return isRange(range) ? range : 'today';
}

/**
 * The installation's state in a few words, linking to the status page. It
 * reads the same health as the sidebar card and System status, and is grey
 * while that loads or cannot be read.
 */
function StatusPill() {
  const health = useSystemHealth();
  return (
    <Link to="/system/status" className={`status-pill glass status-pill--${health.tone}`} aria-busy={health.loading || undefined}>
      <span className={`status__dot dot-${health.tone}`} aria-hidden="true" />
      <span className="status-pill__text">{health.text}</span>
    </Link>
  );
}

/** The bell: the To do list, everywhere. */
function Notifications({ compact }: { compact: boolean }) {
  const todo = useTodo(useRange());
  const items = todo.items ?? [];
  const urgent = items.filter((t) => t.tone !== 'info').length;
  return (
    <Disclosure
      label={urgent ? `Notifications, ${urgent} waiting` : 'Notifications'}
      panelLabel="Notifications"
      buttonClassName={compact ? 'icon-button icon-button--glass icon-button--compact glass notify' : 'icon-button icon-button--glass notify'}
      buttonContent={
        <>
          <IconBell size={compact ? 17 : 18} />
          {urgent > 0 && <span className={`notify__dot notify__dot--${items.some((t) => t.tone === 'bad') ? 'bad' : 'warn'}`} aria-hidden="true" />}
        </>
      }
    >
      {(close) => (
        <>
          <p className="popover__title">To do</p>
          {!todo.items ? (
            todo.error ? (
              <ErrorNote what="what needs attention" onRetry={todo.retry} />
            ) : (
              <p className="popover__text">Loading…</p>
            )
          ) : items.length === 0 ? (
            <p className="popover__text">Nothing needs you right now.</p>
          ) : (
            <ul className="todo__list notify__list">
              {items.map((t) => (
                <li key={t.id} onClick={close}>
                  <TodoAnchor link={t.link} className={`todo__item todo__item--${t.tone}`}>
                    <span className="todo__tag">{t.tag}</span>
                    <span className="todo__item-title">{t.title}</span>
                    <span className="todo__detail">{t.detail}</span>
                  </TodoAnchor>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </Disclosure>
  );
}

export function TopBar({ onSignOut }: { onSignOut: () => void }) {
  const palette = useCommandPalette();
  const session = useSession();
  const matches = useMatches();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const meta = [...matches].reverse().find((m) => m.staticData?.title)?.staticData;
  const title = meta?.title;
  const org = session.data?.organization;
  const shortcut = isMac() ? '⌘K' : 'Ctrl K';
  // The Overview carries its own search box, so its header is bare and shows the system state instead.
  const bare = pathname === '/';

  return (
    <header className={bare ? 'topbar topbar--bare' : 'topbar glass'}>
      <nav aria-label="Breadcrumb" className="breadcrumb">
        <ol>
          <li>{instanceName(org?.name)}</li>
          {meta?.inSettings && (
            <li>
              <Link to="/settings" className="breadcrumb__link">
                Settings
              </Link>
            </li>
          )}
          {title && (
            <li aria-current="page" className="breadcrumb__current">
              {title}
            </li>
          )}
        </ol>
      </nav>

      {bare ? (
        <StatusPill />
      ) : (
        <button type="button" className="search-trigger" onClick={palette.open} aria-keyshortcuts="Meta+K Control+K">
          <IconSearch size={16} />
          <span className="search-trigger__text">Search packages, digests, CVEs</span>
          <kbd className="kbd">{shortcut}</kbd>
        </button>
      )}

      <Notifications compact={bare} />

      <AccountMenu onSignOut={onSignOut} compact={bare} />
    </header>
  );
}
