import { setupWorker } from 'msw/browser';
import { handlers } from './handlers';

export const worker = setupWorker(...handlers);

export function startMockWorker() {
  return worker.start({
    onUnhandledRequest(req, print) {
      // Only complain about API calls; assets and fonts pass through silently.
      if (new URL(req.url).pathname.startsWith('/api/')) print.warning();
    },
  });
}
