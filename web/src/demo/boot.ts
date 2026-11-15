/**
 * Demo build only (`npm run build:demo`). A static host has no fallback to
 * index.html, so links into the demo use the hash form `demo/#/repositories`;
 * this turns that into the path the router reads before the router starts.
 */
export function openHashLink(base: string = import.meta.env.BASE_URL, loc: Location = window.location, history: History = window.history): void {
  if (!loc.hash.startsWith('#/')) return;
  history.replaceState(null, '', `${base.replace(/\/$/, '')}${loc.hash.slice(1)}`);
}
