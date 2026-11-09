/**
 * Sample data for mock mode (VITE_API_MOCKS=true) and unit tests.
 * Never imported by production code paths.
 */
import type {
  AuthProviders,
  BlobStore,
  BlobStoreList,
  CleanupPolicy,
  ClientEndpoint,
  Format,
  OverviewMetrics,
  OverviewRange,
  PublishedComponent,
  Repository,
  RepositoryStatus,
  RepositoryType,
  Session,
  UpstreamHealth,
} from '../api/types';
import type { SystemStatusDetail } from '../api/opsTypes';

const TB = 1e12;
const GB = 1e9;

export const session: Session = {
  user: {
    id: 'u_01',
    username: 'd.ismoilov',
    display_name: 'Dilshod Ismoilov',
    email: 'd.ismoilov@acme.example',
    kind: 'user',
    source: 'local',
    roles: ['Admin'],
  },
  organization: {
    slug: 'acme-platform',
    name: 'acme-platform',
    environment: 'prod',
    region: 'eu-central',
  },
  privileges: ['repo:*:admin', 'system:*'],
  auth_method: 'session',
  token_policy: { max_lifetime: '8784h0m0s', max_lifetime_days: 366, expiry_warning: '336h0m0s' },
  password_change_required: false,
  password_policy: { min_length: 12, max_length: 256, min_classes: 0, allow_username: false },
  token_warnings: [
    {
      id: 't_0001',
      name: 'laptop CLI',
      owner: 'd.ismoilov',
      owner_kind: 'user',
      prefix: 'hgr_0001',
      scopes: ['repo:*:read', 'repo:npm-*:write'],
      created_at: new Date(Date.now() - 30 * 86_400_000).toISOString(),
      expires_at: new Date(Date.now() + 9 * 86_400_000).toISOString(),
      allowed_cidrs: [],
      read_only: false,
    },
  ],
};

export const systemStatus: SystemStatusDetail = {
  state: 'ok',
  message: 'All systems normal',
  version: '1.0.0',
  instance_name: 'acme-platform',
  nodes_up: 3,
  nodes_total: 3,
  build: { version: '1.0.0', commit: '237ab95', date: '2026-11-03T18:00:00Z', go_version: 'go1.27.0' },
  uptime_seconds: 1_209_600,
  web_ui: true,
  base_url: 'https://pkg.acme.io',
  registry_host: 'cr.acme.io',
  dependencies: [
    { name: 'blobstore', status: 'ok', latency_ms: 12 },
    { name: 'database', status: 'ok', latency_ms: 2 },
    { name: 'eventbus', status: 'ok', latency_ms: 1 },
  ],
};

export const authProviders: AuthProviders = {
  oidc: { display_name: 'company SSO', start_url: '/api/v1/auth/oidc/start' },
  ldap: { enabled: true },
  saml: { enabled: true, start_url: '/api/v1/auth/saml/start' },
  local: { enabled: true },
};

/** Hourly shape of a working day, busiest early afternoon. */
const DAY_DOWNLOADS = [22, 18, 14, 12, 11, 13, 20, 34, 52, 66, 72, 78, 82, 95, 100, 92, 86, 74, 60, 48, 40, 36, 30, 26];
const DAY_UPLOADS = [3, 2, 1, 1, 1, 1, 2, 4, 8, 12, 14, 12, 10, 14, 16, 13, 11, 9, 6, 4, 3, 3, 2, 2];

const RANGE_SHAPE: Record<OverviewRange, { buckets: number; stepMs: number; scale: number }> = {
  '1h': { buckets: 12, stepMs: 5 * 60_000, scale: 1_300 },
  today: { buckets: 24, stepMs: 3_600_000, scale: 15_000 },
  '7d': { buckets: 7, stepMs: 86_400_000, scale: 230_000 },
  '30d': { buckets: 30, stepMs: 86_400_000, scale: 220_000 },
};

