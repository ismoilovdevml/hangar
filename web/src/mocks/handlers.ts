/**
 * MSW handlers for unit tests and offline UI work (VITE_API_MOCKS=true).
 * They follow api/openapi.yaml, including the cookie-session CSRF rule:
 * mutating requests must echo the token in X-CSRF-Token.
 */
import { delay, http, HttpResponse } from 'msw';
import type { ApiError, LoginRequest, OverviewRange, Repository, RepositoryInput, RepositoryType } from '../api/types';
import { accessHandlers, resetAccessState, setMockPassword } from './access';
import { catalogHandlers, resetCatalogState } from './catalog';
import { findingsHandlers, resetFindingsState } from './findings';
import * as data from './data';
import { onMockLiveEvent, opsHandlers, resetOpsMockState } from './ops';
import { mockSystemStatus, resetSystemMockState, systemHandlers } from './system';
import { resetSettingsState, settingsHandlers } from './settings';
import { routingHandlers } from './routing';
import { resetSigningState, signingHandlers } from './signing';
import { openAPIHandlers } from './openapi';
import { allows } from '../lib/can';

const api = (path: string) => `*/api/v1${path}`;

const CSRF_HEADER = 'X-CSRF-Token';
const MAX_FAILED_LOGINS = 5;
const LOCK_SECONDS = 900;

function initialState() {
  return {
    signedIn: true,
    /** Signed in as "admin" with a password that must be replaced first. */
    passwordChangeRequired: false,
    username: data.session.user.username,
    failedLogins: 0,
    csrf: 'mock-csrf-0',
    sessions: 0,
    repositories: data.repositories(),
  };
}

let state = initialState();

export function resetMockState(): void {
  state = initialState();
  // Sign-ins and password changes set the CSRF cookie; start the next test without it.
  if (typeof document !== 'undefined') document.cookie = 'hangar_csrf=; Path=/; Max-Age=0';
  resetOpsMockState();
  resetAccessState();
  resetCatalogState();
  resetSystemMockState();
  resetSettingsState();
  resetFindingsState();
  resetSigningState();
}

/** Test helpers: inspect or change the mock server's state. */
export const mockState = {
  get signedIn() {
    return state.signedIn;
  },
  set signedIn(v: boolean) {
    state.signedIn = v;
  },
  get csrf() {
    return state.csrf;
  },
  get passwordChangeRequired() {
    return state.passwordChangeRequired;
  },
  set passwordChangeRequired(v: boolean) {
    state.passwordChangeRequired = v;
  },
  get repositories(): readonly Repository[] {
    return state.repositories;
  },
};

const error = (status: number, body: ApiError, headers?: Record<string, string>) =>
  HttpResponse.json(body, { status, headers });

const unauthenticated = () => error(401, { code: 'unauthenticated', message: 'not signed in' });

/** Routes a user who must change their password may still call. */
const PASSWORD_CHANGE_ROUTES = new Set(['GET /auth/session', 'PUT /me/password', 'POST /auth/logout']);

/** Session check plus the CSRF rule the real middleware applies to mutating requests. */
function guard(request: Request, mutating = false): Response | null {
  if (!state.signedIn) return unauthenticated();
  if (mutating && request.headers.get(CSRF_HEADER) !== state.csrf) {
    return error(403, { code: 'csrf_failed', message: `missing or invalid ${CSRF_HEADER} header` });
  }
  if (state.passwordChangeRequired) {
    const path = new URL(request.url).pathname.replace(/^\/api\/v1/, '');
    if (!PASSWORD_CHANGE_ROUTES.has(`${request.method} ${path}`)) {
      return error(403, { code: 'password_change_required', message: 'set a new password before doing anything else (PUT /api/v1/me/password)' });
    }
  }
  return null;
}

/**
 * GET /auth/session for the mock user; "admin" is the generated first
 * administrator and "reader" holds only repo:*:read (role-aware UI checks).
 */
function sessionBody() {
  const base =
    state.username === 'admin'
      ? { ...data.session, user: { ...data.session.user, username: 'admin', display_name: 'admin', email: '' } }
      : state.username === 'reader'
        ? {
            ...data.session,
            user: { ...data.session.user, username: 'reader', display_name: 'Read Only', email: 'reader@acme.example', roles: ['Reader'] },
            privileges: ['repo:*:read'],
            token_warnings: [],
          }
        : data.session;
  return state.passwordChangeRequired ? { ...base, privileges: [], token_warnings: [], password_change_required: true } : base;
}

