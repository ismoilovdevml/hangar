import '@testing-library/jest-dom/vitest';
import { cleanup, configure } from '@testing-library/react';
import { afterAll, afterEach, beforeAll } from 'vitest';
import { resetUnsupported } from '../api/catalog';
import { resetMockState } from '../mocks/handlers';
import { server } from '../mocks/node';

// findBy*/waitFor give up after 1 s by default, which a page that waits for
// several mock responses can exceed when every CPU is busy. A real failure
// still fails; it only takes longer to report.
configure({ asyncUtilTimeout: 5_000 });

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => {
  cleanup();
  server.resetHandlers();
  resetMockState();
  resetUnsupported();
});
afterAll(() => server.close());

// jsdom lacks these; components call them defensively.
if (!Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = function scrollIntoView() {};
}
window.scrollTo = (() => {}) as typeof window.scrollTo;
