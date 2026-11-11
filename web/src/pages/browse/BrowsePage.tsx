import { useId, useRef, useState } from 'react';
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router';
import { useComponentLookup } from '../../api/catalog';
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
import { entrySearch, parentPath } from './treeModel';
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
  const { repo, name, path } = search;
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
    void navigate({ to: '/browse', search: s.kind === 'repo' ? { repo: s.repo.name } : entrySearch(s.repo, s.entry) });
    if (window.matchMedia?.(SINGLE_COLUMN).matches) mainRef.current?.scrollIntoView({ block: 'start' });
  };
  const toFolder = (p: string) => void navigate({ to: '/browse', search: { repo, path: p || undefined }, replace: true });

  let pane;
  if (!repo) {
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
    pane = <ResolvedPackage repo={search.member ?? repo} ns={search.ns} name={name} version={search.version} />;
  } else if (path && path.endsWith('/')) {
    pane = <FolderDetail repo={repo} path={path} onOpen={(e) => void navigate({ to: '/browse', search: entrySearch(repo, e) })} />;
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
          <BrowseTree repo={repo} path={path} onSelect={onSelect} />
        </div>
      </div>
      <div className="browse__main" ref={mainRef}>
        {pane}
      </div>
    </div>
  );
}

/** Finds the component for repo / ns / name / version, then shows it. */
function ResolvedPackage({ repo, ns, name, version }: { repo: string; ns?: string; name: string; version?: string }) {
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
  return <PackageDetail id={current.id} versions={versions} />;
}