const REQUESTS: Record<OverviewRange, { total: number; trend: number; peak: number }> = {
  '1h': { total: 912_000, trend: 3, peak: 1_480_000 },
  today: { total: 18_400_000, trend: 6, peak: 25_600_000 },
  '7d': { total: 121_300_000, trend: 4, peak: 140_200_000 },
  '30d': { total: 512_800_000, trend: 9, peak: 540_000_000 },
};

function traffic(range: OverviewRange, now: number) {
  const { buckets, stepMs, scale } = RANGE_SHAPE[range];
  return Array.from({ length: buckets }, (_, i) => {
    const shape = DAY_DOWNLOADS.length;
    const idx = range === 'today' ? i : Math.floor((i / buckets) * shape);
    const weekdayDip = range === '7d' || range === '30d' ? (i % 7 === 5 || i % 7 === 6 ? 0.45 : 1) : 1;
    const d = (DAY_DOWNLOADS[idx] ?? 50) * (range === '7d' || range === '30d' ? 1.2 : 1);
    const u = DAY_UPLOADS[idx] ?? 5;
    const downloads = Math.round(d * scale * weekdayDip);
    return {
      start: new Date(now - (buckets - i) * stepMs).toISOString(),
      downloads,
      uploads: Math.round(u * scale * 0.04 * weekdayDip),
      // Metadata and API calls on top of the content downloads.
      requests: Math.round(downloads * 1.03),
    };
  });
}

/** Proxy requests per range: hits and misses, 94.2% from cache. */
const CACHE_REQUESTS: Record<OverviewRange, number> = { '1h': 2_940, today: 48_200, '7d': 412_000, '30d': 1_730_000 };

function cacheNumbers(range: OverviewRange): OverviewMetrics['cache'] {
  const total = CACHE_REQUESTS[range];
  const hits = Math.round(total * 0.942);
  return { hit_ratio: Math.round((hits / total) * 10_000) / 10_000, bytes_saved: Math.round(hits * 1.21 * 1e6), hits, misses: total - hits };
}

/** Catalog activity per range: publications, proxy cache fills and rejected pushes. */
const ACTIVITY: Record<OverviewRange, OverviewMetrics['activity']> = {
  '1h': { published: 12, cached: 64, pushes_rejected: 0 },
  today: { published: 186, cached: 1_120, pushes_rejected: 4 },
  '7d': { published: 1_310, cached: 7_400, pushes_rejected: 9 },
  '30d': { published: 5_420, cached: 29_800, pushes_rejected: 31 },
};

export function overview(range: OverviewRange, now = Date.now()): OverviewMetrics {
  const r = REQUESTS[range];
  return {
    range,
    generated_at: new Date(now).toISOString(),
    summary: { published_since_yesterday: 214, failed_pushes: 4 },
    activity: ACTIVITY[range],
    storage: { used_bytes: 4.82 * TB, capacity_bytes: 12 * TB, added_bytes_24h: 38 * GB },
    requests: {
      total: r.total,
      trend_pct: r.trend,
      peak_total: r.peak,
      p95_ms: 41,
      coverage_start: null,
      server_errors: Math.round(r.total * 0.0021),
      denied: Math.round(r.total * 0.0094),
    },
    cache: cacheNumbers(range),
    issues: {
      critical: 7,
      packages_affected: 3,
      quarantined: 2,
      open_findings: 39,
      scanning: { enabled: true, scanned_components: 1840, last_scan_at: new Date(now - 40 * 60_000).toISOString() },
    },
    traffic: traffic(range, now),
    storage_by_format: [
      { format: 'docker', bytes: 2.91 * TB },
      { format: 'maven', bytes: 812 * GB },
      { format: 'npm', bytes: 476 * GB },
      { format: 'pypi', bytes: 301 * GB },
      { format: 'helm', bytes: 88 * GB },
      { format: 'go', bytes: 64 * GB },
      { format: 'raw', bytes: 172 * GB },
    ],
  };
}

const clean = { state: 'clean', critical: 0, high: 0, medium: 0, low: 0 } as const;

type Seed = Omit<PublishedComponent, 'published_at'> & { minutesAgo: number };

