/**
 * Mock vulnerability findings, the fix plan, who pulls a package, waivers,
 * assignees, the trend, the scanner status and scans for unit tests and
 * offline UI work. Shapes follow api/openapi.yaml; component ids match the
 * catalog mock.
 */
import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import type {
  ApiError,
  Assignee,
  Finding,
  FindingTrendPoint,
  FixPlan,
  Format,
  PackagePlan,
  PackageWaiverInput,
  PlanFinding,
  Puller,
  Rescan,
  ScanningStatus,
  Severity,
  UpgradeLine,
  WaiverInput,
} from '../api/types';

const api = (path: string) => `*/api/v1${path}`;
type Guard = (request: Request, mutating?: boolean) => Response | null;

const DAY = 86_400_000;
const SCANNED_FORMATS: Format[] = ['cargo', 'go', 'maven', 'npm', 'nuget', 'pypi'];

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
  /** Ranges for the fix plan; defaults to `affected`. */
  fixedVersions?: string[];
  assignee?: string;
}

const log4j = { id: 'c_log4j', repository: 'maven-central', format: 'maven' as const, namespace: 'org.apache.logging.log4j', name: 'log4j-core', version: '2.24.1' };
const ledger = { id: 'c_ledger', repository: 'maven-releases', format: 'maven' as const, namespace: 'com.acme', name: 'ledger-core', version: '7.0.0' };
const uikit = { id: 'c_309', repository: 'npm-internal', format: 'npm' as const, namespace: '@acme', name: 'ui-kit', version: '5.3.0' };
const mlutils = { id: 'c_1207', repository: 'pypi-internal', format: 'pypi' as const, namespace: '', name: 'acme-ml-utils', version: '0.9.4' };
const nuget = (id: string, name: string, version: string) => ({ id, repository: 'nuget-proxy', format: 'nuget' as const, namespace: '', name: name.toLowerCase(), version });
const xml8 = nuget('c_xml8', 'System.Security.Cryptography.Xml', '8.0.2');
const xml10 = nuget('c_xml10', 'System.Security.Cryptography.Xml', '10.0.8');
const newtonsoft = nuget('c_nj11', 'Newtonsoft.Json', '11.0.1');
const nethttp = nuget('c_http', 'System.Net.Http', '4.3.0');

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
  { id: 'f_7', advisory: 'GHSA-g8r8-53c2-pm3f', aliases: ['CVE-2026-47304'], title: '.NET XML encryption security feature bypass', severity: 'high', cvss: 8.1,
    comp: xml8, pkg: 'System.Security.Cryptography.Xml', fixedIn: '8.0.4', affected: '>=8.0.0, <8.0.4; >=10.0.0, <10.0.10', daysAgo: 8, fixedVersions: ['8.0.4', '10.0.10'] },
  { id: 'f_8', advisory: 'GHSA-g8r8-53c2-pm3f', aliases: ['CVE-2026-47304'], title: '.NET XML encryption security feature bypass', severity: 'high', cvss: 8.1,
    comp: xml10, pkg: 'System.Security.Cryptography.Xml', fixedIn: '10.0.10', affected: '>=8.0.0, <8.0.4; >=10.0.0, <10.0.10', daysAgo: 8, fixedVersions: ['8.0.4', '10.0.10'] },
  { id: 'f_9', advisory: 'GHSA-5crp-9r3c-p9vr', aliases: ['CVE-2024-21907'], title: 'Improper handling of exceptional conditions in Newtonsoft.Json', severity: 'high', cvss: 7.5,
    comp: newtonsoft, pkg: 'Newtonsoft.Json', fixedIn: '13.0.1', affected: '<13.0.1', daysAgo: 15, assignee: 'j.karimov' },
  { id: 'f_10', advisory: 'GHSA-7jgj-8wvc-jh57', aliases: ['CVE-2018-8292'], title: 'Information disclosure in System.Net.Http', severity: 'high', cvss: 7.5,
    comp: nethttp, pkg: 'System.Net.Http', fixedIn: '4.3.4', affected: '<4.3.4', daysAgo: 40 },
];

