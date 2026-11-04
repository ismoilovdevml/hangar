/**
 * Mock packages (components, assets), search and blob store management for
 * unit tests and offline UI work. Shapes follow api/openapi.yaml; the endpoints
 * the server does not implement yet (dependents, findings) answer 501 as it does.
 */
import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import type {
  ApiError,
  Asset,
  BlobStore,
  BlobStoreInput,
  ComponentDetail,
  FacetValue,
  Format,
  PublishedComponent,
  ScanSummary,
  SearchHit,
} from '../api/types';
import { tokenize } from '../lib/searchQuery';
import { blobStoreList, blobStores } from './data';

const api = (path: string) => `*/api/v1${path}`;
type Guard = (request: Request, mutating?: boolean) => Response | null;

const clean: ScanSummary = { state: 'clean', critical: 0, high: 0, medium: 0, low: 0 };
const pending: ScanSummary = { state: 'pending', critical: 0, high: 0, medium: 0, low: 0 };
const BASE_URL = 'https://pkg.acme.io';
const HOST = 'cr.acme.io';

const hex = (seed: string, len = 64) => {
  let h = 2166136261;
  let out = '';
  while (out.length < len) {
    for (const ch of seed + out.length) h = Math.imul(h ^ ch.charCodeAt(0), 16777619) >>> 0;
    out += h.toString(16).padStart(8, '0');
  }
  return out.slice(0, len);
};

interface Seed {
  id: string;
  repo: string;
  format: Format;
  ns?: string;
  name: string;
  version: string;
  daysAgo: number;
  size: number;
  downloads: number;
  scan?: ScanSummary;
  by?: PublishedComponent['publisher'];
  extra?: Partial<ComponentDetail>;
}

const ci = (n: number): PublishedComponent['publisher'] => ({ kind: 'ci', display_name: `CI · build ${n}` });

const SEEDS: Seed[] = [
  { id: 'c_8812', repo: 'docker-prod', format: 'docker', name: 'payments-api', version: '2.14.0', daysAgo: 0, size: 186e6, downloads: 12408, by: ci(8812),
    extra: { signed: true, platforms: ['linux/amd64', 'linux/arm64'], base_image: 'distroless/java21', source_url: 'https://git.acme.io/payments/commit/7c1e9a2', downloads_30d: 12408, last_downloaded_by: 'k8s-prod' } },
  { id: 'c_8790_p', repo: 'docker-prod', format: 'docker', name: 'payments-api', version: '2.13.2', daysAgo: 7, size: 184e6, downloads: 3020, by: ci(8790) },
  { id: 'c_8701_p', repo: 'docker-prod', format: 'docker', name: 'payments-api', version: '2.13.1', daysAgo: 14, size: 183e6, downloads: 902, by: ci(8701), scan: { state: 'findings', critical: 0, high: 1, medium: 0, low: 2 } },
  { id: 'c_8790', repo: 'docker-prod', format: 'docker', name: 'auth-gateway', version: '3.2.0', daysAgo: 0, size: 92e6, downloads: 220, by: ci(8790), scan: { state: 'findings', critical: 1, high: 0, medium: 0, low: 0 } },
  { id: 'c_311', repo: 'npm-internal', format: 'npm', ns: '@acme', name: 'ui-kit', version: '5.3.1', daysAgo: 0, size: 412_000, downloads: 8100, by: ci(311), extra: { license: 'MIT', attributes: { description: 'Acme UI components', license: 'MIT' } } },
  { id: 'c_309', repo: 'npm-internal', format: 'npm', ns: '@acme', name: 'ui-kit', version: '5.3.0', daysAgo: 9, size: 409_000, downloads: 2400, by: ci(309) },
  { id: 'c_312', repo: 'npm-internal', format: 'npm', ns: '@acme', name: 'eslint-config', version: '3.0.1', daysAgo: 2, size: 12_000, downloads: 640, by: ci(312), scan: pending },
  { id: 'c_ledger', repo: 'maven-releases', format: 'maven', ns: 'com.acme', name: 'ledger-core', version: '7.0.0', daysAgo: 3, size: 2.4e6, downloads: 9000, by: { kind: 'user', display_name: 'Jasur Karimov' } },
  { id: 'c_log4j', repo: 'maven-central', format: 'maven', ns: 'org.apache.logging.log4j', name: 'log4j-core', version: '2.24.1', daysAgo: 30, size: 1.9e6, downloads: 84_000, by: { kind: 'service_account', display_name: 'proxy' } },
  { id: 'c_1207', repo: 'pypi-internal', format: 'pypi', name: 'acme-ml-utils', version: '0.9.4', daysAgo: 1, size: 88_000, downloads: 310, by: ci(1207) },
  { id: 'c_raw', repo: 'raw-artifacts', format: 'raw', ns: 'releases/1.4.0', name: 'app-linux-amd64.tar.gz', version: '', daysAgo: 4, size: 48e6, downloads: 52, by: ci(1400) },
];