const RECENT: Seed[] = [
  { id: 'c_8812', format: 'docker', name: 'payments-api', version: '2.14.0', repository: 'docker-prod', publisher: { kind: 'ci', display_name: 'CI · build 8812' }, scan: clean, minutesAgo: 0 },
  { id: 'c_311', format: 'npm', name: '@acme/ui-kit', version: '5.3.1', repository: 'npm-internal', publisher: { kind: 'ci', display_name: 'CI · build 311' }, scan: clean, minutesAgo: 1 },
  { id: 'c_ledger', format: 'maven', name: 'ledger-core', version: '7.0.0-rc2', repository: 'maven-snapshots', publisher: { kind: 'user', display_name: 'Jasur Karimov' }, scan: { state: 'findings', critical: 0, high: 2, medium: 1, low: 0 }, minutesAgo: 4 },
  { id: 'c_1207', format: 'pypi', name: 'acme-ml-utils', version: '0.9.4', repository: 'pypi-internal', publisher: { kind: 'ci', display_name: 'CI · build 1207' }, scan: clean, minutesAgo: 9 },
  { id: 'c_ingress', format: 'helm', name: 'ingress-edge', version: '1.8.0', repository: 'helm-stable', publisher: { kind: 'service_account', display_name: 'Argo CD' }, scan: clean, minutesAgo: 17 },
  { id: 'c_8790', format: 'docker', name: 'auth-gateway', version: '3.2.0', repository: 'docker-prod', publisher: { kind: 'ci', display_name: 'CI · build 8790' }, scan: { state: 'findings', critical: 1, high: 0, medium: 0, low: 0 }, minutesAgo: 22 },
];

export function recentComponents(now = Date.now()): PublishedComponent[] {
  return RECENT.map(({ minutesAgo, ...c }) => ({
    ...c,
    published_at: new Date(now - minutesAgo * 60_000 - 10_000).toISOString(),
  }));
}

/** Rotated through by the mocked live feed. */
export const LIVE_SEEDS: Omit<PublishedComponent, 'id' | 'published_at'>[] = [
  { format: 'docker', name: 'billing-worker', version: '1.4.2', repository: 'docker-prod', publisher: { kind: 'ci', display_name: 'CI · build 8813' }, scan: { ...clean, state: 'pending' } },
  { format: 'npm', name: '@acme/eslint-config', version: '3.0.1', repository: 'npm-internal', publisher: { kind: 'ci', display_name: 'CI · build 312' }, scan: { ...clean, state: 'pending' } },
  { format: 'pypi', name: 'acme-feature-store', version: '2.2.0', repository: 'pypi-internal', publisher: { kind: 'user', display_name: 'Malika Yusupova' }, scan: { ...clean, state: 'pending' } },
];

/**
 * Upstream health is reported per proxy repository (name = repository name):
 * live state plus the median remote latency recorded over the range.
 */
export const upstreams: UpstreamHealth[] = [
  { name: 'dockerhub-proxy', url: 'https://registry-1.docker.io', state: 'healthy', latency_ms: 88, requests: 1_240 },
  { name: 'npmjs-proxy', url: 'https://registry.npmjs.org', state: 'slow', latency_ms: 412, requests: 860 },
  { name: 'maven-central', url: 'https://repo1.maven.org/maven2/', state: 'healthy', latency_ms: 64, requests: 312 },
  { name: 'pypi-proxy', url: 'https://pypi.org', state: 'unknown', latency_ms: null, requests: 0 },
  { name: 'ghcr-proxy', url: 'https://ghcr.io', state: 'blocked', latency_ms: null, requests: 0 },
];

/** Fewer remote requests in shorter ranges; the hour may have none. */
const UPSTREAM_SCALE: Record<OverviewRange, number> = { '1h': 0.02, today: 1, '7d': 6.5, '30d': 27 };

export function upstreamHealth(range: OverviewRange): UpstreamHealth[] {
  return upstreams.map((u) => {
    const requests = Math.round((u.requests ?? 0) * UPSTREAM_SCALE[range]);
    return { ...u, requests, latency_ms: requests > 0 ? u.latency_ms : null };
  });
}

