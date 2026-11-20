/**
 * The one client-side mirror of the server's privilege check, used only to
 * hide or disable what the signed-in user cannot do. The server stays the
 * authority: every call is authorized again there.
 *
 * It follows internal/core/auth.go: ParsePrivilege for the string form and
 * Privilege.Allows for the check, whose pattern match is Go's path.Match.
 *
 * Privilege strings: "repo:<pattern>:<action>", "system:<area>:<action>" or
 * the short form "system:*". Actions: read, write, delete, admin and *; admin
 * and * imply every other action on the same resource.
 */

export type Action = 'read' | 'write' | 'delete' | 'admin';
export type Domain = 'repo' | 'system';

export interface Privilege {
  domain: Domain;
  pattern: string;
  action: Action | '*';
}

const ACTIONS = new Set<string>(['read', 'write', 'delete', 'admin', '*']);

/* ---------------------------------------------------------------- path.Match */

/** A pattern Go's path.Match rejects (ErrBadPattern). */
class BadPattern extends Error {}

/**
 * The next chunk of `p` from `i`: a run of stars, then the text up to the
 * next star outside a character class (Go: scanChunk).
 */
function scanChunk(p: string[], i: number): { star: boolean; chunk: string[]; next: number } {
  let star = false;
  while (i < p.length && p[i] === '*') {
    i++;
    star = true;
  }
  let inRange = false;
  let j = i;
  for (; j < p.length; j++) {
    const c = p[j];
    if (c === '\\') {
      if (j + 1 < p.length) j++;
    } else if (c === '[') inRange = true;
    else if (c === ']') inRange = false;
    else if (c === '*' && !inRange) break;
  }
  return { star, chunk: p.slice(i, j), next: j };
}

/** A possibly escaped character of a class, and where the class continues (Go: getEsc). */
function getEsc(chunk: string[], i: number): { r: number; next: number } {
  if (i >= chunk.length || chunk[i] === '-' || chunk[i] === ']') throw new BadPattern();
  if (chunk[i] === '\\') {
    i++;
    if (i >= chunk.length) throw new BadPattern();
  }
  const r = chunk[i]!.codePointAt(0)!;
  i++;
  if (i >= chunk.length) throw new BadPattern();
  return { r, next: i };
}

/**
 * Whether `chunk` (no stars) matches the start of `s` from `at`; the index
 * after the match, or -1. Throws BadPattern for a malformed chunk, also after
 * the match has failed (Go: matchChunk).
 */
function matchChunk(chunk: string[], s: string[], at: number): number {
  let failed = false;
  let k = at;
  let i = 0;
  while (i < chunk.length) {
    failed = failed || k >= s.length;
    const c = chunk[i];
    if (c === '[') {
      let r = -1;
      if (!failed) r = s[k++]!.codePointAt(0)!;
      i++;
      let negated = false;
      if (i < chunk.length && chunk[i] === '^') {
        negated = true;
        i++;
      }
      let match = false;
      let ranges = 0;
      for (;;) {
        if (i < chunk.length && chunk[i] === ']' && ranges > 0) {
          i++;
          break;
        }
        const lo = getEsc(chunk, i);
        let hi = lo;
        if (chunk[lo.next] === '-') hi = getEsc(chunk, lo.next + 1);
        i = hi.next;
        match = match || (lo.r <= r && r <= hi.r);
        ranges++;
      }
      failed = failed || match === negated;
    } else if (c === '?') {
      if (!failed) failed = s[k++] === '/';
      i++;
    } else {
      if (c === '\\') {
        i++;
        if (i >= chunk.length) throw new BadPattern();
      }
      if (!failed) failed = chunk[i] !== s[k++];
      i++;
    }
  }
  return failed ? -1 : k;
}

/**
 * Go's path.Match: `*` any run of characters except "/", `?` one character
 * except "/", `[...]` and `[^...]` classes with ranges, and `\` escapes.
 * `error` is true for a malformed pattern, which the server refuses.
 */
export function pathMatch(pattern: string, name: string): { match: boolean; error: boolean } {
  const p = Array.from(pattern);
  const s = Array.from(name);
  let pi = 0;
  let si = 0;
  try {
    outer: while (pi < p.length) {
      const { star, chunk, next } = scanChunk(p, pi);
      pi = next;
      if (star && chunk.length === 0) return { match: !s.slice(si).includes('/'), error: false };
      let t = -1;
      let err: unknown = null;
      try {
        t = matchChunk(chunk, s, si);
      } catch (e) {
        err = e;
      }
      // The last chunk must use up the name; an earlier one may leave the rest to the next star.
      if (t >= 0 && (t === s.length || pi < p.length)) {
        si = t;
        continue;
      }
      if (err) throw err;
      if (star) {
        for (let i = si; i < s.length && s[i] !== '/'; i++) {
          const u = matchChunk(chunk, s, i + 1);
          if (u >= 0) {
            if (pi >= p.length && u < s.length) continue;
            si = u;
            continue outer;
          }
        }
      }
      // No match: the rest of the pattern must still be well formed.
      while (pi < p.length) {
        const rest = scanChunk(p, pi);
        pi = rest.next;
        matchChunk(rest.chunk, [], 0);
      }
      return { match: false, error: false };
    }
    return { match: si === s.length, error: false };
  } catch (e) {
    if (e instanceof BadPattern) return { match: false, error: true };
    throw e;
  }
}

/** Whether `name` matches the path.Match `pattern`; a malformed pattern matches nothing. */
export function globMatch(pattern: string, name: string): boolean {
  return pathMatch(pattern, name).match;
}

/* ---------------------------------------------------------------- privileges */

/** Parses a privilege the way the server does (core.ParsePrivilege); null when the server would refuse it. */
export function parsePrivilege(s: string): Privilege | null {
  const parts = s.split(':');
  if (parts.length === 2 && parts[0] === 'system' && parts[1] === '*') return { domain: 'system', pattern: '*', action: '*' };
  if (parts.length !== 3) return null;
  const [domain, pattern, action] = parts as [string, string, string];
  if (domain !== 'repo' && domain !== 'system') return null;
  if (!pattern || pathMatch(pattern, '').error) return null;
  if (!ACTIONS.has(action)) return null;
  return { domain, pattern, action: action as Action | '*' };
}

/** Whether one parsed privilege grants `action` on `domain:name` (core.Privilege.Allows). */
export function privilegeAllows(p: Privilege, domain: Domain, name: string, action: Action): boolean {
  if (p.domain !== domain || !globMatch(p.pattern, name)) return false;
  return p.action === action || p.action === 'admin' || p.action === '*';
}

/** Whether any of `privileges` grants `action` on the resource `domain:name`. Malformed entries grant nothing. */
export function allows(privileges: readonly string[] | undefined, domain: Domain, name: string, action: Action): boolean {
  return (privileges ?? []).some((raw) => {
    const p = parsePrivilege(raw);
    return p !== null && privilegeAllows(p, domain, name, action);
  });
}

/** A repository can be configured or deleted by repo admins or by system:repos admins. */
export function canAdminRepo(privileges: readonly string[], repo: string): boolean {
  return allows(privileges, 'system', 'repos', 'admin') || allows(privileges, 'repo', repo, 'admin');
}
