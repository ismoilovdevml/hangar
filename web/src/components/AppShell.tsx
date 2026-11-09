import { useCallback } from 'react';
import { Outlet, useNavigate } from '@tanstack/react-router';
import { signalUnauthorized } from '../api/client';
import { LiveStatusContext, useLiveEvents } from '../api/events';
import { leaveAfterLogout, useLogout } from '../api/hooks';
import { CommandPaletteProvider } from './CommandPalette';
import { Sidebar } from './Sidebar';
import { TopBar } from './TopBar';

export function AppShell() {
  const navigate = useNavigate();
  const { mutate: logout } = useLogout();
  const live = useLiveEvents(signalUnauthorized);

  const signOut = useCallback(() => {
    logout(undefined, { onSettled: (res) => leaveAfterLogout(res, () => void navigate({ to: '/login', replace: true })) });
  }, [logout, navigate]);

  return (
    <LiveStatusContext.Provider value={live}>
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
    </LiveStatusContext.Provider>
  );
}