export const clientEndpoints: ClientEndpoint[] = [
  { format: 'docker', endpoint: 'cr.acme.io', repository: 'docker-group' },
  { format: 'npm', endpoint: 'https://pkg.acme.io/repository/npm-group/', repository: 'npm-group' },
  { format: 'pypi', endpoint: 'https://pkg.acme.io/repository/pypi-group/', repository: 'pypi-group' },
];

/* ---------- repositories, blob stores, cleanup policies ---------- */

/** Formats the mock server speaks (the same set a default build serves). */
export const formats: Format[] = ['docker', 'helm', 'maven', 'npm', 'nuget', 'pypi', 'raw'];

const BASE_URL = 'https://pkg.acme.io';
const REGISTRY_HOST = 'cr.acme.io';

export function repositoryUrl(format: string, name: string): string {
  return format === 'docker' ? `${REGISTRY_HOST}/${name}` : `${BASE_URL}/repository/${name}/`;
}

type RepoSeed = [name: string, format: Format, type: RepositoryType, description: string, bytes: number, status?: RepositoryStatus, extra?: Partial<Repository['config']>];

const REPO_SEEDS: RepoSeed[] = [
  ['docker-all', 'docker', 'group', 'Single pull endpoint for every team', 0, 'online', { group: { members: ['docker-prod', 'dockerhub-proxy', 'ghcr-proxy'], write_member: 'docker-prod' } }],
  ['docker-prod', 'docker', 'hosted', 'Release images, immutable tags', 1.84 * TB],
  ['dockerhub-proxy', 'docker', 'proxy', '', 920 * GB, 'online', { proxy: { remote_url: 'https://registry-1.docker.io', metadata_ttl: '6h0m0s', content_ttl: '0s', negative_cache_ttl: '10m0s' } }],
  ['npm-all', 'npm', 'group', 'internal + npmjs', 0, 'online', { group: { members: ['npm-internal', 'npmjs-proxy'], write_member: 'npm-internal' } }],
  ['npm-internal', 'npm', 'hosted', '@acme scope', 41 * GB],
  ['npmjs-proxy', 'npm', 'proxy', '', 435 * GB, 'slow_upstream', { proxy: { remote_url: 'https://registry.npmjs.org', metadata_ttl: '30m0s', content_ttl: '0s', negative_cache_ttl: '10m0s' } }],
  ['maven-releases', 'maven', 'hosted', 'Release policy, no redeploy', 388 * GB],
  ['maven-snapshots', 'maven', 'hosted', 'Snapshots, kept 14 days', 96 * GB],
  ['pypi-internal', 'pypi', 'hosted', 'ML and tooling wheels', 22 * GB],
  ['ghcr-proxy', 'docker', 'proxy', '', 64 * GB, 'blocked', { proxy: { remote_url: 'https://ghcr.io', metadata_ttl: '6h0m0s', content_ttl: '0s', negative_cache_ttl: '10m0s' } }],
  ['maven-central', 'maven', 'proxy', '', 512 * GB, 'online', { proxy: { remote_url: 'https://repo1.maven.org/maven2/', metadata_ttl: '24h0m0s', content_ttl: '0s', negative_cache_ttl: '10m0s' } }],
  ['maven-all', 'maven', 'group', 'Releases, snapshots and Central', 0, 'online', { group: { members: ['maven-releases', 'maven-snapshots', 'maven-central'] } }],
  ['pypi-proxy', 'pypi', 'proxy', '', 118 * GB, 'online', { proxy: { remote_url: 'https://pypi.org', metadata_ttl: '1h0m0s', content_ttl: '0s', negative_cache_ttl: '10m0s' } }],
  ['nuget-internal', 'nuget', 'hosted', 'Shared .NET libraries', 7.4 * GB],
  ['raw-artifacts', 'raw', 'hosted', 'Build outputs and installers', 64 * GB],
  ['raw-legacy', 'raw', 'hosted', 'Read-only archive', 210 * GB, 'offline'],
  ['docker-dev', 'docker', 'hosted', 'Branch builds, cleaned nightly', 310 * GB],
  ['docker-staging', 'docker', 'hosted', 'Release candidates', 180 * GB],
  ['npm-sandbox', 'npm', 'hosted', 'Experiments', 1.2 * GB],
  ['maven-thirdparty', 'maven', 'hosted', 'Vendor jars without a public repo', 12 * GB],
  ['pypi-ml', 'pypi', 'hosted', 'Model packages', 140 * GB],
  ['helm-charts', 'raw', 'hosted', 'Chart tarballs', 3.1 * GB],
  ['nuget-proxy', 'nuget', 'proxy', '', 9.8 * GB, 'online', { proxy: { remote_url: 'https://api.nuget.org', metadata_ttl: '1h0m0s', content_ttl: '0s', negative_cache_ttl: '10m0s' } }],
  ['pypi-all', 'pypi', 'group', '', 0, 'online', { group: { members: ['pypi-internal', 'pypi-ml', 'pypi-proxy'] } }],
];

