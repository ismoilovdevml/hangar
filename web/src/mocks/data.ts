/**
 * Sample data for mock mode (VITE_API_MOCKS=true) and unit tests.
 * Never imported by production code paths.
 */
import type {
  AuthProviders,
  ClientEndpoint,
  OverviewMetrics,
  OverviewRange,
  PublishedComponent,
  Session,
  SystemStatus,
  UpstreamHealth,
} from '../api/types';

const TB = 1e12;
const GB = 1e9;

export const session: Session = {
  user: {
    id: 'u_01',
    username: 'd.ismoilov',
    display_name: 'Dilshod Ismoilov',
    email: 'd.ismoilov@acme.example',
  },
  organization: {
    slug: 'acme-platform',
    name: 'acme-platform',
    environment: 'prod',
    region: 'eu-central',
  },
};

export const systemStatus: SystemStatus = {
  state: 'ok',
  message: 'All systems normal',
  version: '1.0.0',
  instance_name: 'acme-platform',
  nodes_up: 3,
  nodes_total: 3,
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
    return {
      start: new Date(now - (buckets - i) * stepMs).toISOString(),
      downloads: Math.round(d * scale * weekdayDip),
      uploads: Math.round(u * scale * 0.04 * weekdayDip),
    };
  });
}

export function overview(range: OverviewRange, now = Date.now()): OverviewMetrics {
  const r = REQUESTS[range];
  return {
    range,
    generated_at: new Date(now).toISOString(),
    summary: { published_since_yesterday: 214, failed_pushes: 0 },
    storage: { used_bytes: 4.82 * TB, capacity_bytes: 12 * TB, added_bytes_24h: 38 * GB },
    requests: { total: r.total, trend_pct: r.trend, peak_total: r.peak, p95_ms: 41 },
    cache: { hit_ratio: 0.942, bytes_saved: 2.1 * TB },
    issues: { critical: 7, packages_affected: 3, quarantined: 2, open_findings: 39 },
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

export const upstreams: UpstreamHealth[] = [
  { name: 'Docker Hub', url: 'https://registry-1.docker.io', state: 'healthy', latency_ms: 88 },
  { name: 'npmjs.org', url: 'https://registry.npmjs.org', state: 'slow', latency_ms: 412 },
  { name: 'Maven Central', url: 'https://repo1.maven.org/maven2', state: 'healthy', latency_ms: 64 },
  { name: 'PyPI', url: 'https://pypi.org', state: 'healthy', latency_ms: 95 },
  { name: 'ghcr.io', url: 'https://ghcr.io', state: 'blocked', latency_ms: null },
];

export const clientEndpoints: ClientEndpoint[] = [
  { format: 'docker', endpoint: 'cr.acme.io', repository: 'docker-group' },
  { format: 'npm', endpoint: 'https://pkg.acme.io/npm/', repository: 'npm-group' },
  { format: 'pypi', endpoint: 'https://pkg.acme.io/pypi/simple', repository: 'pypi-group' },
];
