import { Link, useMatches } from '@tanstack/react-router';
import { useSession } from '../api/hooks';
import { initials, instanceName } from '../lib/format';
import { isMac, useCommandPalette } from './commandPaletteContext';
import { Disclosure } from './Disclosure';
import { IconBell, IconKey, IconLogout, IconSearch } from './icons';
import { TokenWarnings } from '../pages/access/AccountPage';

export function TopBar({ onSignOut }: { onSignOut: () => void }) {
  const palette = useCommandPalette();
  const session = useSession();
  const matches = useMatches();
  const title = [...matches].reverse().find((m) => m.staticData?.title)?.staticData?.title;
  const user = session.data?.user;
  const org = session.data?.organization;
  const shortcut = isMac() ? '⌘K' : 'Ctrl K';

  return (
    <header className="topbar glass">
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

      <button type="button" className="search-trigger" onClick={palette.open} aria-keyshortcuts="Meta+K Control+K">
        <IconSearch size={16} />
        <span className="search-trigger__text">Search packages, digests, CVEs</span>
        <kbd className="kbd">{shortcut}</kbd>
      </button>

      <Disclosure
        label="Notifications"
        panelLabel="Notifications"
        buttonClassName="icon-button icon-button--glass"
        buttonContent={<IconBell size={18} />}
      >
        <p className="popover__title">Notifications</p>
        {session.data?.token_warnings?.length ? (
          <TokenWarnings />
        ) : (
          <p className="popover__text">
            Quarantines, failed tasks and quota warnings will appear here. Nothing needs your attention.
          </p>
        )}
      </Disclosure>

      <Disclosure
        label={user ? `Account: ${user.display_name}` : 'Account'}
        panelLabel="Account"
        buttonClassName="avatar-button"
        buttonContent={user ? initials(user.display_name) : ''}
      >
        {user && (
          <p className="popover__text">
            Signed in as <strong>{user.display_name}</strong>
            <br />
            {user.email}
          </p>
        )}
        <Link to="/account" className="menu-item">
          <IconKey size={16} /> My account
        </Link>
        <button type="button" className="menu-item" onClick={onSignOut}>
          <IconLogout size={16} /> Sign out
        </button>
      </Disclosure>
    </header>
  );
}
