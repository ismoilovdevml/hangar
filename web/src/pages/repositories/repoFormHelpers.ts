import { ApiRequestError } from '../../api/client';
import { cleanServerMessage } from '../../lib/apiError';
import type { RepoFormErrors, RepoFormField } from '../../lib/repositories';

/* Element ids and focus handling shared by the repository create and settings forms. */

export const fieldId = (f: RepoFormField) => `repo-${f}`;
export const errorId = (f: RepoFormField) => `repo-${f}-error`;

/** Order in which errors are reported and the first invalid field is focused. */
export const FIELD_ORDER: RepoFormField[] = [
  'format', 'type', 'name', 'blobStore', 'remoteUrl', 'metadataTtl', 'contentTtl', 'negativeTtl',
  'authUsername', 'authPassword', 'authHeader', 'authToken', 'proxyName', 'proxyUrl', 'caBundle', 'members', 'writeMember',
  'relatedHosts', 'aptArchitectures', 'rpmRepodataDepth', 'goSumdbs', 'cargoDownloadUrl',
];

/** Focuses the first field with an error; returns whether there was one. */
export function focusFirstError(errors: RepoFormErrors): boolean {
  const first = FIELD_ORDER.find((f) => errors[f]);
  if (!first) return false;
  const el = document.getElementById(fieldId(first)) ?? document.querySelector<HTMLElement>(`[name="${first}"]`);
  el?.focus();
  return true;
}

export const invalidProps = (field: RepoFormField, errors: RepoFormErrors, hintId?: string) => ({
  id: fieldId(field),
  'aria-invalid': errors[field] ? true : undefined,
  'aria-describedby': [errors[field] ? errorId(field) : '', hintId ?? ''].filter(Boolean).join(' ') || undefined,
});


/**
 * A save the server refused, mapped onto the field it is about, in words: the
 * server's message names the field inside a Go error chain
 * (`repository "x": remote url …: blocked by outbound address policy`).
 * Empty when the error is about no particular field.
 */
export function serverFieldErrors(err: unknown): RepoFormErrors {
  if (!(err instanceof ApiRequestError) || err.status !== 400) return {};
  const msg = err.body?.message ?? '';
  if (/blocked by outbound address policy/i.test(msg)) {
    const host = /address ([^\s:]+(?::\d+)?)\s*:\s*blocked/i.exec(msg)?.[1];
    return {
      remoteUrl: `${host ? `${host} is` : 'This address is'} blocked by the outbound address policy, which keeps proxies away from loopback, private, link-local and cloud metadata addresses. Use a public registry URL, or ask an administrator to allow it (http.allow_private_networks).`,
    };
  }
  const remote = /remote[ _]url\b[^:]*:\s*(.+)$/i.exec(msg);
  if (remote) return { remoteUrl: cleanServerMessage(remote[1]!) };
  if (/blob[ _]store/i.test(msg)) return { blobStore: cleanServerMessage(msg.replace(/^repository "[^"]*":\s*/, '')) };
  return {};
}