/** PUT /me/password succeeded: the session is re-issued without limits. */
function passwordChanged() {
  state.passwordChangeRequired = false;
  state.sessions++;
  state.csrf = `mock-csrf-${state.sessions}`;
  return { [CSRF_HEADER]: state.csrf, 'Set-Cookie': `hangar_csrf=${state.csrf}; Path=/; SameSite=Lax` };
}

const RANGES: OverviewRange[] = ['1h', 'today', '7d', '30d'];

/** Interval between mocked live "component.published" events. */
export const LIVE_INTERVAL_MS = 25_000;

function eventStream(): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  let timer: ReturnType<typeof setInterval> | undefined;
  let unsubscribe: (() => void) | undefined;
  let n = 0;
  return new ReadableStream({
    start(controller) {
      controller.enqueue(enc.encode('retry: 3000\n\n'));
      // Task progress from runs started in the mocked task API.
      unsubscribe = onMockLiveEvent((type, payload) => {
        n++;
        controller.enqueue(enc.encode(`id: ${n}\nevent: ${type}\ndata: ${JSON.stringify(payload)}\n\n`));
      });
      timer = setInterval(() => {
        const seed = data.LIVE_SEEDS[n % data.LIVE_SEEDS.length]!;
        n++;
        const payload = { ...seed, id: `live_${Date.now()}`, published_at: new Date().toISOString() };
        controller.enqueue(enc.encode(`id: ${n}\nevent: component.published\ndata: ${JSON.stringify(payload)}\n\n`));
      }, LIVE_INTERVAL_MS);
    },
    cancel() {
      clearInterval(timer);
      unsubscribe?.();
    },
  });
}

const NAME_RE = /^[a-z0-9][a-z0-9-]{1,62}$/;

function cursorOffset(cursor: string | null): number {
  if (!cursor) return 0;
  const m = /^o(\d+)$/.exec(cursor);
  return m ? Number(m[1]) : 0;
}

function validateRepository(body: RepositoryInput): string | null {
  if (!NAME_RE.test(body.name ?? '')) return `repository name ${JSON.stringify(body.name)} is invalid`;
  if (!data.formats.includes(body.format)) return `format "${body.format}" is not supported by this server`;
  if (!['hosted', 'proxy', 'group'].includes(body.type)) return `unknown type "${body.type}"`;
  if (body.type !== 'group' && !body.blob_store) return `repository "${body.name}": blobStore is required`;
  if (body.type === 'proxy' && !/^https?:\/\/[^/]/.test(body.config?.proxy?.remote_url ?? '')) {
    return `repository "${body.name}": remoteUrl must be an absolute http(s) URL`;
  }
  if (body.type === 'group' && !body.config?.group?.members?.length) {
    return `repository "${body.name}": group requires at least one member`;
  }
  return null;
}

