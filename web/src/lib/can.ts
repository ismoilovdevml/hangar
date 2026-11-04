/**
 * Client-side mirror of the server's privilege check, used only to hide
 * actions the user cannot take. The server still authorizes every call.
 *
 * Privileges look like `repo:<pattern>:<action>`, `system:<area>:<action>` or
 * `system:*`; `admin` and `*` imply every action (internal/core/auth.go).
 */

export type PrivilegeAction = 'read' | 'write' | 'delete' | 'admin';

/** Go path.Match subset: `*` any run of non-slash characters, `?` one character. */
function globToRegExp(glob: string): RegExp {
  let re = '';
  for (const ch of glob) {
    if (ch === '*') re += '[^/]*';
    else if (ch === '?') re += '[^/]';
    else re += ch.replace(/[.+^${}()|[\]\\]/g, '\\$&');
  }
  return new RegExp(`^${re}$`);
}

export function allows(
  privileges: readonly string[] | undefined,
  domain: 'repo' | 'system',
  name: string,
  action: PrivilegeAction,
): boolean {
  for (const p of privileges ?? []) {
    if (p === 'system:*') {
      if (domain === 'system') return true;
      continue;
    }
    const parts = p.split(':');
    if (parts.length !== 3 || parts[0] !== domain) continue;
    const [, pattern, act] = parts as [string, string, string];
    if (act !== action && act !== 'admin' && act !== '*') continue;
    if (globToRegExp(pattern).test(name)) return true;
  }
  return false;
}
