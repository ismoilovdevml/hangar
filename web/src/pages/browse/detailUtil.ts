import { ApiRequestError } from '../../api/client';
import { formatTime, formatUtc } from '../../lib/format';

/** A timestamp spelled out, for titles next to relative times: local with its zone, then UTC. */
export const dateTime = (iso: string) => `${formatTime(iso)} · ${formatUtc(iso, { seconds: false })}`;

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
