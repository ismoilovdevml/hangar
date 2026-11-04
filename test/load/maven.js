// Maven reads the way Maven Resolver and Gradle make them: generated
// maven-metadata.xml with its .sha1, POMs and jars from a hosted repository.
import http from 'k6/http';
import { check } from 'k6';
import { BASE, options as base, token, bearer, ensureRepo, hosted, mavenDeploy } from './lib.js';

export const options = base({}, ['maven-metadata.xml', 'maven-metadata.xml.sha1', 'pom', 'jar']);

const REPO = 'load-maven';
const GROUP = 'com.load';
const ARTIFACTS = 10;
const VERSIONS = 20;

export function setup() {
  const tok = token();
  ensureRepo(tok, hosted(REPO, 'maven'));
  for (let a = 0; a < ARTIFACTS; a++) {
    for (let v = 0; v < VERSIONS; v++) {
      mavenDeploy(tok, REPO, GROUP, `lib${a}`, `2.${v}.0`, 32768);
    }
  }
  return { tok };
}

export default function (data) {
  const auth = bearer(data.tok);
  const a = `lib${Math.floor(Math.random() * ARTIFACTS)}`;
  const v = `2.${Math.floor(Math.random() * VERSIONS)}.0`;
  const dir = `${BASE}/repository/${REPO}/${GROUP.replace(/\./g, '/')}/${a}`;
  const r = Math.random();
  let res;
  if (r < 0.3) {
    res = http.get(`${dir}/maven-metadata.xml`, { headers: auth, tags: { name: 'maven-metadata.xml' } });
  } else if (r < 0.45) {
    res = http.get(`${dir}/maven-metadata.xml.sha1`, { headers: auth, tags: { name: 'maven-metadata.xml.sha1' } });
  } else if (r < 0.7) {
    res = http.get(`${dir}/${v}/${a}-${v}.pom`, { headers: auth, tags: { name: 'pom' } });
  } else {
    res = http.get(`${dir}/${v}/${a}-${v}.jar`, { headers: auth, tags: { name: 'jar' } });
  }
  check(res, { 'status 200': (x) => x.status === 200 });
}
