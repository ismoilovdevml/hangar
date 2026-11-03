/**
 * Hand-written API types for /api/v1.
 *
 * TODO(api): replace with types generated from api/openapi.yaml
 * (`npm run gen:api` writes src/api/schema.d.ts) once the contract lands, then
 * re-export the generated component schemas from here so call sites keep working.
 */

export type Format =
  | 'docker'
  | 'npm'
  | 'maven'
  | 'pypi'
  | 'go'
  | 'helm'
  | 'nuget'
  | 'cargo'
  | 'apt'
  | 'raw';

export type RepositoryType = 'hosted' | 'proxy' | 'group';

/** Range switch on the Overview screen; drives every widget. */
export type OverviewRange = '1h' | 'today' | '7d' | '30d';

/** Standard error body for every non-2xx JSON response. */
export interface ApiError {
  code: string;
  message: string;
  /** Seconds until the action may be retried (lockout, rate limit). */
  retry_after?: number;
}

/* ---------- system / session ---------- */

export interface SystemStatus {
  /** ok = all systems normal; degraded = something needs attention; down = outage. */
  state: 'ok' | 'degraded' | 'down';
  /** Short human sentence, e.g. "All systems normal". */
  message: string;
  version: string;
  instance_name: string;
  nodes_up: number;
  nodes_total: number;
}

export interface AuthProviders {
  /** OIDC provider configured for "Continue with …". Null when not configured. */
  oidc: { display_name: string; start_url: string } | null;
  ldap: { enabled: boolean };
  saml: { enabled: boolean; start_url: string | null };
  local: { enabled: boolean };
}

export interface LoginRequest {
  username: string;
  password: string;
  remember: boolean;
  realm: 'local' | 'ldap';
}

export interface Organization {
  slug: string;
  name: string;
  environment: string;
  region: string;
}

export interface User {
  id: string;
  username: string;
  display_name: string;
  email: string;
}

export interface Session {
  user: User;
  organization: Organization;
}

/* ---------- overview ---------- */

export interface OverviewMetrics {
  range: OverviewRange;
  generated_at: string;
  summary: {
    published_since_yesterday: number;
    failed_pushes: number;
  };
  storage: {
    used_bytes: number;
    capacity_bytes: number;
    added_bytes_24h: number;
  };
  requests: {
    total: number;
    /** Change versus the previous range of the same length, in percent. */
    trend_pct: number;
    /** Busiest equivalent range in the last 30 days; the KPI meter is total / peak_total. */
    peak_total: number;
    p95_ms: number;
  };
  cache: {
    /** 0..1 share of downloads served from cache. */
    hit_ratio: number;
    bytes_saved: number;
  };
  issues: {
    critical: number;
    packages_affected: number;
    quarantined: number;
    open_findings: number;
  };
  traffic: TrafficBucket[];
  storage_by_format: { format: Format; bytes: number }[];
}

export interface TrafficBucket {
  /** Bucket start, RFC 3339. */
  start: string;
  downloads: number;
  uploads: number;
}

export type ScanState = 'pending' | 'clean' | 'findings' | 'quarantined';

export interface ScanSummary {
  state: ScanState;
  critical: number;
  high: number;
  medium: number;
  low: number;
}

export interface Publisher {
  kind: 'user' | 'ci' | 'service_account';
  display_name: string;
}

export interface PublishedComponent {
  id: string;
  format: Format;
  name: string;
  version: string;
  repository: string;
  publisher: Publisher;
  published_at: string;
  scan: ScanSummary;
}

export interface Page<T> {
  items: T[];
  next_cursor: string | null;
}

export type UpstreamState = 'healthy' | 'slow' | 'blocked' | 'offline';

export interface UpstreamHealth {
  name: string;
  url: string;
  state: UpstreamState;
  /** p50 latency over the last 5 minutes; null when there is no recent traffic. */
  latency_ms: number | null;
}

/** Endpoints the current user should configure in their clients, most-used format first. */
export interface ClientEndpoint {
  format: Format;
  /** Registry host for docker, base URL for everything else. */
  endpoint: string;
  repository: string;
}

/* ---------- live events (SSE /api/v1/events) ---------- */

export type LiveEvent =
  | { type: 'component.published'; data: PublishedComponent }
  | { type: 'component.quarantined'; data: { id: string; name: string; version: string } }
  | { type: 'task.progress'; data: { task_id: string; progress: number } };
