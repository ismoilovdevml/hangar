/**
 * Stand-in for the `tldts` package in the demo build (see vite.config.ts).
 * The mock server's cookie jar only asks for the registrable domain of the
 * page's own host; a last-two-labels answer is enough for that, and it keeps
 * the full public suffix list out of the website's download.
 */
export function getDomain(hostname: string): string | null {
  const host = hostname.replace(/\.$/, '').toLowerCase();
  if (!host || /^[\d.]+$/.test(host) || host.includes(':')) return null;
  const labels = host.split('.');
  return labels.length < 2 ? null : labels.slice(-2).join('.');
}