/** Downloads of the vulnerable versions: days since the last one, and who pulled them in the last 7 days. */
const USAGE: Record<string, { lastDays: number; pullers: Omit<Puller, 'last_at'>[] }> = {
  c_xml8: {
    lastDays: 0,
    pullers: [
      { label: '10.0.4.12 · NuGet', principal: null, kind: 'anonymous', address: '10.0.4.12', agent: 'NuGet', pulls: 9 },
      { label: 'ci-publisher', principal: 'ci-publisher', kind: 'service_account', address: '10.0.4.20', agent: 'NuGet', pulls: 3 },
    ],
  },
  c_xml10: { lastDays: 1, pullers: [{ label: 'ci-publisher', principal: 'ci-publisher', kind: 'service_account', address: '10.0.4.20', agent: 'NuGet', pulls: 2 }] },
  c_nj11: { lastDays: 2, pullers: [{ label: '10.0.4.17 · NuGet', principal: null, kind: 'anonymous', address: '10.0.4.17', agent: 'NuGet', pulls: 3 }] },
  c_ledger: { lastDays: 4, pullers: [{ label: 'j.karimov', principal: 'j.karimov', kind: 'user', address: '10.0.7.3', agent: 'Maven', pulls: 6 }] },
  c_309: { lastDays: 20, pullers: [] },
  c_http: { lastDays: 200, pullers: [] },
};

export const ASSIGNEES: Assignee[] = [
  { username: 'd.ismoilov', display_name: 'Dilshod Ismoilov' },
  { username: 'j.karimov', display_name: 'Jasur Karimov' },
  { username: 'm.nazarova', display_name: 'Malika Nazarova' },
  { username: 'n.tursunova', display_name: 'Nodira Tursunova' },
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
    fixed_versions: s.fixedVersions ?? (s.fixedIn ? [s.fixedIn] : []),
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
    assignee: s.assignee ?? null,
    assigned_by: s.assignee ? 'd.ismoilov' : null,
    assigned_at: s.assignee ? new Date(now - DAY).toISOString() : null,
    source: 'osv',
    first_seen_at: first,
    last_seen_at: new Date(now - 3_600_000).toISOString(),
    resolved_at: s.status === 'fixed' ? new Date(now - 4 * DAY).toISOString() : null,
    advisory_published_at: new Date(now - (s.daysAgo + 300) * DAY).toISOString(),
    advisory_modified_at: first,
    component: s.comp,
  };
}

const initialState = () => ({
  findings: SEEDS.map((s) => toFinding(s, Date.now())),
  enabled: true,
  rescan: null as Rescan | null,
  /** Repeated pulls of the fix plan are failed (error state tests). */
  planFails: false,
});
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
  set planFails(v: boolean) {
    state.planFails = v;
  },
  get rescan(): Rescan | null {
    return state.rescan;
  },
};

const RANK_MAJOR = (v: string) => Number(/^\D*(\d+)/.exec(v)?.[1] ?? 0);

function cmpVersion(a: string, b: string): number {
  const pa = a.split(/[.-]/).map((x) => Number(x) || 0);
  const pb = b.split(/[.-]/).map((x) => Number(x) || 0);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const d = (pa[i] ?? 0) - (pb[i] ?? 0);
    if (d) return d;
  }
  return 0;
}

function upgradeCommand(format: Format, pkg: string, v: string): string | null {
  switch (format) {
    case 'nuget':
      return `dotnet add package ${pkg} --version ${v}`;
    case 'npm':
      return `npm install ${pkg}@${v}`;
    case 'pypi':
      return `pip install ${pkg}==${v}`;
    case 'maven':
      return `mvn versions:use-dep-version -Dincludes=${pkg} -DdepVersion=${v} -DforceVersion=true`;
    case 'go':
      return `go get ${pkg}@v${v}`;
    default:
      return null;
  }
}

