/**
 * MSW handlers for access management (users, roles, tokens, sessions, audit),
 * following api/openapi.yaml. State is in memory and reset between tests.
 */
import { http, HttpResponse } from 'msw';
import type { ApiError } from '../api/types';
import { recordMockAudit } from './ops';
import type { AuditEvent, EffectivePermissions, Member, MemberInput, Role, RoleInput, SessionInfo, Token, TokenCreated, TokenInput, TokenRotateInput } from '../pages/access/api';

const api = (path: string) => `*/api/v1${path}`;
const DAY = 86_400_000;

/** The signed-in mock user (data.session). */
export const ME = 'u_01';

const iso = (offsetMs: number) => new Date(Date.now() + offsetMs).toISOString();

function seed() {
  const member = (m: Partial<Member> & Pick<Member, 'id' | 'username'>): Member => ({
    kind: 'user',
    display_name: '',
    email: null,
    avatar_url: null,
    source: 'local',
    roles: [],
    token_count: 0,
    disabled: false,
    locked_until: null,
    last_active_at: null,
    description: '',
    password_set: m.kind !== 'service_account',
    must_change_password: false,
    created_at: iso(-200 * DAY),
    ...m,
  });
  const users: Member[] = [
    member({ id: ME, username: 'd.ismoilov', display_name: 'Dilshod Ismoilov', email: 'd.ismoilov@acme.example', roles: ['Admin'], last_active_at: iso(0) }),
    member({ id: 'u_02', username: 'j.karimov', display_name: 'Jasur Karimov', email: 'jasur@acme.example', source: 'oidc', roles: ['Publisher'], last_active_at: iso(-4 * 60_000) }),
    member({ id: 'u_03', username: 'm.nazarova', display_name: 'Malika Nazarova', email: 'malika@acme.example', source: 'ldap', roles: ['Publisher'], last_active_at: iso(-3_600_000) }),
    member({ id: 'u_04', username: 'gitea-runner', kind: 'service_account', description: 'Gitea Actions runners', roles: ['CI deployer'], last_active_at: iso(-12_000) }),
    member({ id: 'u_05', username: 'argo-cd', kind: 'service_account', description: 'Argo CD image updater', roles: ['Reader'], last_active_at: iso(-17 * 60_000) }),
    member({ id: 'u_06', username: 's.aliyev', display_name: 'Sardor Aliyev', email: 'sardor@acme.example', source: 'ldap', roles: ['Reader'], last_active_at: iso(-DAY) }),
    member({ id: 'u_07', username: 'n.tursunova', display_name: 'Nodira Tursunova', email: 'nodira@acme.example', source: 'oidc', roles: ['Admin'], last_active_at: iso(-2 * DAY) }),
    member({ id: 'u_08', username: 'break-glass-1', display_name: 'break-glass-1', roles: ['Admin'], last_active_at: iso(-94 * DAY), must_change_password: true }),
    member({ id: 'u_09', username: 'b.yusupov', display_name: 'Bekzod Yusupov', email: 'bekzod@acme.example', roles: ['Reader'], locked_until: iso(10 * 60_000) }),
  ];
  const roles: Role[] = [
    { id: 'r_1', name: 'Admin', description: 'Full access to every repository and system setting', builtin: true, privileges: ['repo:*:admin', 'system:*'], group_mappings: [{ provider: 'keycloak', group: 'platform-admins' }], member_count: 0 },
    { id: 'r_2', name: 'Publisher', description: 'Read and publish to every repository', builtin: true, privileges: ['repo:*:read', 'repo:*:write'], group_mappings: [], member_count: 0 },
    { id: 'r_3', name: 'Reader', description: 'Read every repository', builtin: true, privileges: ['repo:*:read'], group_mappings: [], member_count: 0 },
    { id: 'r_4', name: 'CI deployer', description: 'Read every repository, publish and delete in hosted repositories', builtin: true, privileges: ['repo:*:delete', 'repo:*:read', 'repo:*:write'], group_mappings: [], member_count: 0 },
  ];
  const token = (t: Partial<Token> & Pick<Token, 'id' | 'name' | 'owner'>): Token => ({
    prefix: `hgr_${t.id.slice(-4)}`,
    scopes: ['repo:*:read'],
    owner_kind: 'user',
    created_at: iso(-30 * DAY),
    expires_at: iso(60 * DAY),
    last_used_at: null,
    last_used_ip: null,
    allowed_cidrs: [],
    read_only: false,
    ...t,
  });
  const tokens: Token[] = [
    token({ id: 't_0001', name: 'laptop CLI', owner: 'd.ismoilov', scopes: ['repo:*:read', 'repo:npm-*:write'], last_used_at: iso(-3_600_000), last_used_ip: '10.4.2.18', expires_at: iso(9 * DAY) }),
    token({ id: 't_0002', name: 'release script', owner: 'd.ismoilov', scopes: ['repo:docker-prod:write'], expires_at: iso(200 * DAY) }),
    token({ id: 't_0003', name: 'gitea deploy', owner: 'gitea-runner', owner_kind: 'service_account', scopes: ['repo:*:write', 'repo:*:read'], last_used_at: iso(-12_000), last_used_ip: '10.8.0.4', expires_at: iso(28 * DAY) }),
    token({ id: 't_0004', name: 'image updater', owner: 'argo-cd', owner_kind: 'service_account', scopes: ['repo:docker-*:read'], last_used_at: iso(-17 * 60_000), last_used_ip: '10.8.0.9', expires_at: iso(61 * DAY), allowed_cidrs: ['10.8.0.0/16'], read_only: true }),
    token({ id: 't_0005', name: 'old laptop', owner: 'j.karimov', scopes: ['repo:*:read'], expires_at: iso(-3 * DAY) }),
  ];
  const sessions: Record<string, SessionInfo[]> = {
    [ME]: [
      { id: 's_1', created_at: iso(-2 * 3_600_000), last_seen_at: iso(0), expires_at: iso(10 * 3_600_000), ip: '10.4.2.18', user_agent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36', remember: false, current: true },
      { id: 's_2', created_at: iso(-3 * DAY), last_seen_at: iso(-DAY), expires_at: iso(27 * DAY), ip: '192.0.2.44', user_agent: 'Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0', remember: true, current: false },
    ],
    u_02: [{ id: 's_3', created_at: iso(-3_600_000), last_seen_at: iso(-4 * 60_000), expires_at: iso(11 * 3_600_000), ip: '10.4.7.33', user_agent: 'Mozilla/5.0 (Windows NT 10.0) Chrome/131.0', remember: false, current: false }],
  };
  const audit: AuditEvent[] = [];
  return { users, roles, tokens, sessions, audit, nextId: 100, password: 'password1234' };
}

let st = seed();

export function resetAccessState(): void {
  st = seed();
}

/** The signed-in user's password, for PUT /me/password (set by a mocked sign-in). */
export function setMockPassword(password: string): void {
  st.password = password;
}

/** Test helpers. */
export const accessState = {
  get users(): readonly Member[] {
    return st.users;
  },
  get tokens(): readonly Token[] {
    return st.tokens;
  },
  get audit(): readonly AuditEvent[] {
    return st.audit;
  },
};

type Guard = (request: Request, mutating?: boolean) => Response | null;

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });
const notFound = (what: string) => error(404, { code: 'not_found', message: `${what}: not found` });
const invalid = (message: string) => error(400, { code: 'invalid_argument', message });

