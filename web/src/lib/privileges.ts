/**
 * Client-side mirror of the server's privilege check (core.Privilege.Allows),
 * used only to hide or disable actions the signed-in user cannot take. The
 * server stays the authority: every mutation is authorized again there.
 *
 * Privilege strings: "repo:<glob>:<action>", "system:<area>:<action>" or the
 * short form "system:*". Actions: read, write, delete, admin, *; admin and *
 * imply every other action on the same resource.
 */

export type Action = 'read' | 'write' | 'delete' | 'admin';
export type Domain = 'repo' | 'system';

interface Parsed {
  domain: string;
  pattern: string;
  action: string;
}

function parse(p: string): Parsed | null {
  if (p === 'system:*') return { domain: 'system', pattern: '*', action: '*' };
  const first = p.indexOf(':');
  const last = p.lastIndexOf(':');
  if (first <= 0 || last === first) return null;
  return { domain: p.slice(0, first), pattern: p.slice(first + 1, last), action: p.slice(last + 1) };
}

/**
 * Go's path.Match for names without "/": `*` any run, `?` one character,
 * `[...]` / `[^...]` classes with ranges, `\` escapes. Malformed patterns match nothing.
 */
export function globMatch(pattern: string, name: string): boolean {
  let re = '^';
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i]!;
    if (c === '*') re += '[^/]*';
    else if (c === '?') re += '[^/]';
    else if (c === '\\') {
      const n = pattern[++i];
      if (n === undefined) return false;
      re += n.replace(/[.*+?^${}()|[\]\\/-]/g, '\\$&');
    } else if (c === '[') {
      const end = pattern.indexOf(']', i + 2);
      if (end < 0) return false;
      let body = pattern.slice(i + 1, end);
      const negate = body.startsWith('^');
      if (negate) body = body.slice(1);
      if (!body) return false;
      re += `[${negate ? '^' : ''}${body.replace(/[\]\\]/g, '\\$&')}]`;
      i = end;
    } else re += c.replace(/[.*+?^${}()|[\]\\/-]/g, '\\$&');
  }
  try {
    return new RegExp(`${re}$`).test(name);
  } catch {
    return false;
  }
}

/** Whether any of `privileges` grants `action` on the resource `domain:name`. */
export function can(privileges: readonly string[], action: Action, domain: Domain, name: string): boolean {
  return privileges.some((raw) => {
    const p = parse(raw);
    if (!p || p.domain !== domain) return false;
    if (!globMatch(p.pattern, name)) return false;
    return p.action === action || p.action === 'admin' || p.action === '*';
  });
}

/** A repository can be configured or deleted by repo admins or by system:repos admins. */
export function canAdminRepo(privileges: readonly string[], repo: string): boolean {
  return can(privileges, 'admin', 'system', 'repos') || can(privileges, 'admin', 'repo', repo);
}
