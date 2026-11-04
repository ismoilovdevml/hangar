import { getRouteApi, Link } from '@tanstack/react-router';
import { useComponentLookup } from '../../api/catalog';
import { ErrorNote } from '../../components/ErrorNote';
import { IconSearch } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { PackageDetail } from './PackageDetail';
import { PackageTree } from './PackageTree';
import { RepositorySummary } from './RepositorySummary';
import '../../styles/facts.css';
import './browse.css';

const route = getRouteApi('/app/browse');

export function BrowsePage() {
  const search = route.useSearch();
  const { repo, name } = search;

  return (
    <div className="browse">
      <PackageTree repo={repo} ns={search.ns} name={name} version={search.version} />
      <div className="browse__main">
        {!repo ? (
          <>
            <PageHeader title="Packages" description="Browse every repository, package and version." />
            <Panel className="browse-empty">
              <p>Choose a repository to see its packages, versions and files.</p>
              <p>
                Looking for something specific?{' '}
                <Link to="/search" className="link-strong">
                  <IconSearch size={14} className="inline-icon" /> Search all packages
                </Link>
              </p>
            </Panel>
          </>
        ) : !name ? (
          <RepositorySummary repo={repo} />
        ) : (
          <ResolvedPackage repo={repo} ns={search.ns} name={name} version={search.version} />
        )}
      </div>
    </div>
  );
}

/** Finds the component for repo / ns / name / version, then shows it. */
function ResolvedPackage({ repo, ns, name, version }: { repo: string; ns?: string; name: string; version?: string }) {
  const lookup = useComponentLookup(repo, name);
  if (lookup.isError) {
    return (
      <Panel>
        <ErrorNote what="the package" onRetry={() => void lookup.refetch()} />
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
                <Link to="/browse" search={{ repo, ns, name }}>
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
