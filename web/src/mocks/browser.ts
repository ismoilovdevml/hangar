import { setupWorker } from 'msw/browser';
import { handlers } from './handlers';

export const worker = setupWorker(...handlers);

export function startMockWorker() {
  return worker.start({
    // The worker sits next to index.html, so its scope follows the build's base path.
    serviceWorker: { url: `${import.meta.env.BASE_URL}mockServiceWorker.js` },
    // The public demo keeps the console clean; local mock mode logs each request.
    quiet: import.meta.env.VITE_DEMO === 'true',
    onUnhandledRequest(req, print) {
      // Only complain about API calls; assets and fonts pass through silently.
      if (new URL(req.url).pathname.startsWith('/api/')) print.warning();
    },
  });
}