export const handlers = [
  ...openAPIHandlers,
  ...catalogHandlers(
    guard,
    () => state.repositories.map((r) => r.name),
    () => state.repositories,
  ),
  ...settingsHandlers(guard, () => state.repositories, () => allows(data.session.privileges, 'system', 'settings', 'admin')),
  ...signingHandlers(guard, () => state.repositories),
  ...routingHandlers(guard, () => state.repositories),
  ...findingsHandlers(
    guard,
    () => state.repositories.map((r) => r.name),
    (name) => state.repositories.find((r) => r.name === name)?.format,
  ),

  http.get(api('/system/status'), () => HttpResponse.json(mockSystemStatus(data.systemStatus))),
  http.get(api('/formats'), () => HttpResponse.json({ items: data.formats.map((name) => ({ name })) })),

  http.get(api('/auth/session'), () =>
    state.signedIn ? HttpResponse.json(sessionBody(), { headers: { [CSRF_HEADER]: state.csrf } }) : unauthenticated(),
  ),

  http.post(api('/auth/login'), async ({ request }) => {
    await delay(150);
    if (!(request.headers.get('Content-Type') ?? '').includes('application/json')) {
      return error(400, { code: 'invalid_argument', message: 'content type must be application/json' });
    }
    const body = (await request.json()) as LoginRequest;
    if (body.username === 'rate-limited') {
      return error(429, { code: 'rate_limited', message: 'too many login attempts', retry_after: 30 }, { 'Retry-After': '30' });
    }
    if (state.failedLogins >= MAX_FAILED_LOGINS) {
      return error(
        423,
        { code: 'locked', message: 'too many failed sign-ins from this address; try again later', retry_after: LOCK_SECONDS },
        { 'Retry-After': String(LOCK_SECONDS) },
      );
    }
    if (!body.username || !body.password) {
      return error(400, { code: 'invalid_argument', message: 'username and password are required' });
    }
    if (body.password === 'wrong' || body.password.length < 4) {
      state.failedLogins++;
      return error(401, { code: 'unauthenticated', message: 'invalid username or password' });
    }
    state.failedLogins = 0;
    state.signedIn = true;
    state.sessions++;
    state.csrf = `mock-csrf-${state.sessions}`;
    state.username = body.username;
    // "admin" plays the generated first administrator: its password must be replaced.
    state.passwordChangeRequired = body.username === 'admin';
    if (state.passwordChangeRequired) setMockPassword(body.password);
    return HttpResponse.json(sessionBody(), {
      headers: { [CSRF_HEADER]: state.csrf, 'Set-Cookie': `hangar_csrf=${state.csrf}; Path=/; SameSite=Lax` },
    });
  }),

  http.post(api('/auth/logout'), ({ request }) => {
    const denied = guard(request, true);
    if (denied && denied.status === 403) return denied;
    state.signedIn = false;
    state.passwordChangeRequired = false;
    return new HttpResponse(null, { status: 204, headers: { 'Set-Cookie': 'hangar_csrf=; Path=/; Max-Age=0' } });
  }),

  http.get(api('/metrics/overview'), ({ request }) => {
    const denied = guard(request);
    if (denied) return denied;
    const range = new URL(request.url).searchParams.get('range') as OverviewRange | null;
    if (!range || !RANGES.includes(range)) {
      return error(400, { code: 'invalid_argument', message: 'range must be one of 1h, today, 7d, 30d' });
    }
    return HttpResponse.json(data.overview(range));
  }),

  http.get(api('/components'), ({ request }) => {
    const denied = guard(request);
    if (denied) return denied;
    const limit = Number(new URL(request.url).searchParams.get('limit') ?? 20);
    return HttpResponse.json({ items: data.recentComponents().slice(0, limit), next_cursor: null });
  }),

  http.get(api('/upstreams/health'), ({ request }) => {
    const denied = guard(request);
    if (denied) return denied;
    const range = (new URL(request.url).searchParams.get('range') ?? 'today') as OverviewRange;
    if (!RANGES.includes(range)) {
      return error(400, { code: 'invalid_argument', message: 'range must be one of 1h, today, 7d, 30d' });
    }
    return HttpResponse.json({ items: data.upstreamHealth(range), next_cursor: null });
  }),
  http.get(api('/me/client-endpoints'), ({ request }) => guard(request) ?? HttpResponse.json({ items: data.clientEndpoints, next_cursor: null })),

  http.get(api('/repositories'), ({ request }) => {
    const denied = guard(request);
    if (denied) return denied;
    const p = new URL(request.url).searchParams;
    const type = p.get('type') as RepositoryType | null;
    const format = p.get('format');
    const q = p.get('q');
    const limit = Math.min(1000, Math.max(1, Number(p.get('limit') ?? 50)));
    const offset = cursorOffset(p.get('cursor'));
    // Counts follow the format and name filters but ignore the type filter (tab badges).
    const matching = state.repositories.filter((r) => (!format || r.format === format) && (!q || r.name.includes(q)));
    const counts = { all: matching.length, hosted: 0, proxy: 0, group: 0 };
    for (const r of matching) counts[r.type]++;
    const sort = p.get('sort');
    if (sort && !/^-?(name|type|format|last_activity)$/.test(sort)) return error(400, { code: 'invalid_argument', message: `sort "${sort}"` });
    const rows = data.sortRepositories(
      matching.filter((r) => !type || r.type === type),
      sort,
    );
    const items = rows.slice(offset, offset + limit);
    const next = offset + limit < rows.length ? `o${offset + limit}` : null;
    return HttpResponse.json({ items, next_cursor: next, counts });
  }),

  http.post(api('/repositories'), async ({ request }) => {
    const denied = guard(request, true);
    if (denied) return denied;
    await delay(100);
    const body = (await request.json()) as RepositoryInput;
    const invalid = validateRepository(body);
    if (invalid) return error(400, { code: 'invalid_argument', message: invalid });
    if (state.repositories.some((r) => r.name === body.name)) {
      return error(409, { code: 'conflict', message: `repository "${body.name}" already exists` });
    }
    const now = new Date().toISOString();
    const repo: Repository = {
      ...body,
      description: body.description ?? '',
      online: body.online ?? true,
      id: `r_${Date.now()}`,
      url: data.repositoryUrl(body.format, body.name),
      status: 'online',
      size_bytes: 0,
      component_count: 0,
      created_at: now,
      updated_at: now,
      last_activity_at: null,
    };
    if (repo.config.proxy?.auth) repo.config.proxy.auth = { ...repo.config.proxy.auth, password: undefined, token: undefined };
    state.repositories = [...state.repositories, repo];
    return HttpResponse.json(repo, { status: 201 });
  }),

  http.get(api('/repositories/:name'), ({ request, params }) => {
    const denied = guard(request);
    if (denied) return denied;
    const repo = state.repositories.find((r) => r.name === params.name);
    return repo ? HttpResponse.json(repo) : error(404, { code: 'not_found', message: `repository "${String(params.name)}" not found` });
  }),

  http.put(api('/repositories/:name'), async ({ request, params }) => {
    const denied = guard(request, true);
    if (denied) return denied;
    const old = state.repositories.find((r) => r.name === params.name);
    if (!old) return error(404, { code: 'not_found', message: `repository "${String(params.name)}" not found` });
    const body = (await request.json()) as RepositoryInput;
    if (body.name !== old.name || body.format !== old.format || body.type !== old.type) {
      return error(400, { code: 'invalid_argument', message: 'name, format and type are immutable' });
    }
    if ((body.blob_store ?? '') !== (old.blob_store ?? '')) {
      return error(400, { code: 'invalid_argument', message: `repository "${old.name}": blob store cannot be changed` });
    }
    const invalid = validateRepository(body);
    if (invalid) return error(400, { code: 'invalid_argument', message: invalid });
    const config = structuredClone(body.config);
    // Upstream auth: left out keeps the stored credentials, null removes them,
    // and they never follow the remote to another host unless resent.
    const oldAuth = old.config.proxy?.auth;
    if (config.proxy && oldAuth) {
      const sent = config.proxy.auth;
      const moved = new URL(config.proxy.remote_url).origin !== new URL(old.config.proxy?.remote_url ?? '').origin;
      if (moved && sent !== null && !sent?.password && !sent?.token) {
        return error(400, {
          code: 'invalid_argument',
          message: 'config.proxy.remote_url moves to another host: resend config.proxy.auth with its password or token, or set "auth": null',
        });
      }
      if (sent === undefined) config.proxy.auth = oldAuth;
      if (sent === null) delete config.proxy.auth;
    }
    if (config.proxy?.auth) config.proxy.auth = { ...config.proxy.auth, password: undefined, token: undefined };
    const online = body.online ?? true;
    const repo: Repository = {
      ...old,
      description: body.description ?? '',
      online,
      status: online ? (old.status === 'offline' ? 'online' : old.status) : 'offline',
      config,
      updated_at: new Date().toISOString(),
    };
    state.repositories = state.repositories.map((r) => (r.name === repo.name ? repo : r));
    return HttpResponse.json(repo);
  }),

  http.delete(api('/repositories/:name'), ({ request, params }) => {
    const denied = guard(request, true);
    if (denied) return denied;
    const name = String(params.name);
    if (!state.repositories.some((r) => r.name === name)) return error(404, { code: 'not_found', message: `repository "${name}" not found` });
    const groups = state.repositories.filter((r) => r.config.group?.members.includes(name)).map((r) => r.name);
    if (groups.length) {
      return error(400, { code: 'invalid_argument', message: `repository "${name}" is a member of group(s) ${groups.join(', ')}; remove it there first` });
    }
    state.repositories = state.repositories.filter((r) => r.name !== name);
    return new HttpResponse(null, { status: 204 });
  }),

  ...opsHandlers(guard),
  ...systemHandlers(guard),

  ...accessHandlers(guard, passwordChanged),

  http.get(api('/events'), ({ request }) =>
    guard(request) ??
    new HttpResponse(eventStream(), {
      headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' },
    }),
  ),
];