/** Records into this area's log (for test assertions) and the shared audit mock (GET /audit). */
function record(action: string, target: string, details?: Record<string, unknown>) {
  const e = { at: new Date().toISOString(), actor: 'd.ismoilov', action, target, outcome: 'success' as const, ip: '10.4.2.18', details };
  st.audit.unshift({ id: ++st.nextId, ...e });
  recordMockAudit(e);
}

function withCounts(): Role[] {
  return st.roles.map((r) => ({ ...r, member_count: st.users.filter((u) => u.roles.includes(r.name)).length }));
}

function live(t: Token) {
  return new Date(t.expires_at).getTime() > Date.now();
}

/** Stores a new token and returns it with its one-time secret. */
function mint(name: string, owner: string, ownerKind: 'user' | 'service_account', scopes: string[], expiresAt: string, cidrs: string[], readOnly: boolean): TokenCreated {
  const id = `t_${++st.nextId}`;
  const secret = `hgr_Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4zAb7cDe0f${id.replace('_', '')}`;
  const t: Token = {
    id,
    name,
    owner,
    owner_kind: ownerKind,
    prefix: secret.slice(0, 8),
    scopes: [...new Set(scopes)].sort(),
    expires_at: expiresAt,
    created_at: new Date().toISOString(),
    last_used_at: null,
    last_used_ip: null,
    allowed_cidrs: cidrs,
    read_only: readOnly,
  };
  st.tokens.unshift(t);
  return { ...t, secret };
}

