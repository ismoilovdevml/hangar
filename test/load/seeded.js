// Database-bound reads against repositories filled by cmd/hangar-seed
// (default: 12 repositories, ~8 300 components and ~83 000 assets each):
// generated Maven metadata, raw directory listings and NuGet search. Seeded
// assets have no bytes, so only metadata endpoints are exercised.
//
//   go run ./cmd/hangar-seed --database-url "$HANGAR_DATABASE_URL"
import http from 'k6/http';
import { check } from 'k6';
import { BASE, options as base, token, bearer } from './lib.js';

export const options = base({}, ['seeded maven-metadata.xml', 'seeded raw listing', 'seeded nuget search']);

const PREFIX = __ENV.HANGAR_SEED_PREFIX || 'seed';
const PACKAGES = 833; // packages per seeded repository (components / repositories / versions)

export function setup() {
  return { tok: token() };
}

export default function (data) {
  const auth = bearer(data.tok);
  const pkg = Math.floor(Math.random() * PACKAGES);
  const r = Math.random();
  let res;
  if (r < 0.4) {
    res = http.get(`${BASE}/repository/${PREFIX}-maven-1/com/seed/g${pkg % 100}/pkg-${pkg}/maven-metadata.xml`,
      { headers: auth, tags: { name: 'seeded maven-metadata.xml' } });
  } else if (r < 0.8) {
    res = http.get(`${BASE}/repository/${PREFIX}-raw-2/pkg-${pkg}/`,
      { headers: Object.assign({ Accept: 'application/json' }, auth), tags: { name: 'seeded raw listing' } });
  } else {
    res = http.get(`${BASE}/repository/${PREFIX}-nuget-4/query?q=pkg-${pkg}&take=20`,
      { headers: auth, tags: { name: 'seeded nuget search' } });
  }
  check(res, { 'status 200': (x) => x.status === 200 });
}
