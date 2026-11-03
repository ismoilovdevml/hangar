import { useEffect, useState } from 'react';
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query';
import { RouterProvider, useRouterState, type RouterHistory } from '@tanstack/react-router';
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
