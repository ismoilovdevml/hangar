import { QueryClient } from '@tanstack/react-query';
import { ApiRequestError } from '../api/client';

export function createQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 15_000,
        refetchOnWindowFocus: true,
        // Client errors (4xx) will not fix themselves; retry only server/network failures.
        retry: (count, err) => !(err instanceof ApiRequestError && err.status < 500) && count < 2,
      },
    },
  });
}