function refreshTokenCounts() {
  for (const u of st.users) u.token_count = st.tokens.filter((t) => t.owner === u.username && live(t)).length;
}

function enabledAdmins(exceptId: string) {
  return st.users.filter((u) => u.id !== exceptId && !u.disabled && u.roles.includes('Admin')).length;
}

const PRIV_RE = /^(repo|system):[^:]+:(read|write|delete|admin|\*)$|^system:\*$/;

export function accessHandlers(guard: Guard, onPasswordChanged: () => Record<string, string> = () => ({})) {
  const findUser = (id: string) => st.users.find((u) => u.id === id);
  return [
    http.get(api('/users'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      refreshTokenCounts();
      const p = new URL(request.url).searchParams;
      const q = (p.get('q') ?? '').toLowerCase();
      const now = Date.now();
      const items = st.users
        .filter((u) => !p.get('kind') || u.kind === p.get('kind'))
        .filter((u) => !p.get('role') || u.roles.includes(p.get('role')!))
        .filter((u) => !q || [u.username, u.display_name, u.email ?? ''].some((v) => v.toLowerCase().includes(q)))
        .filter((u) => {
          const status = p.get('status');
          const locked = !!u.locked_until && new Date(u.locked_until).getTime() > now;
          if (status === 'disabled') return u.disabled;
          if (status === 'locked') return !u.disabled && locked;
          if (status === 'active') return !u.disabled && !locked;
          return true;
        })
        .sort((a, b) => a.username.localeCompare(b.username));
      return HttpResponse.json({ items, next_cursor: null });
    }),

    http.post(api('/users'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const body = (await request.json()) as MemberInput;
      if (!/^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$/.test(body.username)) return invalid(`username "${body.username}" is invalid`);
      if (st.users.some((u) => u.username === body.username)) return error(409, { code: 'conflict', message: `create user "${body.username}": already exists` });
      if (body.kind === 'user' && body.password !== undefined && body.password.length < 12) return invalid('password must be at least 12 characters');
      const m: Member = {
        id: `u_${++st.nextId}`,
        username: body.username,
        kind: body.kind,
        display_name: body.display_name ?? '',
        email: body.email || null,
        avatar_url: null,
        source: 'local',
        roles: body.roles ?? [],
        token_count: 0,
        disabled: !!body.disabled,
        locked_until: null,
        last_active_at: null,
        description: body.description ?? '',
        password_set: !!body.password,
        created_at: new Date().toISOString(),
      };
      st.users.push(m);
      record('user.create', `user:${m.username}`, { kind: m.kind });
      return HttpResponse.json(m, { status: 201 });
    }),

    http.get(api('/users/:id'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      refreshTokenCounts();
      const u = findUser(String(params.id));
      return u ? HttpResponse.json(u) : notFound(`user "${String(params.id)}"`);
    }),

    http.put(api('/users/:id'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const u = findUser(String(params.id));
      if (!u) return notFound('user');
      const body = (await request.json()) as MemberInput;
      if (u.roles.includes('Admin') && body.roles && !body.roles.includes('Admin') && enabledAdmins(u.id) === 0) {
        return invalid('at least one enabled user must keep the Admin role');
      }
      Object.assign(u, {
        username: body.username,
        display_name: body.display_name ?? u.display_name,
        email: body.email === undefined ? u.email : body.email || null,
        description: body.description ?? u.description,
        roles: body.roles ?? u.roles,
      });
      record('user.update', `user:${u.username}`);
      return HttpResponse.json(u);
    }),

    http.delete(api('/users/:id'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const u = findUser(String(params.id));
      if (!u) return notFound('user');
      if (u.id === ME) return invalid('you cannot delete your own account');
      st.users = st.users.filter((x) => x.id !== u.id);
      st.tokens = st.tokens.filter((t) => t.owner !== u.username);
      record('user.delete', `user:${u.username}`);
      return new HttpResponse(null, { status: 204 });
    }),

    http.post(api('/users/:id/:action'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const u = findUser(String(params.id));
      if (!u) return notFound('user');
      switch (params.action) {
        case 'disable':
          if (u.id === ME) return invalid('you cannot disable your own account');
          if (u.roles.includes('Admin') && enabledAdmins(u.id) === 0) return invalid('at least one enabled user must keep the Admin role');
          u.disabled = true;
          delete st.sessions[u.id];
          break;
        case 'enable':
          u.disabled = false;
          break;
        case 'unlock':
          u.locked_until = null;
          break;
        case 'require-password-change':
          if (u.id === ME) return invalid('change your own password on your account page instead');
          if (u.kind !== 'user' || u.source !== 'local' || !u.password_set) return invalid(`user "${u.username}" has no local password`);
          if (u.must_change_password) return HttpResponse.json(u);
          u.must_change_password = true;
          record('user.password.require_change', `user:${u.username}`);
          return HttpResponse.json(u);
        default:
          return notFound('route');
      }
      record(`user.${String(params.action)}`, `user:${u.username}`);
      return HttpResponse.json(u);
    }),

    http.put(api('/users/:id/password'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const u = findUser(String(params.id));
      if (!u) return notFound('user');
      const { password } = (await request.json()) as { password: string };
      if (u.id === ME) return invalid('change your own password with PUT /me/password');
      if (!password || password.length < 12) return invalid('password must be at least 12 characters');
      u.locked_until = null;
      delete st.sessions[u.id];
      record('user.password', `user:${u.username}`, { reset_by_admin: true });
      return new HttpResponse(null, { status: 204 });
    }),

    http.get(api('/users/:id/permissions'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      const u = findUser(String(params.id));
      if (!u) return notFound('user');
      return HttpResponse.json(permissionsOf(u));
    }),

    http.get(api('/users/:id/sessions'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      return HttpResponse.json({ items: (st.sessions[String(params.id)] ?? []).map((s) => ({ ...s, current: String(params.id) === ME && s.current })) });
    }),
    http.delete(api('/users/:id/sessions'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const id = String(params.id);
      const before = st.sessions[id] ?? [];
      st.sessions[id] = id === ME ? before.filter((s) => s.current) : [];
      return HttpResponse.json({ ended: before.length - st.sessions[id]!.length });
    }),
    http.delete(api('/users/:id/sessions/:sid'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const list = st.sessions[String(params.id)] ?? [];
      if (!list.some((s) => s.id === params.sid)) return notFound('session');
      st.sessions[String(params.id)] = list.filter((s) => s.id !== params.sid);
      return new HttpResponse(null, { status: 204 });
    }),

    /* ---- me ---- */
    http.put(api('/me/profile'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const me = findUser(ME)!;
      const body = (await request.json()) as { display_name?: string; email?: string };
      if (me.source !== 'local') return invalid(`your profile is managed by ${me.source}`);
      Object.assign(me, { display_name: body.display_name ?? me.display_name, email: body.email ?? me.email });
      return HttpResponse.json(me);
    }),
    http.put(api('/me/password'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const body = (await request.json()) as { current_password: string; new_password: string };
      if (body.current_password !== st.password) return invalid('the current password is incorrect');
      if (body.new_password.length < 12) return invalid('password must be at least 12 characters');
      if (body.new_password === body.current_password) return invalid('the new password must differ from the current one');
      st.password = body.new_password;
      st.sessions[ME] = (st.sessions[ME] ?? []).filter((s) => s.current);
      record('user.password', 'user:d.ismoilov', { self: true });
      return new HttpResponse(null, { status: 204, headers: onPasswordChanged() });
    }),
    http.get(api('/me/sessions'), ({ request }) => guard(request) ?? HttpResponse.json({ items: st.sessions[ME] ?? [] })),
    http.delete(api('/me/sessions'), ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const before = st.sessions[ME] ?? [];
      st.sessions[ME] = before.filter((s) => s.current);
      return HttpResponse.json({ ended: before.length - st.sessions[ME].length });
    }),
    http.delete(api('/me/sessions/:sid'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const list = st.sessions[ME] ?? [];
      if (!list.some((s) => s.id === params.sid)) return notFound('session');
      st.sessions[ME] = list.filter((s) => s.id !== params.sid);
      return new HttpResponse(null, { status: 204 });
    }),

    /* ---- roles ---- */
    http.get(api('/roles'), ({ request }) => guard(request) ?? HttpResponse.json({ items: withCounts() })),
    http.post(api('/roles'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const body = (await request.json()) as RoleInput;
      const bad = body.privileges.find((p) => !PRIV_RE.test(p));
      if (bad) return invalid(`privilege "${bad}" is invalid`);
      if (st.roles.some((r) => r.name === body.name)) return error(409, { code: 'conflict', message: `create role "${body.name}": already exists` });
      const role: Role = { id: `r_${++st.nextId}`, description: '', group_mappings: [], ...body, builtin: false, member_count: 0 };
      st.roles.push(role);
      record('role.create', `role:${role.name}`);
      return HttpResponse.json(role, { status: 201 });
    }),
    http.put(api('/roles/:name'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const role = st.roles.find((r) => r.name === params.name);
      if (!role) return notFound('role');
      if (role.builtin) return error(409, { code: 'conflict', message: 'built-in roles cannot be changed' });
      const body = (await request.json()) as RoleInput;
      for (const u of st.users) u.roles = u.roles.map((r) => (r === role.name ? body.name : r));
      Object.assign(role, body);
      record('role.update', `role:${role.name}`);
      return HttpResponse.json(role);
    }),
    http.delete(api('/roles/:name'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const role = st.roles.find((r) => r.name === params.name);
      if (!role) return notFound('role');
      if (role.builtin) return error(409, { code: 'conflict', message: 'built-in roles cannot be changed' });
      st.roles = st.roles.filter((r) => r !== role);
      for (const u of st.users) u.roles = u.roles.filter((r) => r !== role.name);
      record('role.delete', `role:${role.name}`);
      return new HttpResponse(null, { status: 204 });
    }),
    http.post(api('/roles/:name/members'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const { user_id } = (await request.json()) as { user_id: string };
      const u = findUser(user_id);
      if (!u || !st.roles.some((r) => r.name === params.name)) return notFound('role or user');
      if (!u.roles.includes(String(params.name))) u.roles = [...u.roles, String(params.name)].sort();
      record('role.bind', `role:${String(params.name)}`, { user: u.username });
      return new HttpResponse(null, { status: 204 });
    }),
    http.delete(api('/roles/:name/members/:id'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const u = findUser(String(params.id));
      if (!u) return notFound('user');
      if (params.name === 'Admin' && enabledAdmins(u.id) === 0) return invalid('at least one enabled user must keep the Admin role');
      u.roles = u.roles.filter((r) => r !== params.name);
      record('role.unbind', `role:${String(params.name)}`, { user: u.username });
      return new HttpResponse(null, { status: 204 });
    }),

    /* ---- tokens ---- */
    http.get(api('/tokens'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = new URL(request.url).searchParams;
      const owner = p.get('owner') ?? (p.get('all') === 'true' ? '' : 'd.ismoilov');
      const status = p.get('status');
      const q = (p.get('q') ?? '').toLowerCase();
      const items = st.tokens
        .filter((t) => !owner || t.owner === owner)
        .filter((t) => !p.get('owner_kind') || t.owner_kind === p.get('owner_kind'))
        .filter((t) => !status || (status === 'active' ? live(t) : !live(t)))
        .filter((t) => !q || t.name.toLowerCase().includes(q))
        .sort((a, b) => b.created_at.localeCompare(a.created_at));
      return HttpResponse.json({ items, next_cursor: null });
    }),
    http.post(api('/tokens'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const body = (await request.json()) as TokenInput;
      if (!body.name?.trim()) return invalid('token name must be 1 to 100 characters');
      if (!body.scopes?.length) return invalid('token needs at least one scope');
      const bad = body.scopes.find((p) => !PRIV_RE.test(p));
      if (bad) return invalid(`privilege "${bad}" is invalid`);
      if (!body.expires_at || new Date(body.expires_at).getTime() <= Date.now()) return invalid('token expires_at must be in the future');
      if (new Date(body.expires_at).getTime() > Date.now() + 366 * DAY) return invalid('token expires_at must be within 8784h0m0s');
      const owner = body.owner || 'd.ismoilov';
      const ownerUser = st.users.find((u) => u.username === owner);
      if (body.owner && ownerUser?.kind !== 'service_account') return invalid(`owner "${body.owner}" must be a service account`);
      const t = mint(body.name.trim(), owner, ownerUser?.kind ?? 'user', body.scopes, body.expires_at, body.allowed_cidrs ?? [], body.read_only ?? false);
      record('token.create', `token:${t.id}`, { name: t.name, owner });
      return HttpResponse.json(t, { status: 201 });
    }),
    http.post(api('/tokens/:id/rotate'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const old = st.tokens.find((x) => x.id === params.id);
      if (!old) return notFound('token');
      const text = await request.text();
      const body = (text ? JSON.parse(text) : {}) as TokenRotateInput;
      const after = body.revoke_old_after;
      const graceMs = after == null ? null : (Number(/^(\d+)h$/.exec(after)?.[1] ?? (after === '0s' ? 0 : NaN)) * 3_600_000);
      if (graceMs !== null && Number.isNaN(graceMs)) return invalid('revoke_old_after must be a duration between 0s and 720h0m0s');
      const lifetime = Math.min(new Date(old.expires_at).getTime() - new Date(old.created_at).getTime(), 366 * DAY);
      const t = mint(old.name, old.owner, old.owner_kind ?? 'user', old.scopes, new Date(Date.now() + lifetime).toISOString(), old.allowed_cidrs, old.read_only);
      let revoked = false;
      if (graceMs === 0) {
        st.tokens = st.tokens.filter((x) => x !== old);
        revoked = true;
        old.expires_at = new Date().toISOString();
      } else if (graceMs !== null) {
        const end = Date.now() + graceMs;
        if (end < new Date(old.expires_at).getTime()) old.expires_at = new Date(end).toISOString();
      }
      record('token.rotate', `token:${t.id}`, { name: t.name, owner: t.owner, replaces: old.id });
      return HttpResponse.json({ ...t, replaced: { id: old.id, revoked, expires_at: old.expires_at } }, { status: 201 });
    }),
    http.delete(api('/tokens/:id'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      const t = st.tokens.find((x) => x.id === params.id);
      if (!t) return notFound('token');
      st.tokens = st.tokens.filter((x) => x !== t);
      record('token.revoke', `token:${t.id}`, { name: t.name, owner: t.owner });
      return new HttpResponse(null, { status: 204 });
    }),
  ];
}