/** The fix plan the server would build, simplified: per major line the highest fix of its findings. */
function buildPlan(list: Finding[], tab: string, now: number): FixPlan {
  const planFinding = (f: Finding): PlanFinding => {
    const use = USAGE[f.component!.id];
    return {
      id: f.id,
      advisory_id: f.advisory_id,
      aliases: f.aliases ?? [],
      title: f.title,
      severity: f.severity,
      cvss: f.cvss ?? null,
      status: f.status,
      installed: f.installed,
      fixed_in: f.fixed_in ?? null,
      assignee: f.assignee ?? null,
      waiver_until: f.waiver_until ?? null,
      component_id: f.component!.id,
      repository: f.component!.repository,
      in_use: !!use && use.lastDays <= 30,
      pulls: use?.pullers.reduce((n, p) => n + p.pulls, 0) ?? 0,
      clients: use?.pullers.length ?? 0,
    };
  };
  const group = (keep: (f: Finding) => boolean): PackagePlan[] => {
    const by = new Map<string, Finding[]>();
    for (const f of list.filter(keep)) {
      const key = `${f.component!.format}:${f.package}`;
      by.set(key, [...(by.get(key) ?? []), f]);
    }
    return [...by.entries()].map(([key, fs]) => {
      const items = fs.map(planFinding).sort((a, b) => RANK[a.severity] - RANK[b.severity] || (b.cvss ?? -1) - (a.cvss ?? -1));
      const format = fs[0]!.component!.format;
      const lines = new Map<number, PlanFinding[]>();
      for (const f of items) lines.set(RANK_MAJOR(f.installed), [...(lines.get(RANK_MAJOR(f.installed)) ?? []), f]);
      const upgrades: UpgradeLine[] = [...lines.entries()]
        .sort((a, b) => a[0] - b[0])
        .map(([line, lf]) => {
          const fixes = lf.filter((f) => f.fixed_in).map((f) => f.fixed_in!);
          const to = fixes.sort(cmpVersion).at(-1) ?? null;
          return {
            line: String(line),
            from: [...new Set(lf.map((f) => f.installed))].sort(cmpVersion),
            to,
            command: to ? upgradeCommand(format, fs[0]!.package, to) : null,
            fixes: to ? fixes.length : 0,
            unfixed: lf.length - fixes.length,
          };
        });
      const comps = [...new Set(items.map((f) => f.component_id))];
      const sev = { critical: 0, high: 0, medium: 0, low: 0, unknown: 0 };
      for (const f of items) sev[f.severity]++;
      const assignees = new Set(items.map((f) => f.assignee));
      const advisories = [...new Map(items.map((f) => [f.advisory_id, { id: f.advisory_id, aliases: f.aliases, severity: f.severity, cvss: f.cvss }])).values()];
      const last = comps.map((c) => USAGE[c]?.lastDays).filter((d): d is number => d !== undefined);
      return {
        key,
        format,
        package: fs[0]!.package,
        repositories: [...new Set(items.map((f) => f.repository))].sort(),
        advisories,
        versions: [...new Set(items.map((f) => f.installed))].sort(cmpVersion),
        findings: items.length,
        max_severity: items[0]!.severity,
        max_cvss: items.reduce<number | null>((m, f) => (f.cvss != null && (m == null || f.cvss > m) ? f.cvss : m), null),
        severities: sev,
        upgrades,
        fixable: upgrades.reduce((n, u) => n + u.fixes, 0),
        pulls: comps.reduce((n, c) => n + (USAGE[c]?.pullers.reduce((m, p) => m + p.pulls, 0) ?? 0), 0),
        clients: new Set(comps.flatMap((c) => USAGE[c]?.pullers.map((p) => p.label) ?? [])).size,
        in_use: items.some((f) => f.in_use),
        last_pulled_at: last.length ? new Date(now - Math.min(...last) * DAY).toISOString() : null,
        assignee: assignees.size === 1 ? (items[0]!.assignee ?? null) : null,
        assignee_mixed: assignees.size > 1,
        items,
      };
    });
  };
  const bySeverity = (a: PackagePlan, b: PackagePlan) =>
    RANK[a.max_severity] - RANK[b.max_severity] || (b.max_cvss ?? -1) - (a.max_cvss ?? -1) || a.package.localeCompare(b.package);
  const openPlans = group((f) => f.status === 'open' || f.status === 'assigned');
  const waived = group((f) => f.status === 'waived').sort(bySeverity);
  const inUse = openPlans.filter((p) => p.in_use);
  const items =
    tab === 'waived'
      ? waived
      : tab === 'all'
        ? [...openPlans].sort(bySeverity)
        : [...openPlans].sort((a, b) => Number(b.in_use) - Number(a.in_use) || RANK[a.max_severity] - RANK[b.max_severity] || b.pulls - a.pulls || bySeverity(a, b));
  const count = (ps: PackagePlan[]) => ps.reduce((n, p) => n + p.findings, 0);
  return {
    tab: tab as FixPlan['tab'],
    window_days: 7,
    in_use_days: 30,
    pulls_since: new Date(now - 60 * DAY).toISOString().slice(0, 10),
    summary: {
      packages: openPlans.length,
      packages_in_use: inUse.length,
      packages_to_upgrade: inUse.filter((p) => p.fixable > 0).length,
      findings: count(openPlans),
      findings_in_use: count(inUse),
      findings_unused: count(openPlans) - count(inUse),
      findings_fixed_in_use: inUse.reduce((n, p) => n + p.fixable, 0),
      waived_packages: waived.length,
    },
    items,
  };
}

