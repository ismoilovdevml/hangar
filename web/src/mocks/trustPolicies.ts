/**
 * MSW handlers for CI OIDC trust policies (api/openapi.yaml /trust-policies),
 * with the server's checks and its error wording (internal/auth/federation):
 * the message starts with the field it is about, and GitHub and gitlab.com
 * policies must pin the owner. State is in memory and reset between tests.
 */
import { http, HttpResponse } from 'msw';
import type { ApiError } from '../api/types';
import { allows, parsePrivilege } from '../lib/privileges';
import type { TrustPolicy, TrustPolicyInput } from '../pages/settings/trustPoliciesApi';
import { accessState } from './access';
import { recordMockAudit } from './ops';

const api = (path: string) => `*/api/v1${path}`;
const iso = (offsetMs = 0) => new Date(Date.now() + offsetMs).toISOString();
const DAY = 86_400_000;
const DEFAULT_AUDIENCE = 'https://pkg.acme.io';
const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const CLAIM_RE = /^[A-Za-z0-9_:-]+(\.[A-Za-z0-9_:-]+)*$/;
const OWNER_CLAIMS: Record<string, string[]> = {
  'https://token.actions.githubusercontent.com': ['repository', 'repository_owner', 'repository_id', 'repository_owner_id', 'sub'],
  'https://gitlab.com': ['project_path', 'namespace_path', 'project_id', 'namespace_id', 'sub'],
};

function seed(): TrustPolicy[] {
  return [
    {
      id: 'tp_01',
      name: 'acme-app-github',
      description: 'Releases of acme/app from the production environment',
      enabled: true,
      issuer: 'https://token.actions.githubusercontent.com',
      audience: DEFAULT_AUDIENCE,
      conditions: [
        { claim: 'repository', match: 'exact', values: ['acme/app'] },
        { claim: 'ref', match: 'glob', values: ['refs/heads/main', 'refs/tags/v*'] },
        { claim: 'environment', match: 'exact', values: ['production'] },
      ],
      service_account: 'gitea-runner',
      scopes: ['repo:npm-internal:write', 'repo:npm-internal:read'],
      max_ttl_seconds: 900,
      created_at: iso(-21 * DAY),
      updated_at: iso(-4 * DAY),
    },
    {
      id: 'tp_02',
      name: 'billing-gitlab',
      description: '',
      enabled: true,
      issuer: 'https://gitlab.com',
      audience: DEFAULT_AUDIENCE,
      conditions: [
        { claim: 'project_path', match: 'exact', values: ['acme/billing'] },
        { claim: 'ref_protected', match: 'exact', values: ['true'] },
      ],
      service_account: 'gitea-runner',
      scopes: ['repo:maven-releases:write'],
      max_ttl_seconds: 1800,
      created_at: iso(-9 * DAY),
      updated_at: iso(-9 * DAY),
    },
  ];
}

let state = { policies: seed(), nextId: 3 };

export function resetTrustPoliciesState(): void {
  state = { policies: seed(), nextId: 3 };
}

/** Test helpers. */
export const trustPoliciesState = {
  get policies(): readonly TrustPolicy[] {
    return state.policies;
  },
};

type Guard = (request: Request, mutating?: boolean) => Response | null;
const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });
const invalid = (message: string) => error(400, { code: 'invalid_argument', message });
const forbidden = (message = 'forbidden') => error(403, { code: 'forbidden', message });

/** A value that fixes an owner: exact, or a glob whose first wildcard follows a "/". */
function anchored(match: string, v: string): boolean {
  if (match !== 'glob') return true;
  const i = v.search(/[*?]/);
  return i < 0 || (i > 0 && v.slice(0, i).includes('/'));
}

/** The server's first complaint about a policy, worded like internal/auth/federation; null when it is fine. */
function problem(b: TrustPolicyInput): string | null {
  const ttl = b.max_ttl_seconds ?? 900;
  if (!NAME_RE.test(b.name ?? '')) return `name "${b.name}": 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or digit`;
  if ((b.description ?? '').length > 500) return 'description: at most 500 characters';
  if (ttl < 60 || ttl > 3600) return `max_ttl_seconds ${ttl}: between 60 and 3600`;
  if (!b.scopes?.length) return 'scopes: at least one is required (for example repo:npm-hosted:write)';
  if (!/^https:\/\/[^/]+/.test(b.issuer ?? '')) return `issuer "${b.issuer}" must be an https URL`;
  const bad = b.scopes.find((s) => parsePrivilege(s) === null);
  if (bad) return `privilege ${bad}: unknown domain`;
  const conds = b.conditions ?? [];
  if (conds.length === 0) return 'conditions: at least one claim condition is required (for example repository or project_path)';
  for (const [i, c] of conds.entries()) {
    const match = c.match ?? 'exact';
    if (!CLAIM_RE.test(c.claim ?? '')) return `conditions: condition ${i + 1}: claim "${c.claim}": want a claim name such as repository or a dotted path`;
    if (!c.values?.length) return `conditions: condition ${i + 1} (${c.claim}): at least one value is required`;
    const any = c.values.find((v) => match === 'glob' && v.replace(/[*?]/g, '') === '');
    if (any !== undefined) return `conditions: condition ${i + 1} (${c.claim}): "${any}" matches anything; leave the condition out instead`;
  }
  const owners = OWNER_CLAIMS[b.issuer];
  if (owners && !conds.some((c) => owners.includes(c.claim) && c.values.every((v) => anchored(c.match ?? 'exact', v)))) {
    return `conditions: this issuer signs jobs of every account on the platform: add a condition on one of ${owners.join(', ')} with exact values or globs that fix the owner (acme/*, not acme*)`;
  }
  return null;
}

