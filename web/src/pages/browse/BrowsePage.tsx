import { useId, useRef, useState } from 'react';
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router';
import { flatItems, useComponentAssets, useComponentLookup } from '../../api/catalog';
import type { BrowseEntry, PublishedComponent } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { IconSearch } from '../../components/icons';
import { IconChevronRight } from '../../components/opsIcons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { AssetDetail } from './AssetDetail';
import { BrowseTree, type TreeSelection } from './BrowseTree';
import { FolderDetail } from './FolderDetail';
import { PackageDetail } from './PackageDetail';
import { RepositorySummary } from './RepositorySummary';
import { entrySearch, parentPath, versionSearch } from './treeModel';
import '../../styles/catalog.css';
import './browse.css';

const route = getRouteApi('/app/browse');

const TREE_KEY = 'hangar.browse.treeHidden';

/** Whether this browser hid the tree. Storage may be missing or blocked: hiding then lasts until a reload. */
function readTreeHidden(): boolean {
  try {
    return window.localStorage.getItem(TREE_KEY) === '1';
  } catch {
    return false;
  }
}

/** One column: the tree sits under the page, so a selection scrolls the page back up to it. */
const SINGLE_COLUMN = '(max-width: 1000px)';

export function BrowsePage() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const { repo, name, path, files } = search;
  const treeId = useId();
  const mainRef = useRef<HTMLDivElement>(null);
  const [treeHidden, setTreeHidden] = useState(readTreeHidden);
  const toggleTree = () => {
    const next = !treeHidden;
    setTreeHidden(next);
    try {
      window.localStorage.setItem(TREE_KEY, next ? '1' : '0');
    } catch {
      // not persisted
    }
  };

  const onSelect = (s: TreeSelection) => {
    void navigate({
      to: '/browse',
      search: s.kind === 'repo' ? { repo: s.repo.name } : s.kind === 'version' ? versionSearch(s.repo, s.entry, s.version) : entrySearch(s.repo, s.entry),
    });
    if (window.matchMedia?.(SINGLE_COLUMN).matches) mainRef.current?.scrollIntoView({ block: 'start' });
  };
  const toFolder = (p: string) => void navigate({ to: '/browse', search: { repo, path: p || undefined }, replace: true });
  // The Files view walks the stored files: folders of any kind open as folders.
  const toFiles = (p: string, asset?: string, replace = false) => void navigate({ to: '/browse', search: { repo, files: p || '/', asset }, replace });
  const openFile = (e: BrowseEntry) => (e.kind === 'asset' ? toFiles(e.path, e.asset?.id) : toFiles(e.path));

  let pane;
  if (repo && files !== undefined) {
    const folder = files === '/' ? '' : files;
    pane = search.asset ? (
      <AssetDetail key={search.asset} id={search.asset} treeRepo={repo} onDeleted={() => toFiles(parentPath(folder), undefined, true)} />
    ) : (
      <FolderDetail key={folder} repo={repo} path={folder} onOpen={openFile} onUp={folder ? () => toFiles(parentPath(folder)) : undefined} />
    );
  } else if (!repo) {
    pane = (
      <>
        <PageHeader title="Packages" description="Browse every repository, package, version and file." />
        <Panel className="browse-empty">
          <p>Expand a repository in the list to browse its content the way clients see it, or select it to see how to use it.</p>
          <p>
            Looking for something specific?{' '}
            <Link to="/search" className="link-strong">
              <IconSearch size={14} className="inline-icon" /> Search all packages
            </Link>
          </p>
        </Panel>
      </>
    );
  } else if (search.asset) {
    pane = <AssetDetail key={search.asset} id={search.asset} treeRepo={repo} onDeleted={() => toFolder(parentPath(path ?? ''))} />;
  } else if (name) {
    pane = <ResolvedPackage repo={search.member ?? repo} ns={search.ns} name={name} version={search.version} onRawDeleted={() => toFolder(parentPath(path ?? ''))} />;
  } else if (path && path.endsWith('/')) {
    pane = <FolderDetail key={path} repo={repo} path={path} onOpen={(e) => void navigate({ to: '/browse', search: entrySearch(repo, e) })} />;
  } else {
    pane = <RepositorySummary repo={repo} />;
  }

  return (
    <div className={treeHidden ? 'browse browse--tree-hidden' : 'browse'}>
      <div className="browse__side">
        <button
          type="button"
          className="icon-button icon-button--sm icon-button--glass browse__tree-toggle"
          aria-expanded={!treeHidden}
          aria-controls={treeId}
          aria-label={treeHidden ? 'Show the repository tree' : 'Hide the repository tree'}
          title={treeHidden ? 'Show the repository tree' : 'Hide the repository tree'}
          onClick={toggleTree}
        >
          <IconChevronRight size={16} />
        </button>
        <div className="browse__tree" id={treeId} hidden={treeHidden}>
          {/* The Files view is not a tree node: the tree marks the repository. */}
          <BrowseTree
            repo={repo}
            path={files !== undefined ? '' : path}
            ns={files !== undefined ? undefined : search.ns}
            name={files !== undefined ? undefined : name}
            version={files !== undefined ? undefined : search.version}
            onSelect={onSelect}
          />
        </div>
      </div>
      <div className="browse__main" ref={mainRef}>
        {pane}
      </div>
    </div>
  );
}