function planCsv(plan: FixPlan): string {
  const q = (v: string) => (/[",\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v);
  const rows = [['package', 'format', 'repositories', 'advisories', 'vulnerable_versions', 'findings', 'max_severity', 'max_cvss', 'upgrade_to', 'upgrade_commands', 'pulls_7d', 'clients_7d', 'in_use', 'assignee', 'status']];
  for (const p of plan.items) {
    rows.push([
      p.package, p.format, p.repositories.join(' '), p.advisories.map((a) => a.id).join('; '), p.versions.join(' '), String(p.findings), p.max_severity,
      p.max_cvss?.toFixed(1) ?? '', p.upgrades.map((u) => u.to ?? 'no fix').join(' / '), p.upgrades.map((u) => u.command ?? '').filter(Boolean).join('\n'),
      String(p.pulls), String(p.clients), String(p.in_use), p.assignee ?? '', plan.tab === 'waived' ? 'waived' : 'open',
    ]);
  }
  return rows.map((r) => r.map(q).join(',')).join('\n') + '\n';
}

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
      api('/vulnerabilities/fix-plan'),
      withGuard(({ request }) => {
        if (state.planFails) return error(500, { code: 'internal', message: 'database unavailable' });
        const p = new URL(request.url).searchParams;
        const tab = p.get('tab') ?? 'in_use';
        if (!['in_use', 'all', 'waived'].includes(tab)) return error(400, { code: 'invalid_argument', message: `tab "${tab}": want in_use, all or waived` });
        const repo = p.get('repository');
        const assignee = p.get('assignee') === 'me' ? 'd.ismoilov' : p.get('assignee');
        const q = (p.get('q') ?? '').toLowerCase();
        const list = readable()
          .filter((f) => f.status !== 'fixed')
          .filter((f) => !repo || f.component!.repository === repo)
          .filter((f) => !assignee || f.assignee === assignee)
          .filter((f) => !q || [f.advisory_id, f.package, f.title, ...(f.aliases ?? [])].some((x) => x.toLowerCase().includes(q)));
        const plan = buildPlan(list, tab, Date.now());
        if (p.get('format') === 'csv' || (request.headers.get('Accept') ?? '').includes('text/csv')) {
          return new HttpResponse(planCsv(plan), {
            headers: { 'Content-Type': 'text/csv; charset=utf-8', 'Content-Disposition': `attachment; filename="hangar-fix-plan-${tab}-2026-11-04.csv"` },
          });
        }
        return HttpResponse.json(plan);
      }),
    ),

    http.post(
      api('/vulnerabilities/fix-plan/assign'),
      withGuard(async ({ request }) => {
        const body = (await request.json()) as { package: string; assignee: string | null };
        const list = readable().filter((f) => f.status !== 'fixed' && `${f.component!.format}:${f.package}` === body.package);
        if (!list.length) return error(404, { code: 'not_found', message: `package "${body.package}" has no unresolved findings` });
        if (body.assignee && !ASSIGNEES.some((u) => u.username === body.assignee)) {
          return error(400, { code: 'invalid_argument', message: `assignee "${body.assignee}" is not an active user` });
        }
        for (const f of list) Object.assign(f, { assignee: body.assignee || null, assigned_by: body.assignee ? 'd.ismoilov' : null, assigned_at: body.assignee ? new Date().toISOString() : null });
        return HttpResponse.json({ key: body.package, findings: list.map((f) => f.id) });
      }, true),
    ),

    http.post(
      api('/vulnerabilities/fix-plan/waive'),
      withGuard(async ({ request }) => {
        const body = (await request.json()) as PackageWaiverInput;
        if ((body.reason ?? '').trim().length < 3) return error(400, { code: 'invalid_argument', message: 'waiver reason must be at least 3 characters' });
        const until = Date.parse(body.until ?? '');
        if (!(until > Date.now())) return error(400, { code: 'invalid_argument', message: 'waiver expiry must be in the future' });
        let list = readable().filter((f) => f.status !== 'fixed' && `${f.component!.format}:${f.package}` === body.package);
        if (!list.length) return error(404, { code: 'not_found', message: `package "${body.package}" has no unresolved findings` });
        if (body.finding_ids?.length) {
          if (body.finding_ids.some((id) => !list.some((f) => f.id === id))) return error(400, { code: 'invalid_argument', message: 'finding is not an unresolved finding of the package' });
          list = list.filter((f) => body.finding_ids!.includes(f.id));
        }
        for (const f of list) {
          Object.assign(f, { status: 'waived', waiver_reason: body.reason.trim(), waiver_until: new Date(until).toISOString(), waived_by: 'd.ismoilov', waived_at: new Date().toISOString(), quarantined: false });
        }
        return HttpResponse.json({ key: body.package, findings: list.map((f) => f.id) });
      }, true),
    ),

    http.get(
      api('/vulnerabilities/assignees'),
      withGuard(({ request }) => {
        const q = (new URL(request.url).searchParams.get('q') ?? '').toLowerCase();
        return HttpResponse.json({ items: ASSIGNEES.filter((u) => !q || u.username.includes(q) || u.display_name.toLowerCase().includes(q)) });
      }),
    ),

    http.get(
      api('/vulnerabilities/rescan'),
      withGuard(() => {
        const r = state.rescan;
        if (r && r.state === 'running') {
          r.done = Math.min(r.total, r.done + 6);
          if (r.done >= r.total) r.state = 'done';
        }
        return HttpResponse.json({ run: r });
      }),
    ),

    http.post(
      api('/vulnerabilities/rescan'),
      withGuard(() => {
        if (!state.enabled) return error(501, { code: 'not_implemented', message: 'vulnerability scanning is turned off (scanning.enabled)' });
        if (state.rescan?.state === 'running') return HttpResponse.json({ run: state.rescan, started: false });
        state.rescan = { id: 'r_1', started_at: new Date().toISOString(), started_by: 'd.ismoilov', repositories: 5, total: 12, done: 0, state: 'running' };
        return HttpResponse.json({ run: state.rescan, started: true }, { status: 202 });
      }, true),
    ),

    http.get(
      api('/findings/:id/pullers'),
      withGuard(({ params, request }) => {
        const f = find(String(params.id));
        if (!f) return error(404, { code: 'not_found', message: 'finding not found' });
        const window = Number(/^(\d+)/.exec(new URL(request.url).searchParams.get('window') ?? '7')?.[1] ?? 7);
        const items = (USAGE[f.component!.id]?.pullers ?? []).map((p, i) => ({ ...p, last_at: new Date(Date.now() - (i + 1) * 3_600_000).toISOString() }));
        return HttpResponse.json({
          component_id: f.component!.id,
          window_days: window,
          pulls: items.reduce((n, p) => n + p.pulls, 0),
          clients: items.length,
          pulls_since: new Date(Date.now() - 60 * DAY).toISOString().slice(0, 10),
          items,
        });
      }),
    ),

    http.put(
      api('/findings/:id/assignee'),
      withGuard(async ({ params, request }) => {
        const f = find(String(params.id));
        if (!f) return error(404, { code: 'not_found', message: 'finding not found' });
        const body = (await request.json()) as { assignee?: string };
        if (!body.assignee || !ASSIGNEES.some((u) => u.username === body.assignee)) {
          return error(400, { code: 'invalid_argument', message: `assignee "${body.assignee ?? ''}" is not an active user` });
        }
        Object.assign(f, { assignee: body.assignee, assigned_by: 'd.ismoilov', assigned_at: new Date().toISOString() });
        return HttpResponse.json(f);
      }, true),
    ),

    http.delete(
      api('/findings/:id/assignee'),
      withGuard(({ params }) => {
        const f = find(String(params.id));
        if (!f) return error(404, { code: 'not_found', message: 'finding not found' });
        Object.assign(f, { assignee: null, assigned_by: null, assigned_at: null });
        return HttpResponse.json(f);
      }, true),
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
