import { describe, expect, it } from 'vitest';
import type { BrowseEntry, BrowsePage, Repository } from '../../api/types';
import type { PublishedComponent } from '../../api/types';
import { ancestorKeys, entrySearch, flattenTree, move, nodeKey, packageTreePath, parentPath, versionKey, versionSearch, type FolderState } from './treeModel';

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

describe('packages', () => {
  const v = (version: string, repository = 'r'): PublishedComponent => ({ id: version, version, name: 'ui', namespace: '@acme', repository }) as PublishedComponent;
  const pkg = { ...entry('@acme/ui/', 'package'), component: { id: 'c', namespace: '@acme', name: 'ui', version: '2.0.0' } };
  const folders = new Map<string, FolderState>([
    [nodeKey('r'), folder([pkg])],
    [nodeKey('r', '@acme/ui/'), folder([{ ...entry('@acme/ui/v2/', 'package'), component: { id: 'd', namespace: '@acme', name: 'ui/v2', version: '1' } }])],
  ]);
  const names = (rows: ReturnType<typeof flattenTree>) =>
    rows.map((r) => (r.type === 'repo' ? r.repo.name : r.type === 'entry' ? r.entry.path : r.type === 'version' ? `v${r.version.version}` : r.type));

  it('expands a package to its versions, newest first, then the packages nested in it', () => {
    const rows = flattenTree({
      repos: [repo('r')],
      expanded: new Set([nodeKey('r'), nodeKey('r', '@acme/ui/')]),
      folder: (r, p) => folders.get(nodeKey(r, p)),
      versions: (k) => (k === nodeKey('r', '@acme/ui/') ? { items: [v('2.0.0'), v('1.0.0')], loading: false, error: false } : undefined),
      filter: '',
    });
    expect(names(rows)).toEqual(['r', '@acme/ui/', 'v2.0.0', 'v1.0.0', '@acme/ui/v2/']);
    const newest = rows[2]!;
    expect(newest.type === 'version' && [newest.key, newest.level, newest.newest, newest.posInSet, newest.setSize]).toEqual([
      versionKey(nodeKey('r', '@acme/ui/'), '2.0.0'),
      3,
      true,
      1,
      3,
    ]);
  });

  it('says when a package has no versions and while they load', () => {
    const base = { repos: [repo('r')], expanded: new Set([nodeKey('r'), nodeKey('r', '@acme/ui/')]), filter: '' };
    const only = new Map([[nodeKey('r'), folder([pkg])], [nodeKey('r', '@acme/ui/'), folder([])]]);
    const folderOf = (r: string, p: string) => only.get(nodeKey(r, p));
    expect(names(flattenTree({ ...base, folder: folderOf, versions: () => ({ items: [], loading: true, error: false }) }))).toEqual(['r', '@acme/ui/', 'loading']);
    const done = flattenTree({ ...base, folder: folderOf, versions: () => ({ items: [], loading: false, error: false }) });
    expect(done.at(-1)).toMatchObject({ type: 'empty', pkg: true });
  });

  it('finds the folder of a package named by a link', () => {
    expect(packageTreePath('npm', '@acme', 'ui')).toBe('@acme/ui/');
    expect(packageTreePath('maven', 'com.acme', 'ledger-core')).toBe('com/acme/ledger-core/');
    expect(packageTreePath('pypi', '', 'Acme_ML.utils')).toBe('acme-ml-utils/');
    expect(packageTreePath('nuget', '', 'Acme.Core')).toBe('acme.core/');
    expect(packageTreePath('docker', 'team', 'app')).toBe('team/app/');
    expect(packageTreePath('raw', 'releases/1.4.0', 'app.tgz')).toBe('releases/1.4.0/app.tgz');
  });

  it('links a version row, naming the group member that holds it', () => {
    expect(versionSearch('npm-all', pkg, v('1.0.0', 'npm-internal'))).toEqual({
      repo: 'npm-all',
      path: '@acme/ui/',
      member: 'npm-internal',
      ns: '@acme',
      name: 'ui',
      version: '1.0.0',
    });
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
