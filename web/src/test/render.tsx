import { render } from '@testing-library/react';
import { QueryClient } from '@tanstack/react-query';
import { createMemoryHistory } from '@tanstack/react-router';
import { App } from '../App';

/** Renders the whole app at `path`, backed by the MSW handlers in src/mocks. */
export function renderApp(path = '/') {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, refetchInterval: false, refetchOnWindowFocus: false } },
  });
  const history = createMemoryHistory({ initialEntries: [path] });
  const utils = render(<App queryClient={queryClient} history={history} />);
  return { ...utils, history, queryClient };
}
