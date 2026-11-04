/**
 * The browse tree as a flat list of visible rows (what the virtualised
 * ARIA tree renders), plus the keyboard moves over it. Pure functions:
 * the component supplies the loaded folders and the expanded set.
 */
import type { BrowseEntry, BrowsePage, Repository } from '../../api/types';
import type { BrowseSearch } from '../../router';

/** Key of a container node: a repository root or a folder in it. */
export const nodeKey = (repo: string, path = '') => (path ? `${repo}\u0000${path}` : repo);

export function splitKey(key: string): { repo: string; path: string } {
  const i = key.indexOf('\u0000');
  return i < 0 ? { repo: key, path: '' } : { repo: key.slice(0, i), path: key.slice(i + 1) };
}

/** What the tree knows about one container's children. */
export interface FolderState {
  pages: BrowsePage[];
  loading: boolean;
  error: boolean;
  hasMore: boolean;
}

interface RowBase {
  key: string;
  level: number;
}

export type Row =
  | (RowBase & { type: 'repo'; repo: Repository; expanded: boolean; setSize: number; posInSet: number })
  | (RowBase & { type: 'entry'; repo: string; entry: BrowseEntry; expanded: boolean; container: boolean; setSize: number; posInSet: number })
  | (RowBase & { type: 'more' | 'retry' | 'loading' | 'empty'; repo: string; path: string });

export const isContainerEntry = (e: BrowseEntry) => e.kind !== 'asset';

/** Rows that keyboard focus can land on. */
export const isFocusable = (r: Row) => r.type === 'repo' || r.type === 'entry' || r.type === 'more' || r.type === 'retry';

export interface FlattenInput {
  repos: Repository[];
  expanded: ReadonlySet<string>;
  folder: (repo: string, path: string) => FolderState | undefined;
  /** Lower-case filter text; empty shows everything. */
  filter: string;
}

/**
 * The visible rows. With a filter, a node is shown when its name matches
 * (with its expanded subtree unfiltered) or when a loaded descendant matches.
 */
export function flattenTree({ repos, expanded, folder, filter }: FlattenInput): Row[] {
  const matches = (name: string) => name.toLowerCase().includes(filter);

  const children = (repo: string, path: string, level: number, filtering: boolean): Row[] => {
    const st = folder(repo, path);
    const status = (type: 'more' | 'retry' | 'loading' | 'empty'): Row => ({ type, key: `${nodeKey(repo, path)}\u0001${type}`, level, repo, path });
    if (!st || (st.loading && st.pages.length === 0)) return filtering ? [] : [status(st?.error ? 'retry' : 'loading')];
    const groups: Row[][] = [];
    for (const entry of st.pages.flatMap((p) => p.items)) {
      const container = isContainerEntry(entry);
      const key = nodeKey(repo, entry.path);
      const open = container && expanded.has(key);
      const row: Row = { type: 'entry', key, level, repo, entry, expanded: open, container, setSize: 0, posInSet: 0 };
      if (!filtering || matches(entry.name)) {
        groups.push([row, ...(open ? children(repo, entry.path, level + 1, false) : [])]);
        continue;
      }
      if (open) {
        const sub = children(repo, entry.path, level + 1, true);
        if (sub.length) groups.push([row, ...sub]);
      }
    }
    number(groups);
    const out = groups.flat();
    if (filtering) return out;
    if (st.error) out.push(status('retry'));
    else if (st.loading) out.push(status('loading'));
    else if (st.hasMore) out.push(status('more'));
    else if (groups.length === 0) out.push(status('empty'));
    return out;
  };

  const groups: Row[][] = [];
  for (const repo of repos) {
    const key = nodeKey(repo.name);
    const open = expanded.has(key);
    const row: Row = { type: 'repo', key, level: 1, repo, expanded: open, setSize: 0, posInSet: 0 };
    if (!filter || matches(repo.name)) {
      groups.push([row, ...(open ? children(repo.name, '', 2, false) : [])]);
      continue;
    }
    if (open) {
      const sub = children(repo.name, '', 2, true);
      if (sub.length) groups.push([row, ...sub]);
    }
  }
  number(groups);
  return groups.flat();
}

/** Sets aria-setsize / aria-posinset on the first row of every sibling group. */
function number(groups: Row[][]) {
  groups.forEach((g, i) => {
    const head = g[0];
    if (head && (head.type === 'repo' || head.type === 'entry')) {
      head.setSize = groups.length;
      head.posInSet = i + 1;
    }
  });
}

/** Folders that must be expanded to show `path` (a folder ends in "/", a file does not). */
export function ancestorKeys(repo: string, path: string | undefined): string[] {
  const keys = [nodeKey(repo)];
  if (!path) return keys;
  const parts = path.split('/');
  // "a/b/" -> a/, a/b/ ; "a/b/c.txt" -> a/, a/b/
  for (let i = 1; i < parts.length; i++) keys.push(nodeKey(repo, `${parts.slice(0, i).join('/')}/`));
  return keys;
}

/** The parent folder of a tree path ("" for a top-level node). */
export function parentPath(path: string): string {
  const trimmed = path.endsWith('/') ? path.slice(0, -1) : path;
  const i = trimmed.lastIndexOf('/');
  return i < 0 ? '' : trimmed.slice(0, i + 1);
}

export type Move = 'next' | 'prev' | 'first' | 'last' | 'parent';

/** Index of the row a key press moves focus to, or -1. */
export function move(rows: Row[], from: number, how: Move): number {
  const step = (start: number, dir: 1 | -1) => {
    for (let i = start; i >= 0 && i < rows.length; i += dir) if (isFocusable(rows[i]!)) return i;
    return -1;
  };
  switch (how) {
    case 'next':
      return step(from + 1, 1);
    case 'prev':
      return step(from - 1, -1);
    case 'first':
      return step(0, 1);
    case 'last':
      return step(rows.length - 1, -1);
    case 'parent': {
      const level = rows[from]?.level ?? 1;
      for (let i = from - 1; i >= 0; i--) if (rows[i]!.level < level && isFocusable(rows[i]!)) return i;
      return -1;
    }
  }
}

/** A short stable DOM id for a row key (keys hold arbitrary path characters). */
export function rowId(prefix: string, key: string): string {
  let h = 2166136261;
  for (let i = 0; i < key.length; i++) h = Math.imul(h ^ key.charCodeAt(i), 16777619) >>> 0;
  return `${prefix}-${h.toString(36)}-${key.length.toString(36)}`;
}

/** The URL that shows a tree entry of repository `repo`. */
export function entrySearch(repo: string, e: BrowseEntry): BrowseSearch {
  const base: BrowseSearch = { repo, path: e.path };
  const c = e.component;
  const member = e.repository !== repo ? e.repository : undefined;
  switch (e.kind) {
    case 'asset':
      return { ...base, asset: e.asset?.id };
    case 'package':
      return c ? { ...base, member, ns: c.namespace || undefined, name: c.name } : base;
    case 'version':
      return c ? { ...base, member, ns: c.namespace || undefined, name: c.name, version: c.version } : base;
    default:
      return base;
  }
}

