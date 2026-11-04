import { ApiRequestError } from '../../api/client';

/** A timestamp spelled out in UTC, for titles next to relative times. */
export const dateTime = (iso: string) =>
  new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: 'UTC' }).format(
    new Date(iso),
  ) + ' UTC';

/** "sha256:0123abcd…ef01" */
export const shortDigest = (d: string) => {
  const [algo, hex] = d.includes(':') ? d.split(':', 2) : ['', d];
  const h = hex ?? '';
  return `${algo ? `${algo}:` : ''}${h.length > 16 ? `${h.slice(0, 8)}…${h.slice(-4)}` : h}`;
};

/** The message a failed delete shows in its dialog. */
export function deleteMessage(err: unknown, what: 'versions' | 'files'): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return `You do not have permission to delete ${what} in this repository.`;
    if (err.status === 404) return 'It was already deleted.';
    return err.body?.message ?? `The server could not delete it.`;
  }
  return 'Cannot reach the server. Check your connection and try again.';
}
