import { useEffect, useState } from 'react';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { RouterProvider, useRouterState, type RouterHistory } from '@tanstack/react-router';
import { setUnauthorizedHandler } from './api/client';
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
