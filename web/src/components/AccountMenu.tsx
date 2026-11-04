import { useEffect, useId, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from 'react';
import { Link } from '@tanstack/react-router';
import { useSession } from '../api/hooks';
import { initials } from '../lib/format';
import { IconKey, IconLogout, IconShield, IconUser } from './icons';
import { IconSettings } from './opsIcons';

const ITEM = '[role="menuitem"]';

/**
 * The avatar in the top bar and the account menu it opens: who is signed in,
 * links to their account, tokens and sign-in security, Settings, and signing out.
 * Menu-button pattern: arrows move between items, Escape closes and returns
 * focus to the avatar, Tab closes and moves on.
 */
export function AccountMenu({ onSignOut, compact }: { onSignOut: () => void; compact?: boolean }) {
  const session = useSession();
  const [open, setOpen] = useState(false);
  // Which item takes focus when the menu opens: the first, or the last (ArrowUp on the avatar).
  const [initial, setInitial] = useState<'first' | 'last'>('first');
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const menuId = useId();
  const labelId = useId();

  const s = session.data;
  const user = s?.user;

  useEffect(() => {
    if (!open) return;
    const items = menuRef.current?.querySelectorAll<HTMLElement>(ITEM);
    const target = initial === 'last' ? items?.[items.length - 1] : items?.[0];
    target?.focus();
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open, initial]);

  const show = (at: 'first' | 'last') => {
    setInitial(at);
    setOpen(true);
  };
  const close = (refocus: boolean) => {
    setOpen(false);
    if (refocus) buttonRef.current?.focus();
  };

  const onButtonKey = (e: ReactKeyboardEvent) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      show(e.key === 'ArrowUp' ? 'last' : 'first');
    }
  };

  const onMenuKey = (e: ReactKeyboardEvent) => {
    const items = [...(menuRef.current?.querySelectorAll<HTMLElement>(ITEM) ?? [])];
    const i = items.indexOf(document.activeElement as HTMLElement);
    const focusAt = (n: number) => items[(n + items.length) % items.length]?.focus();
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        focusAt(i + 1);
        break;
      case 'ArrowUp':
        e.preventDefault();
        focusAt(i < 0 ? -1 : i - 1);
        break;
      case 'Home':
        e.preventDefault();
        focusAt(0);
        break;
      case 'End':
        e.preventDefault();
        focusAt(-1);
        break;
      case 'Escape':
        e.preventDefault();
        e.stopPropagation();
        close(true);
        break;
      case 'Tab':
        close(false);
        break;
    }
  };

  const local = (user?.source ?? 'local') === 'local' && user?.kind !== 'service_account';
  const viaToken = s?.auth_method === 'token';
  const role = user?.roles?.[0];
  const label = user ? `Account: ${user.display_name}` : 'Account';

  return (
    <div className="account" ref={rootRef}>
      <button
        ref={buttonRef}
        type="button"
        className={['avatar-button', compact ? 'avatar-button--compact' : '', open ? 'is-open' : ''].filter(Boolean).join(' ')}
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => (open ? close(false) : show('first'))}
        onKeyDown={onButtonKey}
      >
        {user ? initials(user.display_name) : ''}
      </button>
      {open && (
        <div ref={menuRef} id={menuId} className="account-menu" role="menu" aria-labelledby={labelId} onKeyDown={onMenuKey}>
          <div className="account-menu__head">
            <span className="account-menu__avatar" aria-hidden="true">
              {user ? initials(user.display_name) : ''}
            </span>
            <span className="account-menu__who">
              <span className="account-menu__name" id={labelId}>
                {user?.display_name ?? 'Account'}
              </span>
              {user?.email && <span className="account-menu__email">{user.email}</span>}
            </span>
            {role && <span className="account-menu__role pill tone-violet">{role}</span>}
          </div>
          <div className="account-menu__group" role="group" aria-label="Account">
            <MenuLink icon={<IconUser size={16} />} to="/account" onPick={() => close(false)}>
              My account
            </MenuLink>
            <MenuLink icon={<IconKey size={16} />} to="/account" hash="my-tokens-title" onPick={() => close(false)}>
              Access tokens
            </MenuLink>
            {!viaToken && (
              <MenuLink
                icon={<IconShield size={16} />}
                to="/account"
                hash={local ? 'password-title' : 'my-sessions-title'}
                onPick={() => close(false)}
              >
                {local ? 'Password & sessions' : 'Signed-in devices'}
              </MenuLink>
            )}
          </div>
          <div className="account-menu__group" role="group" aria-label="Settings">
            <MenuLink icon={<IconSettings size={16} />} to="/settings" onPick={() => close(false)}>
              Settings
            </MenuLink>
          </div>
          <div className="account-menu__group" role="group" aria-label="Session">
            <button
              type="button"
              role="menuitem"
              tabIndex={-1}
              className="account-menu__item account-menu__item--danger"
              onClick={() => {
                close(false);
                onSignOut();
              }}
            >
              <span className="account-menu__tile" aria-hidden="true">
                <IconLogout size={16} />
              </span>
              Sign out
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

function MenuLink({ icon, to, hash, onPick, children }: { icon: ReactNode; to: '/account' | '/settings'; hash?: string; onPick: () => void; children: ReactNode }) {
  return (
    <Link to={to} hash={hash} role="menuitem" tabIndex={-1} className="account-menu__item" onClick={onPick}>
      <span className="account-menu__tile" aria-hidden="true">
        {icon}
      </span>
      {children}
    </Link>
  );
}
