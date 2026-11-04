// npm reads: abbreviated and full packuments and tarballs from a hosted
// repository. One package has 100 versions (packument rendering cost grows
// with the version count), 20 small scoped packages have 3 each.
import http from 'k6/http';
import { check } from 'k6';
import { BASE, options as base, token, bearer, ensureRepo, hosted, npmPublish, npmTarballPath, pick } from './lib.js';

export const options = base({}, ['packument (abbreviated)', 'packument 100 versions (full)', 'tarball']);

const REPO = 'load-npm';
const BIG = 'load-big';
const BIG_VERSIONS = 100;

export function setup() {
  const tok = token();
  ensureRepo(tok, hosted(REPO, 'npm'));
  const pkgs = [];
  for (let v = 0; v < BIG_VERSIONS; v++) {
    npmPublish(tok, REPO, BIG, `1.${v}.0`, 4096, true);
  }
  pkgs.push({ name: BIG, versions: BIG_VERSIONS });
  for (let i = 0; i < 20; i++) {
    const name = `@load/p${i}`;
    for (let v = 0; v < 3; v++) {
      npmPublish(tok, REPO, name, `0.${v}.0`, 16384, true);
    }
    pkgs.push({ name, versions: 3 });
  }
  return { tok, pkgs };
}

export default function (data) {
  const auth = bearer(data.tok);
  const p = pick(data.pkgs);
  const r = Math.random();
  let res;
  if (r < 0.5) {
    res = http.get(`${BASE}/repository/${REPO}/${p.name.replace('/', '%2f')}`, {
      headers: Object.assign({ Accept: 'application/vnd.npm.install-v1+json' }, auth), tags: { name: 'packument (abbreviated)' } });
  } else if (r < 0.65) {
    res = http.get(`${BASE}/repository/${REPO}/${BIG}`, { headers: auth, tags: { name: 'packument 100 versions (full)' } });
  } else {
    const v = Math.floor(Math.random() * p.versions);
    const version = p.name === BIG ? `1.${v}.0` : `0.${v}.0`;
    res = http.get(`${BASE}/repository/${REPO}/${npmTarballPath(p.name, version)}`, { headers: auth, tags: { name: 'tarball' } });
  }
  check(res, { 'status 200': (x) => x.status === 200 });
}
