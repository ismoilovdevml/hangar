/**
 * Mock vulnerability findings, waivers, the trend, the scanner status and
 * on-demand scans for unit tests and offline UI work. Shapes follow
 * api/openapi.yaml; component ids match the catalog mock.
 */
import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import type { ApiError, Finding, FindingTrendPoint, Format, ScanningStatus, Severity, WaiverInput } from '../api/types';

const api = (path: string) => `*/api/v1${path}`;
type Guard = (request: Request, mutating?: boolean) => Response | null;

const DAY = 86_400_000;
const SCANNED_FORMATS: Format[] = ['go', 'maven', 'npm', 'nuget', 'pypi'];

interface Seed {
  id: string;
  advisory: string;
  aliases: string[];
  title: string;
  severity: Severity;
  cvss: number | null;
  vector?: string;
  comp: { id: string; repository: string; format: Format; namespace: string; name: string; version: string };
  pkg: string;
  fixedIn: string | null;
  affected: string;
  daysAgo: number;
  status?: Finding['status'];
  quarantined?: boolean;
}

const log4j = { id: 'c_log4j', repository: 'maven-central', format: 'maven' as const, namespace: 'org.apache.logging.log4j', name: 'log4j-core', version: '2.24.1' };
const ledger = { id: 'c_ledger', repository: 'maven-releases', format: 'maven' as const, namespace: 'com.acme', name: 'ledger-core', version: '7.0.0' };
const uikit = { id: 'c_309', repository: 'npm-internal', format: 'npm' as const, namespace: '@acme', name: 'ui-kit', version: '5.3.0' };
const mlutils = { id: 'c_1207', repository: 'pypi-internal', format: 'pypi' as const, namespace: '', name: 'acme-ml-utils', version: '0.9.4' };

const SEEDS: Seed[] = [
  { id: 'f_1', advisory: 'GHSA-ghhp-997w-qr28', aliases: ['CVE-2021-26701'], title: 'Remote code execution in System.Text.Encodings.Web', severity: 'critical', cvss: 9.8,
    vector: 'CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H', comp: ledger, pkg: 'com.acme:ledger-core', fixedIn: '7.0.1', affected: '>=7.0.0, <7.0.1', daysAgo: 2, quarantined: true },
  { id: 'f_2', advisory: 'GHSA-p6mc-m468-83gw', aliases: ['CVE-2020-8203'], title: 'Prototype pollution in lodash', severity: 'high', cvss: 7.4,
    vector: 'CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:N/I:H/A:H', comp: uikit, pkg: '@acme/ui-kit', fixedIn: '5.3.2', affected: '<5.3.2', daysAgo: 6 },
  { id: 'f_3', advisory: 'GHSA-35jh-r3h4-6jhm', aliases: ['CVE-2021-23337'], title: 'Command injection in template helpers', severity: 'high', cvss: 7.2,
    comp: uikit, pkg: '@acme/ui-kit', fixedIn: '5.4.0', affected: '<5.4.0', daysAgo: 12, status: 'waived' },
  { id: 'f_4', advisory: 'GHSA-xxjr-mmjv-4gpg', aliases: ['CVE-2025-13465'], title: 'Uncontrolled recursion when parsing nested input', severity: 'medium', cvss: 6.9,
    vector: 'CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:N/VI:N/VA:L/SC:N/SI:N/SA:N', comp: log4j, pkg: 'org.apache.logging.log4j:log4j-core', fixedIn: null, affected: '>=2.0, <=2.24.3', daysAgo: 20 },
  { id: 'f_5', advisory: 'PYSEC-2026-11', aliases: [], title: 'Path traversal in archive extraction', severity: 'low', cvss: 3.1, comp: mlutils, pkg: 'acme-ml-utils',
    fixedIn: '0.9.5', affected: '<0.9.5', daysAgo: 27 },
  { id: 'f_6', advisory: 'GHSA-29mw-wpgm-hmr9', aliases: ['CVE-2020-28500'], title: 'Regular expression denial of service', severity: 'medium', cvss: 5.3, comp: uikit,
    pkg: '@acme/ui-kit', fixedIn: '5.3.0', affected: '<5.3.0', daysAgo: 25, status: 'fixed' },
];