const repoUrl = (format: string, repo: string) => (format === 'docker' ? `${HOST}/${repo}` : `${BASE_URL}/repository/${repo}/`);

function installCommand(s: Seed): string {
  const url = repoUrl(s.format, s.repo);
  const full = s.ns ? `${s.ns}/${s.name}` : s.name;
  switch (s.format) {
    case 'npm':
      return `npm install ${full}@${s.version} --registry ${url}`;
    case 'maven':
      return `mvn dependency:get -Dartifact=${s.ns}:${s.name}:${s.version} -DremoteRepositories=${url}`;
    case 'pypi':
      return `pip install ${s.name}==${s.version} --index-url ${url}simple/`;
    case 'docker':
      return `docker pull ${url}/${full}:${s.version}`;
    default:
      return `curl -fLO ${url}${full}`;
  }
}

function toDetail(s: Seed, now: number): ComponentDetail {
  const sha = hex(s.id);
  return {
    id: s.id,
    format: s.format,
    namespace: s.ns ?? '',
    name: s.name,
    version: s.version,
    repository: s.repo,
    publisher: s.by ?? { kind: 'user', display_name: 'admin' },
    published_at: new Date(now - s.daysAgo * 86_400_000 - 60_000).toISOString(),
    scan: s.scan ?? clean,
    download_count: s.downloads,
    last_downloaded_at: s.downloads ? new Date(now - 120_000).toISOString() : null,
    size_bytes: s.size,
    digest: `sha256:${sha}`,
    checksums: { sha1: hex(`${s.id}1`, 40), sha256: sha },
    install_command: installCommand(s),
    signed: false,
    quarantined: false,
    ...s.extra,
  };
}

function assetsFor(c: ComponentDetail): Asset[] {
  const created = c.published_at;
  if (c.format === 'docker') {
    const layers: [string, number][] = [['manifest', 1_800], ['config', 7_400], ['layer-1', 92e6], ['layer-2', 71e6], ['layer-3', 18e6]];
    return layers.map(([p, size], i) => ({
      id: `${c.id}_a${i}`,
      path: `${c.name}/blobs/sha256:${hex(`${c.id}${p}`)}`,
      kind: i < 2 ? 'metadata' : 'content',
      blob_digest: `sha256:${hex(`${c.id}${p}`)}`,
      content_type: i === 0 ? 'application/vnd.oci.image.manifest.v1+json' : 'application/vnd.oci.image.layer.v1.tar+gzip',
      size_bytes: size,
      created_at: created,
      last_downloaded_at: null,
    }));
  }
  const full = c.namespace ? `${c.namespace}/${c.name}` : c.name;
  const file =
    c.format === 'npm'
      ? `${full}/-/${c.name}-${c.version}.tgz`
      : c.format === 'maven'
        ? `${(c.namespace ?? '').replace(/\./g, '/')}/${c.name}/${c.version}/${c.name}-${c.version}.jar`
        : c.format === 'pypi'
          ? `packages/${c.name}/${c.name.replace(/-/g, '_')}-${c.version}-py3-none-any.whl`
          : full;
  const sha = c.checksums.sha256 ?? hex(c.id);
  return [
    {
      id: `${c.id}_a0`,
      path: file,
      kind: 'content',
      blob_digest: `sha256:${sha}`,
      content_type: 'application/octet-stream',
      size_bytes: c.size_bytes,
      checksums: { sha256: sha },
      download_url: `${BASE_URL}/repository/${c.repository}/${file}`,
      created_at: created,
      last_downloaded_at: c.last_downloaded_at ?? null,
    },
  ];
}

function initialState() {
  const now = Date.now();
  return {
    components: SEEDS.map((s) => toDetail(s, now)),
    blobStores: blobStores.map((b) => ({ ...b })),
  };
}

let state = initialState();

export function resetCatalogState(): void {
  state = initialState();
}

export const catalogState = {
  get components(): readonly ComponentDetail[] {
    return state.components;
  },
  get blobStores(): readonly BlobStore[] {
    return state.blobStores;
  },
};

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });
const notImplemented = (what: string) => error(501, { code: 'not_implemented', message: `${what} is not implemented yet` });

const offsetOf = (cursor: string | null) => {
  const m = cursor ? /^o(\d+)$/.exec(cursor) : null;
  return m ? Number(m[1]) : 0;
};
function page<T>(rows: T[], params: URLSearchParams, defLimit = 50) {
  const limit = Math.min(1000, Math.max(1, Number(params.get('limit') ?? defLimit)));
  const offset = offsetOf(params.get('cursor'));
  return { items: rows.slice(offset, offset + limit), next_cursor: offset + limit < rows.length ? `o${offset + limit}` : null };
}

