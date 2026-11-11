import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import './styles/index.css';

// A tab opened before an upgrade asks for page scripts the server no longer
// has: load the new release instead of failing. At most once a minute, so a
// broken deployment shows its error instead of reloading forever.
window.addEventListener('vite:preloadError', (event) => {
  const key = 'hangar:stale-reload';
  try {
    if (Date.now() - Number(sessionStorage.getItem(key) ?? 0) < 60_000) return;
    sessionStorage.setItem(key, String(Date.now()));
  } catch {
    return;
  }
  event.preventDefault();
  window.location.reload();
});

async function enableMocks() {
  if (import.meta.env.VITE_API_MOCKS !== 'true') return;
  const { startMockWorker } = await import('./mocks/browser');
  await startMockWorker();
}

void enableMocks().then(() => {
  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
});