function toFinding(s: Seed, now: number): Finding {
  const first = new Date(now - s.daysAgo * DAY).toISOString();
  const waived = s.status === 'waived';
  return {
    id: s.id,
    kind: 'vulnerability',
    advisory_id: s.advisory,
    aliases: s.aliases,
    title: s.title,
    details: `${s.title}.\n\nUpgrade to ${s.fixedIn ?? 'a fixed release when one is published'}.`,
    severity: s.severity,
    cvss: s.cvss,
    cvss_vector: s.vector ?? null,
    package: s.pkg,
    installed: s.comp.version,
    fixed_in: s.fixedIn,
    fixed_versions: s.fixedIn ? [s.fixedIn] : [],
    affected_versions: s.affected,
    references: [
      { type: 'ADVISORY', url: `https://github.com/advisories/${s.advisory}` },
      { type: 'WEB', url: `https://security.example.com/advisories/${s.advisory}` },
    ],
    status: s.status ?? 'open',
    quarantined: !!s.quarantined,
    affected_components: 1,
    waiver_reason: waived ? 'Only used by the build, never shipped' : null,
    waiver_until: waived ? new Date(now + 21 * DAY).toISOString() : null,
    waived_by: waived ? 'admin' : null,
    waived_at: waived ? new Date(now - 3 * DAY).toISOString() : null,
    assignee: null,
    source: 'osv',
    first_seen_at: first,
    last_seen_at: new Date(now - 3_600_000).toISOString(),
    resolved_at: s.status === 'fixed' ? new Date(now - 4 * DAY).toISOString() : null,
    advisory_published_at: new Date(now - (s.daysAgo + 300) * DAY).toISOString(),
    advisory_modified_at: first,
    component: s.comp,
  };
}

const initialState = () => ({ findings: SEEDS.map((s) => toFinding(s, Date.now())), enabled: true });
let state = initialState();

export function resetFindingsState(): void {
  state = initialState();
}

/** Test helpers. */
export const findingsState = {
  get findings(): Finding[] {
    return state.findings;
  },
  set enabled(v: boolean) {
    state.enabled = v;
  },
};

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });
const open = (f: Finding) => f.status === 'open' || f.status === 'assigned';

function counts(list: Finding[], pick: (f: Finding) => boolean) {
  const c = { critical: 0, high: 0, medium: 0, low: 0, unknown: 0 };
  for (const f of list) if (pick(f)) c[f.severity]++;
  return c;
}

const RANK: Record<Severity, number> = { critical: 0, high: 1, medium: 2, low: 3, unknown: 4 };

function trend(list: Finding[], now: number): FindingTrendPoint[] {
  const end = Math.floor(now / DAY) * DAY;
  return Array.from({ length: 30 }, (_, i) => {
    const day = end - (29 - i) * DAY;
    const next = day + DAY;
    const alive = list.filter((f) => Date.parse(f.first_seen_at!) < next && (!f.resolved_at || Date.parse(f.resolved_at) >= next));
    const c = counts(alive, () => true);
    return {
      date: new Date(day).toISOString().slice(0, 10),
      critical: c.critical,
      high: c.high,
      medium: c.medium,
      low: c.low + c.unknown,
      new: list.filter((f) => Date.parse(f.first_seen_at!) >= day && Date.parse(f.first_seen_at!) < next).length,
      resolved: list.filter((f) => f.resolved_at && Date.parse(f.resolved_at) >= day && Date.parse(f.resolved_at) < next).length,
    };
  });
}

