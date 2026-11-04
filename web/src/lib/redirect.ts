/**
 * Where to go after signing in or setting a new password: only same-origin,
 * in-app paths (path plus query string) are followed, never back to the
 * change-password page; anything else becomes "/".
 */
export function safeRedirect(target: string | undefined): string {
  if (!target || !target.startsWith('/') || target.startsWith('//') || target.startsWith('/change-password')) return '/';
  return target;
}
