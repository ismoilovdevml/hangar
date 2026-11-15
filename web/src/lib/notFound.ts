/** Addresses people guess, and the page they were after. */
export function notFoundSuggestion(path: string): { label: string; to: '/browse'; search?: { repo: string } } | null {
  const p = path.replace(/\/+$/, '');
  if (p === '/packages' || p === '/components') return { label: 'Packages', to: '/browse' };
  const m = /^\/(?:browse|packages)\/([^/]+)$/.exec(p);
  if (m) {
    const repo = decodeURIComponent(m[1]!);
    return { label: `the packages of ${repo}`, to: '/browse', search: { repo } };
  }
  return null;
}
