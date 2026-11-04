import { useEffect, useState } from 'react';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { RouterProvider, useRouterState, type RouterHistory } from '@tanstack/react-router';
import { setPasswordChangeHandler, setUnauthorizedHandler } from './api/client';
import { queryKeys } from './api/hooks';
import { createQueryClient } from './lib/queryClient';
import { createAppRouter } from './router';

function DocumentTitle() {
  const title = useRouterState({
    select: (s) => [...s.matches].reverse().find((m) => m.staticData?.title)?.staticData?.title,
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
  // out in another tab): forget cached data and ask for sign-in, then come back.
  useEffect(
    () =>
      setUnauthorizedHandler(() => {
        const loc = router.state.location;
        if (loc.pathname === '/login') return;
        qc.clear();
        void router.navigate({ to: '/login', search: { redirect: loc.href }, replace: true });
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
        </>
      )} />
    </QueryClientProvider>
  );
}
