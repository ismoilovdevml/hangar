import { lazy, Suspense, useEffect, useState } from 'react';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { RouterProvider, useRouterState, type RouterHistory } from '@tanstack/react-router';
import { setPasswordChangeHandler, setUnauthorizedHandler } from './api/client';
import { queryKeys } from './api/hooks';
import type { Session } from './api/types';
import { requestReauth } from './lib/reauth';
import { createQueryClient } from './lib/queryClient';
import { createAppRouter } from './router';

/** Only the demo build (`npm run build:demo`) has the note; the product bundle drops it. */
const DemoNote = import.meta.env.VITE_DEMO === 'true' ? lazy(() => import('./demo/DemoNote')) : null;

function DocumentTitle() {
  // The page first, then the area it sits in: "Logs · System · Hangar".
  const title = useRouterState({
    select: (s) =>
      [...s.matches]
        .reverse()
        .flatMap((m) => (m.staticData?.title ? [m.staticData.title] : []))
        .join(' · '),
  });
  useEffect(() => {
    document.title = title ? `${title} · Hangar` : 'Hangar';
  }, [title]);
  return null;
}

export function App({ queryClient, history }: { queryClient?: QueryClient; history?: RouterHistory }) {
  const [qc] = useState(() => queryClient ?? createQueryClient());
  const [router] = useState(() => createAppRouter(qc, history));

  // Any API call answering 401 means the session ended (expired, revoked, signed
  // out in another tab). Inside the app the user signs in again over the page,
  // which keeps what they were editing; elsewhere cached data is dropped and
  // the sign-in page says why, then comes back.
  useEffect(
    () =>
      setUnauthorizedHandler(() => {
        const loc = router.state.location;
        if (loc.pathname === '/login') return;
        const session = qc.getQueryData<Session>(queryKeys.session);
        if (session && loc.pathname !== '/change-password' && !session.password_change_required && session.auth_method !== 'token') {
          requestReauth(session.user.username);
          return;
        }
        qc.clear();
        void router.navigate({ to: '/login', search: { redirect: loc.href, reason: 'expired' }, replace: true });
      }),
    [qc, router],
  );

  // 403 password_change_required: the account must set a new password before
  // anything else (an administrator asked for it while this tab was open).
  useEffect(
    () =>
      setPasswordChangeHandler(() => {
        const loc = router.state.location;
        if (loc.pathname === '/change-password' || loc.pathname === '/login') return;
        void qc.invalidateQueries({ queryKey: queryKeys.session });
        void router.navigate({ to: '/change-password', search: { redirect: loc.href }, replace: true });
      }),
    [qc, router],
  );

  return (
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} InnerWrap={({ children }) => (
        <>
          <DocumentTitle />
          {children}
          {DemoNote && (
            <Suspense fallback={null}>
              <DemoNote />
            </Suspense>
          )}
        </>
      )} />
    </QueryClientProvider>
  );
}