export function findingsHandlers(guard: Guard, readableRepos: () => string[], repoFormat: (name: string) => Format | undefined) {
  const withGuard =
    (resolver: HttpResponseResolver, mutating = false): HttpResponseResolver =>
    (info) =>
      guard(info.request, mutating) ?? resolver(info);
  const readable = () => state.findings.filter((f) => readableRepos().includes(f.component!.repository));
  const find = (id: string) => readable().find((f) => f.id === id);

  return [
    http.get(
      api('/findings'),
      withGuard(({ request }) => {
        const p = new URL(request.url).searchParams;
        const repo = p.get('repository');
        const scoped = readable().filter((f) => !repo || f.component!.repository === repo);
        const severities = (p.get('severity') ?? '').split(',').filter(Boolean);
        const statuses = (p.get('status') ?? '').split(',').filter(Boolean);
        const fix = p.get('fix_available');
        const q = (p.get('q') ?? '').toLowerCase();
        const rows = scoped
          .filter((f) => (severities.length ? severities.includes(f.severity) : true))
          .filter((f) => (statuses.length ? statuses.includes(f.status) : f.status !== 'fixed'))
          .filter((f) => (fix === 'true' ? f.fixed_in !== null : fix === 'false' ? f.fixed_in === null : true))
          .filter((f) => !q || [f.advisory_id, f.package, f.title, ...(f.aliases ?? [])].some((s) => s.toLowerCase().includes(q)))
          .sort((a, b) => RANK[a.severity] - RANK[b.severity] || (b.cvss ?? -1) - (a.cvss ?? -1));
        const limit = Number(p.get('limit') ?? 50);
        const offset = Number(/^o(\d+)$/.exec(p.get('cursor') ?? '')?.[1] ?? 0);
        const items = rows.slice(offset, offset + limit).map(({ details: _d, ...f }) => f);
        return HttpResponse.json({
          items,
          next_cursor: offset + limit < rows.length ? `o${offset + limit}` : null,
          total: rows.length,
          totals: counts(scoped, open),
          fixable: counts(scoped, (f) => open(f) && f.fixed_in !== null),
          waived: scoped.filter((f) => f.status === 'waived').length,
          components: new Set(scoped.filter(open).map((f) => f.component!.id)).size,
        });
      }),
    ),

    http.get(
      api('/findings/trend'),
      withGuard(({ request }) => {
        const repo = new URL(request.url).searchParams.get('repository');
        const list = readable().filter((f) => !repo || f.component!.repository === repo);
        return HttpResponse.json({ days: 30, items: trend(list, Date.now()) });
      }),
    ),

    http.get(
      api('/findings/:id'),
      withGuard(({ params }) => {
        const f = find(String(params.id));
        return f ? HttpResponse.json(f) : error(404, { code: 'not_found', message: `finding "${String(params.id)}" not found` });
      }),
    ),

    http.post(
      api('/findings/:id/waiver'),
      withGuard(async ({ params, request }) => {
        const f = find(String(params.id));
        if (!f) return error(404, { code: 'not_found', message: 'finding not found' });
        const body = (await request.json()) as WaiverInput;
        const until = Date.parse(body.until ?? '');
        if ((body.reason ?? '').trim().length < 3) return error(400, { code: 'invalid_argument', message: 'waiver reason must be at least 3 characters' });
        if (!(until > Date.now())) return error(400, { code: 'invalid_argument', message: 'waiver expiry must be in the future' });
        if (f.status === 'fixed') return error(400, { code: 'invalid_argument', message: 'finding is fixed; nothing to waive' });
        Object.assign(f, {
          status: 'waived',
          waiver_reason: body.reason.trim(),
          waiver_until: new Date(until).toISOString(),
          waived_by: 'admin',
          waived_at: new Date().toISOString(),
          quarantined: false,
        });
        return HttpResponse.json(f);
      }, true),
    ),

    http.delete(
      api('/findings/:id/waiver'),
      withGuard(({ params }) => {
        const f = find(String(params.id));
        if (!f) return error(404, { code: 'not_found', message: 'finding not found' });
        if (f.status === 'waived') Object.assign(f, { status: 'open', waiver_reason: null, waiver_until: null, waived_by: null, waived_at: null });
        return HttpResponse.json(f);
      }, true),
    ),

    http.get(
      api('/scanning/status'),
      withGuard(() => {
        const status: ScanningStatus = {
          enabled: state.enabled,
          source: 'osv',
          source_url: 'https://api.osv.dev',
          rescan_interval: '24h0m0s',
          supported_formats: SCANNED_FORMATS,
          coverage: { scanned: 9, failed: 1, never: 2, unsupported: 6, last_scan_at: new Date(Date.now() - 40 * 60_000).toISOString() },
        };
        return HttpResponse.json(status);
      }),
    ),

    http.get(
      api('/components/:id/findings'),
      withGuard(({ params }) => {
        const id = String(params.id);
        const items = readable().filter((f) => f.component!.id === id && f.status !== 'fixed');
        return HttpResponse.json({ items, next_cursor: null, scan: scanFor(id) });
      }),
    ),

    // ":scan" is part of the last path segment.
    http.post(
      new RegExp('/api/v1/components/([^/]+):scan$'),
      withGuard(({ request }) => {
        const id = decodeURIComponent(/components\/([^/]+):scan$/.exec(new URL(request.url).pathname)![1]!);
        const items = readable().filter((f) => f.component!.id === id && f.status !== 'fixed');
        return HttpResponse.json({
          component_id: id,
          scan: scanFor(id),
          summary: { scanned: 1, failed: 0, findings: items.length, new_findings: 0, quarantined: 0, released: 0 },
        });
      }, true),
    ),

    http.post(
      new RegExp('/api/v1/repositories/([^/]+):scan$'),
      withGuard(({ request }) => {
        const name = decodeURIComponent(/repositories\/([^/]+):scan$/.exec(new URL(request.url).pathname)![1]!);
        if (!readableRepos().includes(name)) return error(404, { code: 'not_found', message: `repository "${name}" not found` });
        const format = repoFormat(name);
        if (format && !SCANNED_FORMATS.includes(format)) {
          return error(400, { code: 'invalid_argument', message: `repository "${name}": ${format === 'docker' ? 'image scanning not configured' : `no advisory database for ${format} packages`}` });
        }
        return HttpResponse.json({ repository: name, components: 12 }, { status: 202 });
      }, true),
    ),
  ];
}

const DOCKER_IDS = new Set(['c_8812', 'c_8790_p', 'c_8701_p', 'c_8790']);

/** Docker images and raw files are not scanned; c_312 waits for its first scan; the rest were scanned an hour ago. */
function scanFor(componentId: string) {
  if (DOCKER_IDS.has(componentId)) return { status: 'unsupported' as const, error: 'image scanning not configured', scanned_at: null };
  if (componentId === 'c_raw') return { status: 'unsupported' as const, error: 'no advisory database for raw packages', scanned_at: null };
  if (componentId === 'c_312') return { status: 'never' as const, scanned_at: null };
  return { status: 'ok' as const, scanned_at: new Date(Date.now() - 3_600_000).toISOString() };
}
