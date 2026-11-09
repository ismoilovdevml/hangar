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