/** Minutes since the last upload or download per repository; absent = no activity yet. */
const ACTIVITY_MINUTES: Record<string, number> = {
  'docker-prod': 3, 'dockerhub-proxy': 1, 'npm-internal': 12, 'npmjs-proxy': 2, 'maven-releases': 95, 'maven-snapshots': 240,
  'pypi-internal': 9, 'ghcr-proxy': 3 * 1440, 'maven-central': 30, 'pypi-proxy': 55, 'nuget-internal': 2 * 1440,
  'raw-artifacts': 600, 'raw-legacy': 200 * 1440, 'docker-dev': 45, 'docker-staging': 26 * 60, 'maven-thirdparty': 40 * 1440,
  'pypi-ml': 7 * 1440, 'helm-charts': 9 * 1440, 'nuget-proxy': 18,
};

export function repositories(): Repository[] {
  const created = '2026-06-01T09:00:00Z';
  const now = Date.now();
  const activity = (name: string) => (name in ACTIVITY_MINUTES ? now - ACTIVITY_MINUTES[name]! * 60_000 : null);
  // A group is as recent as its most recently used member.
  const seen = (name: string, extra: Partial<Repository['config']>) => {
    const times = [activity(name), ...(extra.group?.members ?? []).map(activity)].filter((t): t is number => t !== null);
    return times.length ? new Date(Math.max(...times)).toISOString() : null;
  };
  const repos: Repository[] = REPO_SEEDS.map(([name, format, type, description, bytes, status = 'online', extra = {}], i) => ({
    id: `r_${String(i + 1).padStart(3, '0')}`,
    name,
    format,
    type,
    description,
    blob_store: type === 'group' ? undefined : bytes > 1 * TB ? 's3-eu-primary' : 'fs-local',
    online: status !== 'offline',
    config: {
      anonymous_read: false,
      scan_on_arrival: true,
      quarantine_critical: type !== 'proxy',
      ...(type === 'hosted' ? { hosted: { write_policy: format === 'docker' ? 'allow' : 'allow_once' } } : {}),
      ...extra,
    },
    url: repositoryUrl(format, name),
    status,
    size_bytes: Math.round(bytes),
    component_count: Math.round(bytes / (40 * 1e6)),
    created_at: created,
    updated_at: created,
    last_activity_at: seen(name, extra),
  }));
  // A group serves its members' content: the mock sums them (the server
  // counts blobs shared by members once).
  const byName = new Map(repos.map((r) => [r.name, r]));
  for (const g of repos.filter((r) => r.type === 'group')) {
    const members = (g.config.group?.members ?? []).map((m) => byName.get(m)).filter((m) => m !== undefined);
    g.size_bytes = members.reduce((n, m) => n + m.size_bytes, 0);
    g.component_count = members.reduce((n, m) => n + m.component_count, 0);
  }
  return repos;
}