function serviceAccountProblem(name: string): string | null {
  if (!name) return 'service_account is required';
  const u = accessState.users.find((x) => x.username === name);
  if (!u) return `service_account "${name}": no such service account`;
  if (u.kind !== 'service_account') return `service_account "${name}" is not a service account`;
  if (u.disabled) return `service account "${name}" is disabled`;
  return null;
}

function record(action: string, p: TrustPolicy) {
  recordMockAudit({ at: iso(), actor: 'd.ismoilov', action, target: `trust_policy:${p.name}`, outcome: 'success', ip: '10.4.2.18', details: { issuer: p.issuer, scopes: p.scopes } });
}

/** Handlers; `privileges` are those of the signed-in mock user (the caller must cover a policy's scopes). */
export function trustPoliciesHandlers(guard: Guard, privileges: () => readonly string[]) {
  const can = (action: 'read' | 'admin') => allows(privileges(), 'system', 'settings', action);
  /** Like auth.RequireCoversPrivileges: every scope must be something the caller holds. */
  const uncovered = (scopes: readonly string[]) =>
    scopes.find((s) => {
      const p = parsePrivilege(s);
      if (!p) return false;
      if (p.domain === 'system') return !allows(privileges(), 'system', p.pattern, p.action === '*' ? 'admin' : p.action);
      return !allows(privileges(), 'repo', p.pattern, p.action === '*' ? 'admin' : p.action);
    });
  const build = (b: TrustPolicyInput, base: Pick<TrustPolicy, 'id' | 'created_at'>): TrustPolicy => ({
    ...base,
    name: b.name,
    description: (b.description ?? '').trim(),
    enabled: b.enabled ?? true,
    issuer: b.issuer,
    audience: b.audience || DEFAULT_AUDIENCE,
    conditions: b.conditions.map((c) => ({ claim: c.claim, match: c.match ?? 'exact', values: [...new Set(c.values)] })),
    service_account: b.service_account,
    scopes: [...b.scopes],
    max_ttl_seconds: b.max_ttl_seconds ?? 900,
    updated_at: iso(),
  });

  return [
    http.get(api('/trust-policies'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      if (!can('read')) return forbidden();
      return HttpResponse.json({ items: [...state.policies].sort((a, b) => a.name.localeCompare(b.name)) });
    }),

    http.post(api('/trust-policies'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!can('admin')) return forbidden();
      const b = (await request.json()) as TrustPolicyInput;
      const p = problem(b);
      if (p) return invalid(p);
      const missing = uncovered(b.scopes);
      if (missing) return forbidden(`trust policy ${b.name}: you do not hold ${missing}`);
      const sa = serviceAccountProblem(b.service_account);
      if (sa) return invalid(sa);
      if (state.policies.some((x) => x.name === b.name)) return error(409, { code: 'already_exists', message: `trust policy "${b.name}": already exists` });
      const created = build(b, { id: `tp_${String(state.nextId++).padStart(2, '0')}`, created_at: iso() });
      state.policies.push(created);
      record('trust_policy.create', created);
      return HttpResponse.json(created, { status: 201 });
    }),

    http.get(api('/trust-policies/:name'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      if (!can('read')) return forbidden();
      const p = state.policies.find((x) => x.name === params.name);
      return p ? HttpResponse.json(p) : error(404, { code: 'not_found', message: `trust policy "${String(params.name)}": not found` });
    }),

    http.put(api('/trust-policies/:name'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!can('admin')) return forbidden();
      const i = state.policies.findIndex((x) => x.name === params.name);
      const old = state.policies[i];
      if (!old) return error(404, { code: 'not_found', message: `trust policy "${String(params.name)}": not found` });
      const b = (await request.json()) as TrustPolicyInput;
      const body = { ...b, name: b.name || old.name };
      const p = problem(body);
      if (p) return invalid(p);
      if (body.name !== old.name) return invalid('name: a trust policy cannot be renamed');
      const missing = uncovered([...old.scopes, ...body.scopes]);
      if (missing) return forbidden(`trust policy ${old.name}: you do not hold ${missing}`);
      const sa = serviceAccountProblem(body.service_account);
      if (sa) return invalid(sa);
      const next = build(body, { id: old.id, created_at: old.created_at });
      state.policies[i] = next;
      record('trust_policy.update', next);
      return HttpResponse.json(next);
    }),

    http.delete(api('/trust-policies/:name'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!can('admin')) return forbidden();
      const p = state.policies.find((x) => x.name === params.name);
      if (!p) return error(404, { code: 'not_found', message: `trust policy "${String(params.name)}": not found` });
      const missing = uncovered(p.scopes);
      if (missing) return forbidden(`trust policy ${p.name}: you do not hold ${missing}`);
      state.policies = state.policies.filter((x) => x !== p);
      record('trust_policy.delete', p);
      return new HttpResponse(null, { status: 204 });
    }),
  ];
}