const ACTIONS = ['read', 'write', 'delete', 'admin'] as const;

function permissionsOf(u: Member): EffectivePermissions {
  const roles = st.roles.filter((r) => u.roles.includes(r.name));
  const match = (pattern: string, name: string) => pattern === '*' || pattern === name || (pattern.endsWith('*') && name.startsWith(pattern.slice(0, -1)));
  const evaluate = (domain: 'repo' | 'system', name: string, anon: boolean) => {
    const grants: EffectivePermissions['repositories'][number]['grants'] = [];
    const actions: (typeof ACTIONS)[number][] = [];
    for (const a of ACTIONS) {
      let ok = false;
      for (const r of roles)
        for (const p of r.privileges) {
          const [d, pattern, act] = p === 'system:*' ? ['system', '*', '*'] : p.split(':');
          if (d === domain && match(pattern!, name) && (act === a || act === 'admin' || act === '*')) {
            ok = true;
            grants.push({ action: a, source: 'role', role: r.name, privilege: p });
          }
        }
      if (a === 'read' && anon) {
        ok = true;
        grants.push({ action: a, source: 'anonymous' });
      }
      if (ok) actions.push(a);
    }
    return { resource: `${domain}:${name}`, name, actions, grants };
  };
  const repos = [
    { name: 'docker-prod', format: 'docker', type: 'hosted', anon: false },
    { name: 'npm-internal', format: 'npm', type: 'hosted', anon: false },
    { name: 'npm-public', format: 'npm', type: 'group', anon: true },
  ];
  return {
    user_id: u.id,
    username: u.username,
    kind: u.kind,
    disabled: !!u.disabled,
    locked: !!u.locked_until && new Date(u.locked_until).getTime() > Date.now(),
    roles: roles.map((r) => ({ name: r.name, builtin: r.builtin, privileges: r.privileges })),
    repositories: repos
      .map((r) => ({ ...evaluate('repo', r.name, r.anon), format: r.format, type: r.type, anonymous_read: r.anon }))
      .filter((r) => r.actions.length > 0),
    system: ['users', 'tokens', 'audit', 'repos'].map((a) => evaluate('system', a, false)).filter((r) => r.actions.length > 0),
    notes: u.source !== 'local' ? [`Roles granted through ${u.source} group mappings are applied at sign-in and are not included.`] : [],
  };
}

