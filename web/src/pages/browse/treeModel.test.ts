import { describe, expect, it } from 'vitest';
import type { BrowseEntry, BrowsePage, Repository } from '../../api/types';
import { ancestorKeys, entrySearch, flattenTree, move, nodeKey, parentPath, type FolderState } from './treeModel';

const repo = (name: string): Repository =>
  ({ id: name, name, format: 'npm', type: 'hosted', config: {}, url: '', status: 'online', size_bytes: 0, component_count: 0, created_at: '', updated_at: '' }) as Repository;
const entry = (path: string, kind: BrowseEntry['kind'] = path.endsWith('/') ? 'folder' : 'asset'): BrowseEntry => ({
  name: path.replace(/\/$/, '').split('/').pop()!,
  path,
  kind,
  repository: 'r',
});
const folder = (items: BrowseEntry[], more = false): FolderState => ({
  pages: [{ repository: 'r', format: 'npm', path: '', items, next_cursor: more ? 'c' : null } as BrowsePage],
  loading: false,
  error: false,
  hasMore: more,
});

describe('flattenTree', () => {
  const folders = new Map<string, FolderState>([
    [nodeKey('r'), folder([entry('a/'), entry('readme.txt')], true)],
    [nodeKey('r', 'a/'), folder([entry('a/b/'), entry('a/x.tgz')])],
    [nodeKey('r', 'a/b/'), folder([])],
  ]);
  const input = (expanded: string[], filter = '') => ({
    repos: [repo('r'), repo('s')],
    expanded: new Set(expanded),
    folder: (r: string, p: string) => folders.get(nodeKey(r, p)),
    filter,
  });
  const names = (rows: ReturnType<typeof flattenTree>) => rows.map((r) => (r.type === 'repo' ? r.repo.name : r.type === 'entry' ? r.entry.path : r.type));

  it('shows expanded folders with levels, positions and status rows', () => {
    const rows = flattenTree(input([nodeKey('r'), nodeKey('r', 'a/'), nodeKey('r', 'a/b/'), nodeKey('s')]));
    expect(names(rows)).toEqual(['r', 'a/', 'a/b/', 'empty', 'a/x.tgz', 'readme.txt', 'more', 's', 'loading']);
    const ab = rows[2]!;
    expect(ab.level).toBe(3);
    expect(ab.type === 'entry' && [ab.posInSet, ab.setSize]).toEqual([1, 2]);
    const s = rows[7]!;
    expect(s.type === 'repo' && [s.posInSet, s.setSize]).toEqual([2, 2]);
  });

  it('filters by name, keeping the ancestors of loaded matches', () => {
    const rows = flattenTree(input([nodeKey('r'), nodeKey('r', 'a/')], 'x.t'));
    expect(names(rows)).toEqual(['r', 'a/', 'a/x.tgz']);
    expect(names(flattenTree(input([nodeKey('r')], 's')))).toEqual(['s']);
  });

  it('moves focus over focusable rows only', () => {
    const rows = flattenTree(input([nodeKey('r'), nodeKey('r', 'a/'), nodeKey('r', 'a/b/')]));
    const empty = names(rows).indexOf('empty');
    expect(names(rows)[move(rows, empty - 1, 'next')]).toBe('a/x.tgz');
    expect(names(rows)[move(rows, names(rows).indexOf('a/x.tgz'), 'parent')]).toBe('a/');
    expect(names(rows)[move(rows, 3, 'last')]).toBe('s');
    expect(names(rows)[move(rows, 3, 'first')]).toBe('r');
    expect(move(rows, 0, 'prev')).toBe(-1);
  });
});

describe('paths', () => {
  it('lists the folders to open for a node', () => {
    expect(ancestorKeys('r', 'a/b/')).toEqual([nodeKey('r'), nodeKey('r', 'a/'), nodeKey('r', 'a/b/')]);
    expect(ancestorKeys('r', 'a/b/c.txt')).toEqual([nodeKey('r'), nodeKey('r', 'a/'), nodeKey('r', 'a/b/')]);
    expect(ancestorKeys('r', undefined)).toEqual([nodeKey('r')]);
    expect(parentPath('a/b/c.txt')).toBe('a/b/');
    expect(parentPath('a/b/')).toBe('a/');
    expect(parentPath('a/')).toBe('');
  });

  it('builds the URL of an entry, naming the member of a group that holds it', () => {
    const c = { id: 'c1', namespace: '@acme', name: 'ui', version: '1.0.0' };
    expect(entrySearch('npm-all', { ...entry('@acme/ui/', 'package'), repository: 'npm-internal', component: c })).toEqual({
      repo: 'npm-all',
      path: '@acme/ui/',
      member: 'npm-internal',
      ns: '@acme',
      name: 'ui',
    });
    expect(entrySearch('r', { ...entry('g/a/1.0/', 'version'), repository: 'r', component: { ...c, namespace: '' } })).toMatchObject({ version: '1.0.0', member: undefined, ns: undefined });
    expect(entrySearch('r', { ...entry('f.txt'), asset: { id: 'a1' } as BrowseEntry['asset'] })).toEqual({ repo: 'r', path: 'f.txt', asset: 'a1' });
  });
});
