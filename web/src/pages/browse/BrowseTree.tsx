import { memo, useCallback, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { Link } from '@tanstack/react-router';
import { useQueries, useQueryClient, type UseQueryResult } from '@tanstack/react-query';
import { browseFolderQuery, browseKeys } from '../../api/browse';
import { catalogKeys, componentLookupQuery, useAllRepositories } from '../../api/catalog';
import type { BrowseEntry, BrowsePage, PublishedComponent, PublishedComponentPage, Repository } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { formatBytes } from '../../lib/format';
import { TYPE_META } from '../../lib/repositories';
import { FormatIcon } from '../../components/FormatIcon';
import {
  ancestorKeys,
  flattenTree,
  isFocusable,
  move,
  nodeKey,
  packageTreePath,
  rowId,
  splitKey,
  versionKey,
  type FolderState,
  type Row,
  type VersionState,
} from './treeModel';

/** Row height in pixels; rows are fixed-height so the list can be virtualised. */
export const ROW_HEIGHT = 34;
/** Rows rendered above and below the viewport. */
const OVERSCAN = 12;
/** Viewport height assumed before the element is measured (and in test DOMs). */
const FALLBACK_VIEWPORT = 640;

export type TreeSelection =
  | { kind: 'repo'; repo: Repository }
  | { kind: 'entry'; repo: string; entry: BrowseEntry }
  | { kind: 'version'; repo: string; entry: BrowseEntry; version: PublishedComponent };

interface BrowseTreeProps {
  /** Selected repository and node path, from the URL. */
  repo?: string;
  path?: string;
  /** The package shown (a link may name it without a tree path), and its version. */
  ns?: string;
  name?: string;
  version?: string;
  onSelect: (s: TreeSelection) => void;
}

/** What the tree reads of a query. */
interface Slice<T> {
  data: T | undefined;
  pending: boolean;
  error: boolean;
}

/**
 * `combine` for useQueries: React Query re-runs it only when a result changed
 * and keeps the previous array when the slices are equal, so the memoised tree
 * below is rebuilt when data arrives, not on every render (a scroll, say).
 */
const folderSlices = (rs: UseQueryResult<BrowsePage>[]): Slice<BrowsePage>[] =>
  rs.map((r) => ({ data: r.data, pending: r.isPending && !r.isError, error: r.isError }));
const lookupSlices = (rs: UseQueryResult<PublishedComponentPage>[]): Slice<PublishedComponentPage>[] =>
  rs.map((r) => ({ data: r.data, pending: r.isPending && !r.isError, error: r.isError }));

/** Versions of one package, merged over the members that hold it (first member wins), newest first. */
function mergeVersions(lists: (PublishedComponent[] | undefined)[], ns: string): PublishedComponent[] {
  const seen = new Map<string, PublishedComponent>();
  for (const list of lists) for (const c of list ?? []) if ((c.namespace ?? '') === ns && !seen.has(c.version)) seen.set(c.version, c);
  return [...seen.values()].sort((a, b) => (a.published_at < b.published_at ? 1 : a.published_at > b.published_at ? -1 : 0));
}

/**
 * Left pane: repositories at the root, each a lazily loaded tree of what
 * clients consume: folders down to packages, and each package's versions.
 * Stored files are on the repository's Files view (raw repositories, whose
 * files are their content, show them here). An ARIA tree with one focus
 * stop (aria-activedescendant), so that only the rows in view need to exist
 * in the DOM.
 */
export function BrowseTree({ repo, path, ns, name, version, onSelect }: BrowseTreeProps) {
  const qc = useQueryClient();
  const repos = useAllRepositories();
  const treeId = useId();
  const filterId = useId();
  const [filter, setFilter] = useState('');
  // A link to a package names it, not its folder: find the folder from the format's layout.
  const format = repos.data?.find((r) => r.name === repo)?.format;
  const treePath = path ?? (name && format ? packageTreePath(format, ns, name) : undefined);
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set(repo ? ancestorKeys(repo, treePath) : []));
  /** Pages after the first that were asked for, per container key. */
  const [more, setMore] = useState<Record<string, string[]>>({});
  const [active, setActive] = useState<string | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  /** The exact scroll offset; only the first row in view is state, so scrolling within a row renders nothing. */
  const scrollTop = useRef(0);
  const [topRow, setTopRow] = useState(0);
  const [viewport, setViewport] = useState(FALLBACK_VIEWPORT);

  const pkgKey = repo ? nodeKey(repo, treePath) : null;
  const selectedKey = pkgKey && version && treePath?.endsWith('/') ? versionKey(pkgKey, version) : pkgKey;

  // A selection made elsewhere (a link, the back button) opens its folders
  // (a package: its versions) and takes the keyboard focus.
  const [followed, setFollowed] = useState(selectedKey);
  if (followed !== selectedKey) {
    setFollowed(selectedKey);
    if (repo) {
      const need = ancestorKeys(repo, treePath).filter((k) => !expanded.has(k));
      if (need.length) setExpanded(new Set([...expanded, ...need]));
    }
    if (selectedKey) setActive(selectedKey);
  }

  const sorted = useMemo(() => [...(repos.data ?? [])].sort((a, b) => a.name.localeCompare(b.name)), [repos.data]);

  const requests = useMemo(
    () =>
      [...expanded].flatMap((key) => {
        const { repo: r, path: p } = splitKey(key);
        return [undefined, ...(more[key] ?? [])].map((cursor) => ({ key, repo: r, path: p, cursor }));
      }),
    [expanded, more],
  );
  const results = useQueries({ queries: requests.map((q) => browseFolderQuery(q.repo, q.path, q.cursor, 'packages')), combine: folderSlices });

  const folders = useMemo(() => {
    const map = new Map<string, FolderState>();
    requests.forEach((q, i) => {
      const res = results[i];
      const st = map.get(q.key) ?? { pages: [], loading: false, error: false, hasMore: false };
      if (res?.data) st.pages.push(res.data);
      st.loading ||= !!res?.pending;
      st.error ||= !!res?.error;
      map.set(q.key, st);
    });
    for (const st of map.values()) st.hasMore = !!st.pages[st.pages.length - 1]?.next_cursor;
    return map;
  }, [requests, results]);

  // Expanded packages list their versions, from every member that holds them.
  const packages = useMemo(() => {
    const out: { key: string; entry: BrowseEntry; holders: string[] }[] = [];
    for (const [parent, st] of folders) {
      const r = splitKey(parent).repo;
      for (const e of st.pages.flatMap((p) => p.items)) {
        const key = nodeKey(r, e.path);
        if (e.kind === 'package' && e.component && expanded.has(key)) out.push({ key, entry: e, holders: e.repositories ?? [e.repository] });
      }
    }
    return out;
  }, [folders, expanded]);
  const lookups = useMemo(
    () => packages.flatMap((p) => p.holders.map((h) => ({ key: p.key, q: componentLookupQuery(h, p.entry.component!.name) }))),
    [packages],
  );
  const lookupResults = useQueries({ queries: lookups.map((l) => l.q), combine: lookupSlices });
  const versions = useMemo(() => {
    const map = new Map<string, VersionState>();
    for (const p of packages) {
      const mine = lookups.flatMap((l, i) => (l.key === p.key && lookupResults[i] ? [lookupResults[i]] : []));
      map.set(p.key, {
        items: mergeVersions(
          mine.map((x) => x.data?.items),
          p.entry.component?.namespace ?? '',
        ),
        loading: mine.some((x) => x.pending),
        error: mine.some((x) => x.error),
      });
    }
    return map;
  }, [packages, lookups, lookupResults]);

  const rows = useMemo(
    () =>
      flattenTree({
        repos: sorted,
        expanded,
        folder: (r, p) => folders.get(nodeKey(r, p)),
        versions: (k) => versions.get(k),
        filter: filter.trim().toLowerCase(),
      }),
    [sorted, expanded, folders, versions, filter],
  );
  const positions = useMemo(() => new Map(rows.map((r, i) => [r.key, i])), [rows]);
  const indexOf = useCallback((key: string | null) => (key ? (positions.get(key) ?? -1) : -1), [positions]);

  const measure = useCallback(() => {
    const el = scrollRef.current;
    if (el && el.clientHeight > 0) setViewport(el.clientHeight);
  }, []);
  useEffect(() => {
    measure();
    window.addEventListener('resize', measure);
    return () => window.removeEventListener('resize', measure);
  }, [measure]);

  const onScrollTo = useCallback((top: number) => {
    scrollTop.current = top;
    setTopRow(Math.floor(top / ROW_HEIGHT));
  }, []);

  const reveal = useCallback(
    (index: number) => {
      const el = scrollRef.current;
      const top = index * ROW_HEIGHT;
      const current = scrollTop.current;
      let next = current;
      if (top < current) next = top;
      else if (top + ROW_HEIGHT > current + viewport) next = top + ROW_HEIGHT - viewport;
      if (next !== current) {
        if (el) el.scrollTop = next;
        onScrollTo(next);
      }
    },
    [viewport, onScrollTo],
  );

  // Bring the selection into view once its row is loaded (deep links).
  const revealed = useRef<string | null>(null);
  useEffect(() => {
    const el = scrollRef.current;
    if (!selectedKey || revealed.current === selectedKey || !el) return;
    const i = indexOf(selectedKey);
    if (i < 0) return;
    revealed.current = selectedKey;
    const top = i * ROW_HEIGHT;
    if (top < el.scrollTop || top + ROW_HEIGHT > el.scrollTop + (el.clientHeight || FALLBACK_VIEWPORT)) {
      el.scrollTop = Math.max(0, top - (el.clientHeight || FALLBACK_VIEWPORT) / 2);
    }
  }, [selectedKey, indexOf]);

  const toggle = useCallback(
    (key: string, open?: boolean) =>
      setExpanded((prev) => {
        const next = new Set(prev);
        if (open ?? !prev.has(key)) next.add(key);
        else next.delete(key);
        return next;
      }),
    [],
  );

  const loadMore = useCallback(
    (r: string, p: string) => {
      const key = nodeKey(r, p);
      const st = folders.get(key);
      const cursor = st?.pages[st.pages.length - 1]?.next_cursor;
      if (cursor) setMore((m) => ({ ...m, [key]: [...(m[key] ?? []), cursor] }));
    },
    [folders],
  );
  const retry = useCallback(
    (r: string, p: string) => {
      void qc.invalidateQueries({ queryKey: [...browseKeys.all, 'folder', r, p] });
      void qc.invalidateQueries({ queryKey: [...catalogKeys.components, 'lookup'] });
    },
    [qc],
  );

  // Stable while only the scroll position changes, so the memoised rows keep their render.
  const activate = useCallback(
    (row: Row) => {
      setActive(row.key);
      switch (row.type) {
        case 'repo':
          if (!row.expanded) toggle(row.key, true);
          onSelect({ kind: 'repo', repo: row.repo });
          break;
        case 'entry':
          if (row.container && !row.expanded) toggle(row.key, true);
          onSelect({ kind: 'entry', repo: row.repo, entry: row.entry });
          break;
        case 'version':
          onSelect({ kind: 'version', repo: row.repo, entry: row.entry, version: row.version });
          break;
        case 'more':
          loadMore(row.repo, row.path);
          break;
        case 'retry':
          retry(row.repo, row.path);
          break;
      }
    },
    [toggle, onSelect, loadMore, retry],
  );

  const activeIndex = (() => {
    const i = indexOf(active);
    if (i >= 0) return i;
    const s = indexOf(selectedKey);
    return s >= 0 ? s : move(rows, -1, 'next');
  })();
  const activeRow = rows[activeIndex];

  const focusIndex = (i: number) => {
    if (i < 0) return;
    setActive(rows[i]!.key);
    reveal(i);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!activeRow) return;
    const container = activeRow.type === 'repo' || (activeRow.type === 'entry' && activeRow.container);
    const open = (activeRow.type === 'repo' || activeRow.type === 'entry') && activeRow.expanded;
    switch (e.key) {
      case 'ArrowDown':
        focusIndex(move(rows, activeIndex, 'next'));
        break;
      case 'ArrowUp':
        focusIndex(move(rows, activeIndex, 'prev'));
        break;
      case 'Home':
        focusIndex(move(rows, activeIndex, 'first'));
        break;
      case 'End':
        focusIndex(move(rows, activeIndex, 'last'));
        break;
      case 'ArrowRight':
        if (container && !open) toggle(activeRow.key, true);
        else if (container && open) {
          const next = activeIndex + 1;
          if (rows[next] && rows[next]!.level > activeRow.level && isFocusable(rows[next]!)) focusIndex(next);
        }
        break;
      case 'ArrowLeft':
        if (container && open) toggle(activeRow.key, false);
        else focusIndex(move(rows, activeIndex, 'parent'));
        break;
      case 'Enter':
      case ' ':
        activate(activeRow);
        break;
      default:
        return;
    }
    e.preventDefault();
  };

  const first = Math.max(0, topRow - OVERSCAN);
  const last = Math.min(rows.length, topRow + Math.ceil(viewport / ROW_HEIGHT) + 1 + OVERSCAN);

  return (
    <nav className="browse-tree glass" aria-label="Repositories and packages">
      <div className="field browse-tree__filter">
        <label htmlFor={filterId} className="visually-hidden">
          Filter the tree
        </label>
        <input
          id={filterId}
          className="input tree-filter"
          type="search"
          placeholder="Filter the tree"
          autoComplete="off"
          spellCheck={false}
          value={filter}
          aria-controls={treeId}
          onChange={(e) => setFilter(e.target.value)}
        />
      </div>

      {repos.isError && <ErrorNote what="repositories" error={repos.error} onRetry={() => void repos.refetch()} />}
      {repos.isPending && (
        <p className="meta tree-status" aria-live="polite">
          Loading repositories…
        </p>
      )}
      {repos.data && sorted.length === 0 && <p className="meta tree-status">No repositories you can read yet.</p>}
      {repos.data && sorted.length > 0 && filter.trim() && (
        <p className="meta tree-status">
          {rows.length === 0 && `Nothing loaded matches “${filter.trim()}”. `}
          <Link to="/search" search={{ q: filter.trim() }} className="link-strong">
            Search all packages for “{filter.trim()}”
          </Link>
        </p>
      )}

      {repos.data && rows.length > 0 && (
        <div
          ref={scrollRef}
          id={treeId}
          className="browse-tree__scroll"
          role="tree"
          aria-label="Repositories"
          tabIndex={0}
          aria-activedescendant={activeRow ? rowId(treeId, activeRow.key) : undefined}
          onKeyDown={onKeyDown}
          onScroll={(e) => onScrollTo(e.currentTarget.scrollTop)}
          onFocus={measure}
        >
          <div role="none" className="browse-tree__canvas" style={{ height: rows.length * ROW_HEIGHT }}>
            {rows.slice(first, last).map((row, i) => (
              <TreeRow
                key={row.key}
                row={row}
                id={rowId(treeId, row.key)}
                top={(first + i) * ROW_HEIGHT}
                active={row.key === activeRow?.key}
                selected={row.key === selectedKey}
                onActivate={activate}
                onToggle={toggle}
              />
            ))}
          </div>
        </div>
      )}
    </nav>
  );
}