const toPublished = (c: ComponentDetail): PublishedComponent => ({
  id: c.id,
  format: c.format,
  namespace: c.namespace,
  name: c.name,
  version: c.version,
  repository: c.repository,
  publisher: c.publisher,
  published_at: c.published_at,
  scan: c.scan,
  download_count: c.download_count,
  last_downloaded_at: c.last_downloaded_at,
});

const byNewest = (a: { published_at: string }, b: { published_at: string }) => b.published_at.localeCompare(a.published_at);
const pkgKey = (c: ComponentDetail) => `${c.repository}\u0000${c.namespace ?? ''}\u0000${c.name}`;

function coordinates(c: ComponentDetail): string {
  const full = c.namespace ? (c.format === 'maven' ? `${c.namespace}:${c.name}` : `${c.namespace}/${c.name}`) : c.name;
  if (!c.version) return full;
  return c.format === 'maven' || c.format === 'docker' ? `${full}:${c.version}` : c.format === 'pypi' ? `${full}==${c.version}` : `${full}@${c.version}`;
}

/** A small version of the server's search: prefix-ish text match, qualifiers, facets that ignore their own filter. */
function runSearch(q: string, sort: string, readable: Set<string>) {
  const toks = tokenize(q);
  const words = toks.filter((t) => !t.key).map((t) => t.text.toLowerCase());
  const values = (k: string) => toks.filter((t) => t.key === k).map((t) => (t.value ?? '').toLowerCase());
  const formats = values('format');
  const repos = toks.filter((t) => t.key === 'repo').map((t) => t.value ?? '');
  const security = values('security');
  const shas = values('sha256');
  for (const s of shas) if (!/^[0-9a-f]{4,64}$/.test(s)) throw new Error(`sha256:${s}: want 4 to 64 hex digits`);
  for (const s of security) if (!['pending', 'clean', 'findings', 'quarantined'].includes(s)) throw new Error(`security:${s}: want pending, clean, findings or quarantined`);

  const match = (c: ComponentDetail, skip: string) =>
    readable.has(c.repository) &&
    words.every((w) => `${c.namespace}/${c.name}`.toLowerCase().includes(w)) &&
    (skip === 'format' || !formats.length || formats.includes(c.format)) &&
    (skip === 'repo' || !repos.length || repos.includes(c.repository)) &&
    (skip === 'security' || !security.length || security.includes(c.scan.state)) &&
    (!shas.length || assetsFor(c).some((a) => shas.some((s) => a.blob_digest.startsWith(`sha256:${s}`))));

  const group = (skip: string) => {
    const g = new Map<string, ComponentDetail[]>();
    for (const c of state.components) {
      if (!match(c, skip)) continue;
      g.set(pkgKey(c), [...(g.get(pkgKey(c)) ?? []), c]);
    }
    return [...g.values()].map((cs) => ({ newest: [...cs].sort(byNewest)[0]!, versions: cs.length, downloads: cs.reduce((n, c) => n + (c.download_count ?? 0), 0) }));
  };
  const facet = (skip: string, value: (c: ComponentDetail) => string): FacetValue[] => {
    const counts = new Map<string, number>();
    for (const p of group(skip)) counts.set(value(p.newest), (counts.get(value(p.newest)) ?? 0) + 1);
    return [...counts].map(([v, count]) => ({ value: v, count })).sort((a, b) => b.count - a.count || a.value.localeCompare(b.value));
  };

  const hits: SearchHit[] = group('').map(({ newest: c, versions, downloads }) => ({
    component_id: c.id,
    format: c.format,
    coordinates: coordinates(c),
    namespace: c.namespace,
    name: c.name,
    version: c.version,
    repository: c.repository,
    versions,
    downloads,
    security: c.scan.state,
    published_at: c.published_at,
  }));
  hits.sort((a, b) =>
    sort === '-downloads' ? b.downloads - a.downloads : sort === '-published_at' ? (b.published_at ?? '').localeCompare(a.published_at ?? '') : a.coordinates.localeCompare(b.coordinates),
  );
  return {
    hits,
    facets: {
      format: facet('format', (c) => c.format),
      security: facet('security', (c) => c.scan.state),
      repository: facet('repo', (c) => c.repository),
    },
  };
}

const STORE_NAME = /^[a-z0-9][a-z0-9-]{1,62}$/;

