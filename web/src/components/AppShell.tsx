import { useCallback } from 'react';
import { Outlet, useMatches, useNavigate } from '@tanstack/react-router';
import { signalUnauthorized } from '../api/client';
import { LiveStatusContext, useLiveEvents } from '../api/events';
import { leaveAfterLogout, useLogout, useSession } from '../api/hooks';
import { passes } from '../lib/gates';
import { NoAccessPage } from './NoAccess';
import { CommandPaletteProvider } from './CommandPalette';
import { Sidebar } from './Sidebar';
import { TopBar } from './TopBar';

export function AppShell() {
  const navigate = useNavigate();
  const { mutate: logout } = useLogout();
  const live = useLiveEvents(signalUnauthorized);
  const privileges = useSession().data?.privileges;
  // The innermost page that names a gate decides; a refused page is never rendered, so it makes no calls.
  const page = useMatches({ select: (ms) => [...ms].reverse().find((m) => m.staticData?.gate)?.staticData });
  const refused = page?.gate && !passes(page.gate, privileges) ? page : undefined;

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
              {refused?.gate ? <NoAccessPage title={refused.title ?? 'No access'} gate={refused.gate} /> : <Outlet />}
            </main>
          </div>
        </div>
      </CommandPaletteProvider>
    </LiveStatusContext.Provider>
  );
}