interface TreeRowProps {
  row: Row;
  id: string;
  top: number;
  active: boolean;
  selected: boolean;
  onActivate: (row: Row) => void;
  onToggle: (key: string) => void;
}

/** One row; memoised, as the tree re-renders while it scrolls and only the rows entering the window are new. */
const TreeRow = memo(function TreeRow({ row, id, top, active, selected, onActivate, onToggle }: TreeRowProps) {
  const indent = { paddingLeft: 6 + (row.level - 1) * 16 };
  const className = ['tree-row', active ? 'is-active' : '', selected ? 'is-selected' : ''].filter(Boolean).join(' ');
  const style = { top, height: ROW_HEIGHT, ...indent };

  if (row.type === 'version') {
    const v = row.version.version;
    return (
      <div
        id={id}
        role="treeitem"
        aria-level={row.level}
        aria-setsize={row.setSize}
        aria-posinset={row.posInSet}
        aria-selected={selected}
        className={className}
        style={style}
        onClick={() => onActivate(row)}
      >
        <span className="tree-row__chevron" aria-hidden="true" />
        <EntryIcon kind="version" />
        <TreeName name={v} />
        {row.newest && <span className="tree-row__meta">newest</span>}
      </div>
    );
  }

  if (row.type !== 'repo' && row.type !== 'entry') {
    const label: Record<typeof row.type, ReactNode> = {
      loading: 'Loading…',
      empty: row.pkg ? 'No versions' : row.path ? 'Empty folder' : 'Nothing stored here yet',
      more: 'Load more',
      retry: 'Could not load. Retry',
    };
    const actionable = row.type === 'more' || row.type === 'retry';
    return (
      <div
        id={id}
        role="treeitem"
        aria-level={row.level}
        aria-selected={false}
        aria-disabled={actionable ? undefined : true}
        className={`${className} tree-row--status tree-row--${row.type}`}
        style={{ ...style, paddingLeft: style.paddingLeft + 22 }}
        onClick={actionable ? () => onActivate(row) : undefined}
      >
        {label[row.type]}
      </div>
    );
  }

  const container = row.type === 'repo' || row.container;
  const name = row.type === 'repo' ? row.repo.name : row.entry.name;
  return (
    <div
      id={id}
      role="treeitem"
      aria-level={row.level}
      aria-setsize={row.setSize}
      aria-posinset={row.posInSet}
      aria-expanded={container ? row.expanded : undefined}
      aria-selected={selected}
      className={className}
      style={style}
      onClick={() => onActivate(row)}
    >
      {container ? (
        <span
          className={row.expanded ? 'tree-row__chevron is-open' : 'tree-row__chevron'}
          aria-hidden="true"
          onClick={(e) => {
            e.stopPropagation();
            onToggle(row.key);
          }}
        >
          <Chevron />
        </span>
      ) : (
        <span className="tree-row__chevron" aria-hidden="true" />
      )}
      {row.type === 'repo' ? (
        <FormatIcon format={row.repo.format} size={18} label={null} className="tree-row__format" />
      ) : (
        <EntryIcon kind={row.entry.kind} />
      )}
      <TreeName name={name} />
      {row.type === 'repo' ? (
        <span className={`tree-row__badge tone-${TYPE_META[row.repo.type].tone}`}>{TYPE_META[row.repo.type].label}</span>
      ) : (
        <EntryMeta entry={row.entry} />
      )}
    </div>
  );
});

