// Shared helpers for the Hangar k6 load tests. See README.md.
import http from 'k6/http';
import encoding from 'k6/encoding';
import crypto from 'k6/crypto';
import { fail } from 'k6';

export const BASE = (__ENV.HANGAR_URL || 'http://127.0.0.1:8110').replace(/\/$/, '');

// Default load shape: VUs and duration can be overridden with K6 flags
// (--vus, --duration) or HANGAR_VUS / HANGAR_DURATION. names lists the
// request names to report latency for separately in the summary.
export function options(extra, names) {
  const o = Object.assign({
    vus: Number(__ENV.HANGAR_VUS || 32),
    duration: __ENV.HANGAR_DURATION || '30s',
    setupTimeout: '5m',
    summaryTrendStats: ['avg', 'med', 'p(95)', 'p(99)', 'max'],
    // Measured traffic only: setup() requests carry no scenario tag.
    thresholds: {
      'http_req_failed{scenario:default}': ['rate<0.001'],
      'http_req_duration{scenario:default}': ['p(95)<250'],
      'http_reqs{scenario:default}': ['count>0'],
    },
  }, extra || {});
  for (const n of names || []) {
    o.thresholds[`http_req_duration{scenario:default,name:${n}}`] = ['p(95)<5000'];
  }
  return o;
}

// token returns a bearer token: $HANGAR_TOKEN, or a personal access token
// minted with $HANGAR_ADMIN_USER / $HANGAR_ADMIN_PASSWORD.
export function token() {
  if (__ENV.HANGAR_TOKEN) {
    return __ENV.HANGAR_TOKEN;
  }
  const user = __ENV.HANGAR_ADMIN_USER || 'admin';
  const pass = __ENV.HANGAR_ADMIN_PASSWORD;
  if (!pass) {
    fail('set HANGAR_TOKEN, or HANGAR_ADMIN_PASSWORD to mint one');
  }
  const expires = new Date(Date.now() + 24 * 3600 * 1000).toISOString();
  const res = http.post(`${BASE}/api/v1/tokens`,
    JSON.stringify({ name: `k6-${Date.now()}`, scopes: ['repo:*:admin', 'system:repos:admin'], expires_at: expires }),
    { headers: { 'Content-Type': 'application/json', Authorization: basic(user, pass) } });
  if (res.status !== 201) {
    fail(`create token: ${res.status} ${res.body}`);
  }
  return res.json('secret');
}

export function basic(user, pass) {
  return 'Basic ' + encoding.b64encode(`${user}:${pass}`);
}

export function bearer(tok) {
  return { Authorization: `Bearer ${tok}` };
}

// ensureRepo creates a repository unless it already exists.
export function ensureRepo(tok, repo) {
  const res = http.post(`${BASE}/api/v1/repositories`, JSON.stringify(repo),
    { headers: Object.assign({ 'Content-Type': 'application/json' }, bearer(tok)) });
  if (res.status !== 201 && res.status !== 409) {
    fail(`create repository ${repo.name}: ${res.status} ${res.body}`);
  }
}

export function hosted(name, format) {
  return { name, format, type: 'hosted', blob_store: 'default', config: { hosted: { write_policy: 'allow' } } };
}

// bytes returns n deterministic pseudo-random bytes.
export function bytes(n, seed) {
  const out = new Uint8Array(n);
  let x = (seed || 1) >>> 0;
  for (let i = 0; i < n; i++) {
    x = (x * 1664525 + 1013904223) >>> 0;
    out[i] = x >>> 24;
  }
  return out.buffer;
}

function expect(res, codes, what) {
  if (!codes.includes(res.status)) {
    fail(`${what}: ${res.status} ${res.body}`);
  }
  return res;
}

// npmPublish publishes name@version with a size-byte tarball. An existing
// version (403) is accepted when allowExisting is set, so setup can re-run.
export function npmPublish(tok, repo, name, version, size, allowExisting) {
  const tgz = bytes(size, version.length * 7919 + name.length);
  const file = `${name.replace(/^@[^/]+\//, '')}-${version}.tgz`;
  const body = {
    _id: name, name, description: 'load test package',
    'dist-tags': { latest: version },
    versions: { [version]: {
      name, version, description: 'load test package', main: 'index.js',
      dependencies: { 'left-pad': '^1.3.0', lodash: '^4.17.21' },
      dist: { shasum: crypto.sha1(tgz, 'hex'), integrity: 'sha512-' + crypto.sha512(tgz, 'base64'), tarball: `${BASE}/x/${file}` },
    } },
    _attachments: { [file]: { content_type: 'application/octet-stream', data: encoding.b64encode(tgz), length: size } },
  };
  const res = http.put(`${BASE}/repository/${repo}/${name.replace('/', '%2f')}`, JSON.stringify(body),
    { headers: Object.assign({ 'Content-Type': 'application/json' }, bearer(tok)), tags: { name: 'npm publish', kind: 'write' } });
  return expect(res, allowExisting ? [201, 403] : [201], `npm publish ${name}@${version}`);
}

export function npmTarballPath(name, version) {
  return `${name}/-/${name.replace(/^@[^/]+\//, '')}-${version}.tgz`;
}

// put uploads data to a repository path (raw, maven).
export function put(tok, path, data, tagName) {
  const res = http.put(`${BASE}${path}`, data, { headers: bearer(tok), tags: { name: tagName || 'PUT', kind: 'write' } });
  return expect(res, [200, 201, 204], `PUT ${path}`);
}

// mavenDeploy deploys a pom and a size-byte jar for group:artifact:version.
export function mavenDeploy(tok, repo, group, artifact, version, size) {
  const dir = `/repository/${repo}/${group.replace(/\./g, '/')}/${artifact}/${version}/${artifact}-${version}`;
  put(tok, `${dir}.pom`, `<project><modelVersion>4.0.0</modelVersion><groupId>${group}</groupId>` +
    `<artifactId>${artifact}</artifactId><version>${version}</version></project>`, 'maven deploy');
  put(tok, `${dir}.jar`, bytes(size, version.length + 31), 'maven deploy');
}

// registryToken gets an OCI registry token for pull,push on name.
export function registryToken(tok, name) {
  const res = http.get(`${BASE}/v2/token?service=hangar&scope=repository:${name}:pull,push`, { headers: bearer(tok) });
  expect(res, [200], 'registry token');
  return res.json('token');
}

// dockerPushBlob uploads a blob monolithically and returns its digest.
export function dockerPushBlob(regTok, name, size, seed) {
  const data = bytes(size, seed);
  const digest = 'sha256:' + crypto.sha256(data, 'hex');
  const res = http.post(`${BASE}/v2/${name}/blobs/uploads/?digest=${digest}`, data,
    { headers: { Authorization: `Bearer ${regTok}`, 'Content-Type': 'application/octet-stream' } });
  expect(res, [201], `push blob ${digest}`);
  return digest;
}

export function pick(arr) {
  return arr[Math.floor(Math.random() * arr.length)];
}