/** Handlers; `guard` is the session/CSRF check shared with the other mock handlers. */
export function catalogHandlers(guard: Guard, readableRepos: () => string[]) {
  const withGuard =
    (resolver: HttpResponseResolver, mutating = false): HttpResponseResolver =>
    (info) =>
      guard(info.request, mutating) ?? resolver(info);

  const find = (id: string) => state.components.find((c) => c.id === id && readableRepos().includes(c.repository));

  return [
    http.get(
      api('/components'),
      withGuard(({ request }) => {
        const p = new URL(request.url).searchParams;
        const repository = p.get('repository');
        const name = p.get('name');
        // Without these filters, the overview's "recently published" handler answers.
        if (!repository && !name) return undefined;
        const rows = state.components
          .filter((c) => readableRepos().includes(c.repository))
          .filter((c) => (!repository || c.repository === repository) && (!name || c.name === name))
          .sort(byNewest)
          .map(toPublished);
        return HttpResponse.json(page(rows, p));
      }),
    ),

    http.get(
      api('/components/:id'),
      withGuard(({ params }) => {
        const c = find(String(params.id));
        return c ? HttpResponse.json(c) : error(404, { code: 'not_found', message: `component ${String(params.id)} not found` });
      }),
    ),

    http.delete(
      api('/components/:id'),
      withGuard(({ params }) => {
        const c = find(String(params.id));
        if (!c) return error(404, { code: 'not_found', message: 'component not found' });
        state.components = state.components.filter((x) => x.id !== c.id);
        return new HttpResponse(null, { status: 204 });
      }, true),
    ),

    http.get(
      api('/components/:id/versions'),
      withGuard(({ params, request }) => {
        const c = find(String(params.id));
        if (!c) return error(404, { code: 'not_found', message: 'component not found' });
        const rows = state.components.filter((x) => pkgKey(x) === pkgKey(c)).sort(byNewest).map(toPublished);
        return HttpResponse.json(page(rows, new URL(request.url).searchParams));
      }),
    ),

    http.get(
      api('/components/:id/assets'),
      withGuard(({ params, request }) => {
        const c = find(String(params.id));
        if (!c) return error(404, { code: 'not_found', message: 'component not found' });
        return HttpResponse.json(page(assetsFor(c), new URL(request.url).searchParams));
      }),
    ),

    http.get(api('/components/:id/dependents'), withGuard(() => notImplemented('endpoint GET dependents'))),
    http.get(api('/components/:id/findings'), withGuard(() => notImplemented('endpoint GET findings'))),

    http.get(
      api('/search'),
      withGuard(({ request }) => {
        const p = new URL(request.url).searchParams;
        try {
          const { hits, facets } = runSearch(p.get('q') ?? '', p.get('sort') ?? 'relevance', new Set(readableRepos()));
          return HttpResponse.json({ ...page(hits, p, 50), total: hits.length, facets });
        } catch (e) {
          return error(400, { code: 'invalid_argument', message: (e as Error).message });
        }
      }),
    ),

    http.get(
      api('/blob-stores'),
      withGuard(() => {
        const base = blobStoreList();
        return HttpResponse.json({ ...base, items: state.blobStores });
      }),
    ),

    http.get(
      api('/blob-stores/:name'),
      withGuard(({ params }) => {
        const s = state.blobStores.find((b) => b.name === params.name);
        return s ? HttpResponse.json(s) : error(404, { code: 'not_found', message: `blob store "${String(params.name)}" not found` });
      }),
    ),

    http.post(
      api('/blob-stores'),
      withGuard(async ({ request }) => {
        const body = (await request.json()) as BlobStoreInput;
        if (!STORE_NAME.test(body.name ?? '')) return error(400, { code: 'invalid_argument', message: `blob store name "${body.name}" is invalid` });
        if (body.kind === 'fs' && !body.fs?.path?.startsWith('/')) return error(400, { code: 'invalid_argument', message: 'fs blob store needs an absolute fs.path' });
        if (body.kind === 's3' && !body.s3?.bucket) return error(400, { code: 'invalid_argument', message: 's3 blob store needs s3.bucket' });
        if (state.blobStores.some((b) => b.name === body.name)) return error(409, { code: 'conflict', message: `blob store "${body.name}" already exists` });
        const s3 = body.s3 ? { ...body.s3, secret_access_key: undefined } : undefined;
        const store: BlobStore = {
          id: `b_${Date.now()}`,
          name: body.name,
          kind: body.kind,
          ...(body.soft_quota_bytes ? { soft_quota_bytes: body.soft_quota_bytes } : {}),
          ...(body.fs ? { fs: body.fs } : {}),
          ...(s3 ? { s3 } : {}),
          state: 'unknown',
          location: body.kind === 'fs' ? body.fs!.path! : `s3://${body.s3!.bucket}/${body.s3!.prefix ?? ''}`,
          used_bytes: 0,
          capacity_bytes: body.soft_quota_bytes ?? null,
          repository_count: 0,
          blob_count: 0,
          reclaimable_bytes: 0,
          dedup_saved_bytes: 0,
          write_latency_ms: null,
          created_at: new Date().toISOString(),
        };
        state.blobStores = [...state.blobStores, store];
        return HttpResponse.json(store, { status: 201 });
      }, true),
    ),
  ];
}
