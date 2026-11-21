import { errorText } from '../../lib/apiError';
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
  return errorText(err, { action: `delete ${what} in this repository`, status: { 404: 'It was already deleted.' } });
}
