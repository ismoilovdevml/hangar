/**
 * Mock repository signing keys (GET/PUT /repositories/{name}/signing-key,
 * POST .../signing-key:rotate), following internal/signing: apt and rpm
 * hosted and group repositories only, the shared key needs
 * system:repos:admin, and no secret key configured answers 501.
 */
import { http, HttpResponse } from 'msw';
import type { ApiError, Repository, SigningKey } from '../api/types';
import { allows } from '../lib/can';
import { session } from './data';

const api = (path: string) => `*/api/v1${path}`;
type Guard = (request: Request, mutating?: boolean) => Response | null;

function initialState() {
  return { keys: new Map<string, SigningKey>(), generation: 0, secretKey: true };
}
let state = initialState();

export function resetSigningState(): void {
  state = initialState();
}

/** Test helpers: switch the server's secret key off, inspect stored keys. */
export const signingState = {
  set secretKey(v: boolean) {
    state.secretKey = v;
  },
  key(name: string): SigningKey | undefined {
    return state.keys.get(name);
  },
};

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });

function fingerprint(seed: string): string {
  let h = 2166136261;
  let out = '';
  while (out.length < 40) {
    for (const c of `${seed}:${out.length}`) h = Math.imul(h ^ c.charCodeAt(0), 16777619);
    out += (h >>> 0).toString(16).toUpperCase().padStart(8, '0');
  }
  return out.slice(0, 40);
}

const shared = (r: Repository) => r.config.format?.signing_key === 'shared';
const keyName = (r: Repository) => (shared(r) ? 'shared' : `repo:${r.id}`);

function newKey(r: Repository, algorithm: string, created: string): SigningKey {
  const fp = fingerprint(`${keyName(r)}#${state.generation++}`);
  const uid = shared(r) ? 'Hangar repository signing key <signing@pkg.acme.io>' : `Hangar ${r.name} signing key <${r.name}@pkg.acme.io>`;
  return {
    repository: r.name,
    key: shared(r) ? 'shared' : 'repository',
    algorithm,
    fingerprint: fp,
    key_id: fp.slice(-16),
    user_id: uid,
    created_at: created,
    public_key: `-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQINBG${fp.slice(0, 24)}\n-----END PGP PUBLIC KEY BLOCK-----\n`,
  };
}

const view = (r: Repository, k: SigningKey): SigningKey => ({ ...k, repository: r.name, key: shared(r) ? 'shared' : 'repository' });

export function signingHandlers(guard: Guard, repositories: () => readonly Repository[]) {
  /** The repository whose key the request is about, or the error response. */
  const resolve = (request: Request, name: string, admin: boolean): Repository | Response => {
    const denied = guard(request, admin);
    if (denied) return denied;
    const r = repositories().find((x) => x.name === name);
    if (!r) return error(404, { code: 'not_found', message: `repository "${name}" not found` });
    if (r.format !== 'apt' && r.format !== 'rpm') return error(404, { code: 'not_found', message: `${r.format} repositories have no signing key` });
    if (r.type === 'proxy') {
      return error(404, { code: 'not_found', message: `proxy repository "${name}" serves its remote's signatures and has no signing key` });
    }
    if (admin) {
      const ok = shared(r) ? allows(session.privileges, 'system', 'repos', 'admin') : allows(session.privileges, 'repo', name, 'admin') || allows(session.privileges, 'system', 'repos', 'admin');
      if (!ok) return error(403, { code: 'forbidden', message: 'permission denied' });
    }
    if (!state.secretKey) {
      return error(501, { code: 'not_implemented', message: 'repository signing keys: storing secrets requires security.secret_key (HANGAR_SECRET_KEY)' });
    }
    return r;
  };
  const store = (r: Repository, k: SigningKey) => {
    state.keys.set(keyName(r), k);
    return HttpResponse.json(view(r, k));
  };

  return [
    http.get(api('/repositories/:name/signing-key'), ({ request, params }) => {
      const r = resolve(request, String(params.name), false);
      if (r instanceof Response) return r;
      const k = state.keys.get(keyName(r)) ?? newKey(r, 'rsa4096', r.created_at ?? '2026-06-01T09:00:00Z');
      return store(r, k);
    }),
    http.post(api('/repositories/:name/signing-key\\:rotate'), async ({ request, params }) => {
      const r = resolve(request, String(params.name), true);
      if (r instanceof Response) return r;
      const text = await request.text();
      const algorithm = (text ? (JSON.parse(text) as { algorithm?: string }).algorithm : undefined) || 'rsa4096';
      if (algorithm !== 'rsa4096' && algorithm !== 'ed25519') return error(400, { code: 'invalid_argument', message: `unknown algorithm "${algorithm}"` });
      return store(r, newKey(r, algorithm, new Date().toISOString()));
    }),
    http.put(api('/repositories/:name/signing-key'), async ({ request, params }) => {
      const r = resolve(request, String(params.name), true);
      if (r instanceof Response) return r;
      const body = (await request.json()) as { private_key?: string; passphrase?: string };
      if (!body.private_key) return error(400, { code: 'invalid_argument', message: 'private_key is required' });
      if (!body.private_key.includes('-----BEGIN PGP PRIVATE KEY BLOCK-----')) {
        return error(400, { code: 'invalid_argument', message: 'parse private key: openpgp: invalid argument: no armored data found' });
      }
      if (body.passphrase === 'wrong') return error(400, { code: 'invalid_argument', message: 'decrypt private key: openpgp: invalid data: private key checksum failure' });
      return store(r, newKey(r, 'ed25519', '2025-02-14T10:00:00Z'));
    }),
  ];
}