/** Server-side repository order: `name`, `type`, `format` or `last_activity`, `-` reverses, name breaks ties. */
export function sortRepositories(list: Repository[], sort: string | null): Repository[] {
  const desc = !!sort?.startsWith('-');
  const key = sort?.replace(/^-/, '') || 'name';
  const by: Record<string, (r: Repository) => string> = {
    name: () => '',
    type: (r) => r.type,
    format: (r) => r.format,
    last_activity: (r) => r.last_activity_at ?? '',
  };
  const pick = by[key];
  if (!pick) return list;
  return [...list].sort((a, b) => {
    const c = pick(a).localeCompare(pick(b)) * (desc ? -1 : 1);
    if (c) return c;
    return a.name.localeCompare(b.name) * (key === 'name' && desc ? -1 : 1);
  });
}

export const blobStores: BlobStore[] = [
  {
    id: 'b_01', name: 's3-eu-primary', kind: 's3', state: 'ok', location: 's3://acme-artifacts/hangar', soft_quota_bytes: 12 * TB,
    used_bytes: 4.62 * TB, capacity_bytes: 12 * TB, repository_count: 9, blob_count: 1_204_331, reclaimable_bytes: 48 * GB,
    dedup_saved_bytes: 1.3 * TB, created_at: '2026-01-10T08:00:00Z', write_latency_ms: 24, encryption: 'SSE-S3',
  },
  {
    id: 'b_02', name: 'fs-local', kind: 'fs', state: 'unknown', location: '/var/lib/hangar/blobs', fs: { path: '/var/lib/hangar/blobs' },
    used_bytes: 200 * GB, capacity_bytes: null, repository_count: 14, blob_count: 88_120, reclaimable_bytes: 0,
    dedup_saved_bytes: 12 * GB, created_at: '2026-01-10T08:00:00Z', write_latency_ms: null, encryption: 'none',
  },
  {
    id: 'b_03', name: 's3-archive', kind: 's3', state: 'read_only', location: 's3://acme-archive/hangar', soft_quota_bytes: 40 * TB,
    used_bytes: 18 * TB, capacity_bytes: 40 * TB, repository_count: 1, blob_count: 3_002_118, reclaimable_bytes: 0,
    dedup_saved_bytes: 2.2 * TB, created_at: '2025-03-02T08:00:00Z', write_latency_ms: 61, encryption: 'SSE-KMS',
  },
];

export function blobStoreList(): BlobStoreList {
  return {
    items: blobStores,
    totals: {
      used_bytes: blobStores.reduce((n, b) => n + b.used_bytes, 0),
      capacity_bytes: null,
      dedup_saved_bytes: blobStores.reduce((n, b) => n + (b.dedup_saved_bytes ?? 0), 0),
      growth_bytes_per_day: 38 * GB,
      days_to_full: null,
      reclaimable_bytes: 48 * GB,
    },
  };
}

export const cleanupPolicies: CleanupPolicy[] = [
  {
    id: 'cp_01', name: 'keep-last-20', description: 'keep last 20 · delete unused after 90 days', format: null,
    criteria: { keep_newest: 20, not_downloaded_days: 90 }, repositories: ['docker-dev'], schedule: '0 2 * * *',
    enabled: true, schedule_text: 'Every day at 02:00 UTC', created_at: '2026-02-01T10:00:00Z',
    updated_at: '2026-02-01T10:00:00Z', effective_repositories: ['docker-dev'], task_id: 'cleanup/keep-last-20',
    next_run_at: null, last_run: null,
  },
  {
    id: 'cp_02', name: 'snapshots-14d', description: 'snapshots older than 14 days', format: 'maven',
    criteria: { older_than_days: 14, prerelease_only: true }, repositories: ['maven-snapshots'], schedule: '0 3 * * *',
    enabled: true, schedule_text: 'Every day at 03:00 UTC', created_at: '2026-02-01T10:00:00Z',
    updated_at: '2026-02-01T10:00:00Z', effective_repositories: ['maven-snapshots'], task_id: 'cleanup/snapshots-14d',
    next_run_at: null, last_run: null,
  },
];
