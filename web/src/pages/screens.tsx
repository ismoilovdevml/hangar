import { Link } from '@tanstack/react-router';
import { ScreenSkeleton } from './ScreenSkeleton';

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
