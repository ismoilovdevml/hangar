// Mixed traffic: 90 % reads across npm, Maven and raw, 10 % writes (new npm
// versions, Maven deploys, raw uploads and npm dist-tag updates, which take
// the per-package lock).
import http from 'k6/http';
import { check } from 'k6';
import exec from 'k6/execution';
import { BASE, options as base, token, bearer, ensureRepo, hosted, npmPublish, npmTarballPath,
  mavenDeploy, put, bytes, pick } from './lib.js';

export const options = base({
  thresholds: {
    'http_req_failed{scenario:default}': ['rate<0.001'],
    'http_req_duration{scenario:default,kind:read}': ['p(95)<250'],
    'http_req_duration{scenario:default,kind:write}': ['p(95)<1000'],
    'http_reqs{scenario:default}': ['count>0'],
  },
}, ['npm publish', 'maven deploy', 'raw upload', 'npm dist-tag', 'read npm-doc', 'read maven-meta']);

const NPM = 'load-mixed-npm';
const MAVEN = 'load-mixed-maven';
const RAW = 'load-mixed-raw';
const TAGGED = 8;

export function setup() {
  const tok = token();
  ensureRepo(tok, hosted(NPM, 'npm'));
  ensureRepo(tok, hosted(MAVEN, 'maven'));
  ensureRepo(tok, hosted(RAW, 'raw'));
  for (let v = 0; v < 20; v++) {
    npmPublish(tok, NPM, 'mixed-lib', `3.${v}.0`, 8192, true);
    mavenDeploy(tok, MAVEN, 'com.mixed', 'core', `3.${v}.0`, 16384);
  }
  // Dist-tag updates spread over a few packages: updates of one package are
  // serialised by its lock (see docs/operations/performance.md).
  for (let k = 0; k < TAGGED; k++) {
    npmPublish(tok, NPM, `mixed-tagged-${k}`, '1.0.0', 4096, true);
    npmPublish(tok, NPM, `mixed-tagged-${k}`, '1.1.0', 4096, true);
  }
  put(tok, `/repository/${RAW}/fixed/file.bin`, bytes(8192, 7), 'raw upload');
  return { tok, run: Date.now() };
}

export default function (data) {
  const auth = bearer(data.tok);
  const id = `${data.run}-${exec.vu.idInTest}-${exec.vu.iterationInScenario}`;
  const r = Math.random();
  let res;
  if (r < 0.9) {
    const which = pick(['npm-doc', 'npm-tgz', 'maven-meta', 'maven-jar', 'raw']);
    const v = `3.${Math.floor(Math.random() * 20)}.0`;
    const url = {
      'npm-doc': `${BASE}/repository/${NPM}/mixed-lib`,
      'npm-tgz': `${BASE}/repository/${NPM}/${npmTarballPath('mixed-lib', v)}`,
      'maven-meta': `${BASE}/repository/${MAVEN}/com/mixed/core/maven-metadata.xml`,
      'maven-jar': `${BASE}/repository/${MAVEN}/com/mixed/core/${v}/core-${v}.jar`,
      raw: `${BASE}/repository/${RAW}/fixed/file.bin`,
    }[which];
    res = http.get(url, { headers: auth, tags: { name: `read ${which}`, kind: 'read' } });
    check(res, { 'read 200': (x) => x.status === 200 });
    return;
  }
  const w = pick(['npm', 'maven', 'raw', 'dist-tag']);
  if (w === 'npm') {
    res = npmPublish(data.tok, NPM, `gen-${data.run}-${exec.vu.idInTest}`, `1.0.${exec.vu.iterationInScenario}`, 8192, false);
  } else if (w === 'maven') {
    mavenDeploy(data.tok, MAVEN, 'com.mixed.gen', `a${exec.vu.idInTest}`, `1.0.${exec.vu.iterationInScenario}`, 16384);
    return;
  } else if (w === 'raw') {
    res = put(data.tok, `/repository/${RAW}/gen/${id}.bin`, bytes(16384, exec.vu.iterationInScenario), 'raw upload');
  } else {
    const v = pick(['1.0.0', '1.1.0']);
    const pkg = `mixed-tagged-${Math.floor(Math.random() * TAGGED)}`;
    res = http.put(`${BASE}/repository/${NPM}/-/package/${pkg}/dist-tags/vu${exec.vu.idInTest % 4}`, JSON.stringify(v),
      { headers: Object.assign({ 'Content-Type': 'application/json' }, auth), tags: { name: 'npm dist-tag', kind: 'write' } });
  }
  check(res, { 'write ok': (x) => x.status >= 200 && x.status < 300 });
}