/** Characters kept at the end of a cut name: versions and extensions tell siblings apart. */
const TAIL = 12;

/** A row's name, cut in the middle when it does not fit, so its end stays readable. */
function TreeName({ name }: { name: string }) {
  if (name.length <= TAIL + 8) {
    return (
      <span className="tree-row__name" title={name}>
        <span className="tree-row__head">{name}</span>
      </span>
    );
  }
  return (
    <span className="tree-row__name" title={name}>
      <span className="tree-row__head">{name.slice(0, -TAIL)}</span>
      <span className="tree-row__tail">{name.slice(-TAIL)}</span>
    </span>
  );
}

function EntryMeta({ entry }: { entry: BrowseEntry }) {
  if (entry.kind === 'asset' && entry.asset) return <span className="tree-row__meta">{formatBytes(entry.asset.size_bytes)}</span>;
  if (entry.kind === 'package') return <span className="tree-row__meta">package</span>;
  if (entry.kind === 'version') return <span className="tree-row__meta">version</span>;
  return null;
}

const svg = { width: 16, height: 16, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', strokeWidth: 1.8, 'aria-hidden': true } as const;

function Chevron() {
  return (
    <svg {...svg} width={14} height={14}>
      <path d="M9 6l6 6-6 6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function EntryIcon({ kind }: { kind: BrowseEntry['kind'] }) {
  switch (kind) {
    case 'folder':
      return (
        <svg {...svg} className="tree-row__icon">
          <path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2V7z" strokeLinejoin="round" />
        </svg>
      );
    case 'package':
      return (
        <svg {...svg} className="tree-row__icon tree-row__icon--package">
          <path d="M12 3l8 4.5v9L12 21l-8-4.5v-9L12 3z" strokeLinejoin="round" />
          <path d="M4 7.5l8 4.5 8-4.5M12 12v9" />
        </svg>
      );
    case 'version':
      return (
        <svg {...svg} className="tree-row__icon tree-row__icon--version">
          <path d="M4 12l8-8h7v7l-8 8-7-7z" strokeLinejoin="round" />
          <circle cx="15.5" cy="8.5" r="1.4" />
        </svg>
      );
    default:
      return (
        <svg {...svg} className="tree-row__icon">
          <path d="M6 3h8l4 4v14H6V3z" strokeLinejoin="round" />
          <path d="M14 3v4h4" strokeLinejoin="round" />
        </svg>
      );
  }
}
