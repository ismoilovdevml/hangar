import { useCallback } from 'react';
import { Outlet, useNavigate } from '@tanstack/react-router';
import { useLiveEvents } from '../api/events';
import { useLogout } from '../api/hooks';
import { CommandPaletteProvider } from './CommandPalette';
import { Sidebar } from './Sidebar';
import { TopBar } from './TopBar';

export function AppShell() {
  const navigate = useNavigate();
  const { mutate: logout } = useLogout();
  useLiveEvents();

  const signOut = useCallback(() => {
    logout(undefined, { onSettled: () => void navigate({ to: '/login' }) });
  }, [logout, navigate]);

  return (
    <CommandPaletteProvider onSignOut={signOut}>
      <a href="#main" className="skip-link">
        Skip to content
      </a>
      <div className="app wall">
        <Sidebar />
        <div className="app__main">
          <TopBar onSignOut={signOut} />
          <main id="main" tabIndex={-1} className="app__content">
            <Outlet />
          </main>
        </div>
      </div>
    </CommandPaletteProvider>
  );
}
