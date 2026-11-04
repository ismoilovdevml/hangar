import { getRouteApi, Link, useNavigate } from '@tanstack/react-router';
import { useState, type FormEvent } from 'react';
import { ScreenSkeleton } from './ScreenSkeleton';

const browseRoute = getRouteApi('/app/browse');

export function BrowsePage() {
  const { repo, name, version } = browseRoute.useSearch();
  const selected = repo && name ? `${repo} / ${name}${version ? `@${version}` : ''}` : null;
  return (
    <ScreenSkeleton
      title="Packages"
      description={selected ? `Selected: ${selected}` : 'Browse every repository, package and version.'}
      planned={[
        'A tree of repository → package → versions.',
        'Package header with format, version, signature, platforms, publisher, and Download SBOM / Delete actions.',
        'The pull or install command for the format, ready to copy.',
        'Tabs: Overview, Versions, Layers or Files, Vulnerabilities, Dependents.',
      ]}
    />
  );
}

const searchRoute = getRouteApi('/app/search');

export function SearchPage() {
  const { q } = searchRoute.useSearch();
  const navigate = useNavigate();
  const [draft, setDraft] = useState(q ?? '');
  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    void navigate({ to: '/search', search: { q: draft.trim() || undefined } });
  };
  return (
    <ScreenSkeleton
      title="Find any package"
      description="Search by name, digest, CVE, licence or repository."
      planned={[
        'Query language: free text, format:, repo:, sha256:, cve:, unused:>90d, license:, published:<date.',
        'Facets for format, security state and repository, with counts.',
        'Results with format, coordinates, repository, versions, downloads and security state, sortable.',
      ]}
    >
      <form className="search-form glass" role="search" onSubmit={onSubmit}>
        <label className="visually-hidden" htmlFor="search-q">
          Search query
        </label>
        <input
          id="search-q"
          className="input"
          type="search"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder="e.g. payments-api format:docker"
        />
        <button type="submit" className="button button--primary">
          Search
        </button>
      </form>
    </ScreenSkeleton>
  );
}

export function BlobStoresPage() {
  return (
    <ScreenSkeleton
      title="Blob stores"
      description="The disks and buckets behind your repositories. Identical layers are stored once."
      planned={[
        'Totals: used versus capacity, space saved by deduplication, growth per day with a time-to-full estimate, space reclaimable now.',
        'One card per store with type, location, state, usage, soft quota, repositories, blob count, write latency and encryption.',
        'Actions to edit settings and move repositories between stores online.',
      ]}
    />
  );
}

export function VulnerabilitiesPage() {
  return (
    <ScreenSkeleton
      title="Vulnerabilities"
      description="Every package is scanned when it arrives and again when new advisories appear."
      planned={[
        'Critical, high, medium and low counts.',
        'Findings with advisory, severity, package, fixed-in version, affected components and status.',
        'Filters for fix available, in production and waived; waivers need a reason and an expiry.',
        'Quarantine, signature and licence policies, and a 30-day trend.',
      ]}
    />
  );
}

export function ClusterPage() {
  return (
    <ScreenSkeleton
      title="Cluster & replication"
      description="Nodes that share the load, and edge nodes that keep a nearby copy for other regions."
      planned={[
        'Overall health and quorum.',
        'One card per node with role, zone, CPU, memory and cache disk, requests per second and uptime.',
        'Replication targets with lag or failures, and the health of PostgreSQL, object storage, the event bus, search and the scanner.',
      ]}
    />
  );
}

export function NotFoundPage() {
  return (
    <div className="screen">
      <div className="page-header">
        <div className="page-header__text">
          <h1 className="page-header__title">Page not found</h1>
          <p className="page-header__desc">
            This address does not match any page. <Link to="/">Go to the overview</Link>.
          </p>
        </div>
      </div>
    </div>
  );
}