/** Finds the component for repo / ns / name / version, then shows it. */
function ResolvedPackage({ repo, ns, name, version, onRawDeleted }: { repo: string; ns?: string; name: string; version?: string; onRawDeleted: () => void }) {
  const search = route.useSearch();
  const lookup = useComponentLookup(repo, name);
  if (lookup.isError) {
    return (
      <Panel>
        <ErrorNote what="the package" error={lookup.error} onRetry={() => void lookup.refetch()} />
      </Panel>
    );
  }
  if (!lookup.data) {
    return (
      <Panel aria-busy="true" className="browse-empty">
        <p>Loading {name}…</p>
      </Panel>
    );
  }
  const versions = lookup.data.items.filter((c) => ns === undefined || (c.namespace ?? '') === ns);
  const current = version !== undefined ? versions.find((c) => c.version === version) : versions[0];
  if (!current) {
    return (
      <Panel className="browse-empty" role="status">
        <p>
          {version ? (
            <>
              Version <strong>{version}</strong> of <strong>{name}</strong> is not in {repo}.{' '}
              {versions.length > 0 && (
                <Link to="/browse" search={{ repo: search.repo, member: search.member, ns, name }}>
                  Show the newest version
                </Link>
              )}
            </>
          ) : (
            <>
              <strong>{name}</strong> is not in {repo}, or you cannot see it.
            </>
          )}
        </p>
      </Panel>
    );
  }
  // A raw "package" is one stored file: show it as the file it is.
  if (current.format === 'raw') return <RawFile c={current} treeRepo={search.repo ?? repo} onDeleted={onRawDeleted} />;
  // Keyed by version: a scan result or a pending delete belongs to one version.
  return <PackageDetail key={current.id} id={current.id} versions={versions} />;
}

function RawFile({ c, treeRepo, onDeleted }: { c: PublishedComponent; treeRepo: string; onDeleted: () => void }) {
  const assets = useComponentAssets(c.id);
  if (assets.isError) {
    return (
      <Panel>
        <ErrorNote what="the file" error={assets.error} onRetry={() => void assets.refetch()} />
      </Panel>
    );
  }
  const file = flatItems(assets.data)[0];
  if (!file) {
    return (
      <Panel aria-busy={assets.isPending || undefined} className="browse-empty" role="status">
        <p>{assets.isPending ? 'Loading…' : 'This file no longer exists.'}</p>
      </Panel>
    );
  }
  return <AssetDetail key={file.id} id={file.id} treeRepo={treeRepo} onDeleted={onDeleted} />;
}
