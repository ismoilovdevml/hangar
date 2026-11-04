// Raw downloads of small, medium and large files from a hosted repository.
import http from 'k6/http';
import { check } from 'k6';
import { BASE, options as base, token, bearer, ensureRepo, hosted, put, bytes, pick } from './lib.js';

export const options = base({}, ['raw 1-4 KiB', 'raw 64 KiB', 'raw 1 MiB']);

const REPO = 'load-raw';
// Request mix: mostly small files, as CI artifact and script downloads are.
const FILES = [
  { path: 'small/a.bin', size: 1024, weight: 6 },
  { path: 'small/b.bin', size: 4096, weight: 6 },
  { path: 'medium/c.bin', size: 65536, weight: 3 },
  { path: 'large/d.bin', size: 1048576, weight: 1 },
];

export function setup() {
  const tok = token();
  ensureRepo(tok, hosted(REPO, 'raw'));
  const paths = [];
  FILES.forEach((f, i) => {
    put(tok, `/repository/${REPO}/${f.path}`, bytes(f.size, i + 1), 'raw upload');
    for (let w = 0; w < f.weight; w++) {
      paths.push({ path: f.path, size: f.size });
    }
  });
  return { tok, paths };
}

export default function (data) {
  const f = pick(data.paths);
  const res = http.get(`${BASE}/repository/${REPO}/${f.path}`, {
    headers: bearer(data.tok), responseType: 'binary', tags: { name: `raw ${f.size >= 1048576 ? '1 MiB' : f.size >= 65536 ? '64 KiB' : '1-4 KiB'}` } });
  check(res, { 'status 200': (x) => x.status === 200, 'full body': (x) => x.body && x.body.byteLength === f.size });
}
