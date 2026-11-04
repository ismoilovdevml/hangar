import { Link, useMatches, useRouterState } from '@tanstack/react-router';
import { useSession, useSystemStatus } from '../api/hooks';
import { instanceName } from '../lib/format';
import { AccountMenu } from './AccountMenu';
import { isMac, useCommandPalette } from './commandPaletteContext';
import { Disclosure } from './Disclosure';
import { IconBell, IconSearch } from './icons';
import { TokenWarnings } from '../pages/access/AccountPage';

const STATUS_TONE = { ok: 'ok', degraded: 'warn', down: 'bad' } as const;

/** The system state in a few words, linking to the status page; nothing while it loads. */
function StatusPill() {
  const status = useSystemStatus();
  const sys = status.data;
  if (!sys && !status.isError) return null;
  const tone = sys ? STATUS_TONE[sys.state] : 'neutral';
  return (
    <Link to="/system/status" className={`status-pill glass status-pill--${tone}`}>
      <span className={`status__dot dot-${tone}`} aria-hidden="true" />
      <span className="status-pill__text">{sys ? sys.message : 'Status unavailable'}</span>
    </Link>
  );
}

export function TopBar({ onSignOut }: { onSignOut: () => void }) {
  const palette = useCommandPalette();
  const session = useSession();
  const matches = useMatches();
  const pathname = useRouterState({ select: (s) => s.location.pathname });
  const title = [...matches].reverse().find((m) => m.staticData?.title)?.staticData?.title;
  const org = session.data?.organization;
  const shortcut = isMac() ? '⌘K' : 'Ctrl K';
  // The Overview carries its own search box, so its header is bare and shows the system state instead.
  const bare = pathname === '/';
  const waiting = session.data?.token_warnings?.length ?? 0;

  return (
    <header className={bare ? 'topbar topbar--bare' : 'topbar glass'}>
      <nav aria-label="Breadcrumb" className="breadcrumb">
        <ol>
          <li>{instanceName(org?.name)}</li>
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

      <Disclosure
        label={waiting ? `Notifications, ${waiting} waiting` : 'Notifications'}
        panelLabel="Notifications"
        buttonClassName={bare ? 'icon-button icon-button--glass icon-button--compact glass notify' : 'icon-button icon-button--glass notify'}
        buttonContent={
          <>
            <IconBell size={bare ? 17 : 18} />
            {waiting > 0 && <span className="notify__dot" aria-hidden="true" />}
          </>
        }
      >
        <p className="popover__title">Notifications</p>
        {waiting ? (
          <TokenWarnings />
        ) : (
          <p className="popover__text">
            Quarantines, failed tasks and quota warnings will appear here. Nothing needs your attention.
          </p>
        )}
      </Disclosure>

      <AccountMenu onSignOut={onSignOut} compact={bare} />
    </header>
  );
}
