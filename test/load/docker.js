// OCI blob GETs (layer pulls) with a registry bearer token, as docker pull
// makes them after the token dance.
import http from 'k6/http';
import { check } from 'k6';
import { BASE, options as base, token, ensureRepo, hosted, registryToken, dockerPushBlob, pick } from './lib.js';

export const options = base({}, ['blob up to 256 KiB', 'blob 1 MiB']);

const REPO = 'load-docker';
const IMAGE = `${REPO}/app`;
const SIZES = [4096, 65536, 262144, 1048576];

export function setup() {
  const tok = token();
  ensureRepo(tok, hosted(REPO, 'docker'));
  const reg = registryToken(tok, IMAGE);
  const blobs = SIZES.map((size, i) => ({ digest: dockerPushBlob(reg, IMAGE, size, 100 + i), size }));
  // Registry tokens live 15 minutes; keep runs shorter or re-run setup.
  return { reg, blobs };
}

export default function (data) {
  const b = pick(data.blobs);
  const res = http.get(`${BASE}/v2/${IMAGE}/blobs/${b.digest}`, {
    headers: { Authorization: `Bearer ${data.reg}` }, responseType: 'binary', tags: { name: `blob ${b.size >= 1048576 ? '1 MiB' : 'up to 256 KiB'}` } });
  check(res, { 'status 200': (x) => x.status === 200, 'full body': (x) => x.body && x.body.byteLength === b.size });
}
