/**
 * MSW handlers for the Settings section (sign-in providers and outbound
 * proxies), following api/openapi.yaml. State is in memory and reset
 * between tests; /auth/providers reflects it like the real server.
 */
import { http, HttpResponse } from 'msw';
import type { ApiError, AuthProviders, Repository } from '../api/types';
import type {
  IdentityProvider,
  LDAPInput,
  LDAPSettings,
  OIDCInput,
  OIDCSettings,
  OutboundProxy,
  OutboundProxyInput,
  TestResult,
} from '../pages/settings/settingsApi';
import { authProviders } from './data';

const api = (path: string) => `*/api/v1${path}`;
const ROLES = ['Admin', 'Publisher', 'Reader', 'CI deployer'];
const OIDC_FIELDS = ['enabled', 'issuer_url', 'client_id', 'client_secret', 'client_auth', 'logout', 'display_name', 'scopes', 'username_claim', 'email_claim', 'name_claim', 'groups_claim', 'ca_pem', 'group_roles', 'default_roles', 'allowed_groups', 'auto_create'];
export const LDAP_FILE_FIELDS = ['enabled', 'url', 'start_tls', 'ca_pem', 'bind_dn', 'bind_password', 'user_base_dn', 'user_filter', 'username_attribute', 'email_attribute', 'name_attribute', 'group_attribute', 'group_base_dn', 'group_filter', 'group_name_attribute', 'nested_groups', 'group_roles', 'default_roles', 'allowed_groups', 'auto_create'];
const PROXY_FILE_FIELDS = ['scheme', 'host', 'port', 'username', 'password', 'no_proxy', 'redirect_hosts', 'ca_pem', 'default'];
const iso = (offsetMs = 0) => new Date(Date.now() + offsetMs).toISOString();

function seed() {
  const oidc: OIDCSettings = {
    enabled: true,
    issuer_url: 'https://sso.acme.example/realms/acme',
    client_id: 'hangar',
    client_secret_configured: true,
    client_auth: 'basic',
    logout: 'provider',
    display_name: 'company SSO',
    scopes: ['openid', 'profile', 'email'],
    username_claim: 'preferred_username',
    email_claim: 'email',
    name_claim: 'name',
    groups_claim: 'groups',
    ca_pem: '',
    preset: 'keycloak',
    group_roles: [
      { group: '/platform-admins', roles: ['Admin'] },
      { group: '/developers', roles: ['Publisher'] },
    ],
    default_roles: ['Reader'],
    allowed_groups: [],
    auto_create: true,
    redirect_uri: 'https://hangar.acme.example/api/v1/auth/oidc/callback',
    source: 'database',
    file_fields: [],
    updated_at: iso(-3 * 86_400_000),
    updated_by: 'd.ismoilov',
  };
  const ldap: LDAPSettings = {
    enabled: true,
    url: 'ldaps://dc1.acme.example',
    start_tls: false,
    ca_pem: '',
    bind_dn: 'CN=svc-hangar,OU=Service,DC=acme,DC=example',
    bind_password_configured: true,
    user_base_dn: 'OU=People,DC=acme,DC=example',
    user_filter: '(&(objectClass=user)(sAMAccountName={username}))',
    username_attribute: 'sAMAccountName',
    email_attribute: 'mail',
    name_attribute: 'displayName',
    group_attribute: 'memberOf',
    group_base_dn: '',
    group_filter: '(member={dn})',
    group_name_attribute: 'cn',
    nested_groups: false,
    preset: 'active-directory',
    group_roles: [{ group: 'Hangar Admins', roles: ['Admin'] }],
    default_roles: [],
    allowed_groups: [],
    auto_create: true,
    source: 'database',
    file_fields: [],
    updated_at: iso(-10 * 86_400_000),
    updated_by: 'd.ismoilov',
  };
  const proxy = (p: Partial<OutboundProxy> & Pick<OutboundProxy, 'name' | 'host' | 'port'>): OutboundProxy => ({
    scheme: 'http',
    username: '',
    no_proxy: [],
    redirect_hosts: [],
    ca_pem: '',
    default: false,
    password_configured: false,
    source: 'database',
    file_fields: [],
    repositories: 0,
    inheriting: 0,
    created_at: iso(-30 * 86_400_000),
    updated_at: iso(-30 * 86_400_000),
    updated_by: 'd.ismoilov',
    ...p,
  });
  const proxies: OutboundProxy[] = [
    proxy({ name: 'corp-proxy', host: 'proxy.acme.example', port: 3128, username: 'svc-hangar', password_configured: true, default: true, no_proxy: ['.acme.example', '10.0.0.0/8'] }),
    proxy({ name: 'dmz-proxy', host: '10.20.0.5', port: 8080, redirect_hosts: ['.cloudflare.docker.com'] }),
  ];
  return { oidc, ldap, proxies, environmentProxy: '' };
}

let state = seed();

export function resetSettingsState(): void {
  state = seed();
}

/** Test helpers: change what the mocked server holds. */
export const mockSettings = {
  /** Makes OIDC managed by the configuration file (which stores no preset). */
  oidcFromFile() {
    state.oidc = { ...state.oidc, source: 'file', file_fields: OIDC_FIELDS, preset: '', updated_at: null, updated_by: undefined };
  },
  /** Makes LDAP managed by the configuration file. */
  ldapFromFile() {
    state.ldap = { ...state.ldap, source: 'file', file_fields: LDAP_FILE_FIELDS, preset: '', updated_at: null, updated_by: undefined };
  },
  /** Neither provider was ever saved: the server's defaults. */
  nothingSaved() {
    state.oidc = {
      ...state.oidc, enabled: false, issuer_url: '', client_id: '', client_secret_configured: false, display_name: 'SSO', preset: '',
      group_roles: [], default_roles: [], source: 'default', updated_at: null, updated_by: undefined,
    };
    state.ldap = {
      ...state.ldap, enabled: false, url: '', bind_dn: '', bind_password_configured: false, user_base_dn: '', preset: '',
      group_roles: [], source: 'default', updated_at: null, updated_by: undefined,
    };
  },
  /** Adds the proxy of the configuration file (http.proxy), which becomes the default. */
  proxyFromFile() {
    state.proxies = state.proxies.map((p) => ({ ...p, default: false }));
    state.proxies.unshift({
      name: 'config', scheme: 'http', host: 'proxy.example.com', port: 3128, username: '', no_proxy: [], redirect_hosts: [], ca_pem: '', default: true,
      password_configured: false, source: 'file', file_fields: PROXY_FILE_FIELDS, repositories: 0, inheriting: 0, created_at: null, updated_at: null,
    });
  },
  /** No proxies at all, as on a server that reaches the internet directly. */
  noProxies(environmentProxy = '') {
    state.proxies = [];
    state.environmentProxy = environmentProxy;
  },
  get oidc() {
    return state.oidc;
  },
  get ldap() {
    return state.ldap;
  },
  get proxies(): readonly OutboundProxy[] {
    return state.proxies;
  },
};

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });
const invalid = (fields: Record<string, string>) =>
  error(400, { code: 'invalid_argument', message: `invalid settings: ${Object.entries(fields).map(([k, v]) => `${k}: ${v}`).join('; ')}`, details: { fields } });
const forbidden = () => error(403, { code: 'forbidden', message: 'forbidden' });
const fileManaged = (what: string) => error(400, { code: 'invalid_argument', message: `${what} is set in the configuration file; change it there` });

function checkMapping(f: Record<string, string>, rows: { group: string; roles: string[] }[], defaults: string[]) {
  rows.forEach((r, i) => {
    if (!r.group.trim()) f[`group_roles[${i}].group`] = 'is required';
    if (!r.roles.length) f[`group_roles[${i}].roles`] = 'pick at least one role';
    const missing = r.roles.find((x) => !ROLES.includes(x));
    if (missing) f[`group_roles[${i}].roles`] = `role "${missing}" does not exist`;
  });
  const missing = defaults.find((x) => !ROLES.includes(x));
  if (missing) f.default_roles = `role "${missing}" does not exist`;
}

function validateOidc(b: OIDCInput): Record<string, string> {
  const f: Record<string, string> = {};
  if (b.enabled && !b.issuer_url) f.issuer_url = 'is required';
  else if (b.issuer_url && !/^https:\/\/[^/]+/.test(b.issuer_url) && !/^http:\/\/(localhost|127\.0\.0\.1)/.test(b.issuer_url)) {
    f.issuer_url = `"${b.issuer_url}" must be an absolute https:// URL`;
  }
  if (b.enabled && !b.client_id) f.client_id = 'is required';
  if (!b.scopes.includes('openid')) f.scopes = 'must include openid';
  checkMapping(f, b.group_roles, b.default_roles);
  return f;
}

function validateLdap(b: LDAPInput, stored: boolean): Record<string, string> {
  const f: Record<string, string> = {};
  if (b.enabled && !b.url) f.url = 'is required';
  else if (b.url && !/^ldaps?:\/\/[^/]+/.test(b.url)) f.url = `"${b.url}" must be ldap://host[:port] or ldaps://host[:port]`;
  if (b.bind_dn && !b.bind_password && (!stored || b.clear_bind_password)) f.bind_password = 'is required when bind_dn is set';
  if (b.enabled && !b.user_base_dn) f.user_base_dn = 'is required';
  if (!b.user_filter.includes('{username}')) f.user_filter = 'must contain {username}';
  checkMapping(f, b.group_roles, b.default_roles);
  return f;
}

const PROXY_NAME = /^[a-z0-9][a-z0-9._-]{0,62}$/;

function validateProxy(b: OutboundProxyInput, creating: boolean): Record<string, string> {
  const f: Record<string, string> = {};
  if (creating && !PROXY_NAME.test(b.name ?? '')) f.name = "use 1 to 63 lower-case letters, digits, '.', '_' or '-'";
  if (b.scheme !== 'http' && b.scheme !== 'https') f.scheme = 'choose http or https';
  if (!b.host) f.host = 'is required';
  if (!(b.port >= 1 && b.port <= 65535)) f.port = 'must be between 1 and 65535';
  if (b.default && state.proxies.some((p) => p.source === 'file')) f.default = 'the configuration file sets the platform default (http.proxy)';
  return f;
}

function step(name: string, status: 'ok' | 'failed' | 'skipped', message: string, latency_ms = 40) {
  return { name, status, message, latency_ms };
}

function result(steps: TestResult['steps'], okMessage: string, extra: Partial<TestResult> = {}): TestResult {
  const failed = steps.find((s) => s.status === 'failed');
  return { ok: !failed, message: failed ? failed.message : okMessage, steps, latency_ms: steps.reduce((n, s) => n + s.latency_ms, 0), ...extra };
}

function usage(repos: readonly Repository[], name: string, isDefault: boolean) {
  const proxyRepos = repos.filter((r) => r.type === 'proxy');
  return {
    repositories: proxyRepos.filter((r) => r.config.proxy?.http?.proxy_name === name).length,
    inheriting: isDefault ? proxyRepos.filter((r) => !r.config.proxy?.http?.proxy_name && (!r.config.proxy?.http?.proxy || r.config.proxy.http.proxy === 'inherit')).length : 0,
  };
}

/** Handlers; `guard` is the session/CSRF check shared with the other mock handlers. */
export function settingsHandlers(guard: (request: Request, mutating?: boolean) => Response | null, repositories: () => readonly Repository[], isAdmin: () => boolean) {
  const listProxies = () =>
    state.proxies.map((p) => ({ ...p, ...usage(repositories(), p.name, p.default) }));

  return [
    http.get(api('/auth/providers'), () => {
      const out: AuthProviders = {
        ...authProviders,
        oidc: state.oidc.enabled ? { display_name: state.oidc.display_name, start_url: '/api/v1/auth/oidc/start' } : null,
        ldap: { enabled: state.ldap.enabled },
      };
      return HttpResponse.json(out);
    }),

    http.get(api('/identity-providers'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      const items: IdentityProvider[] = [
        { name: 'oidc', kind: 'oidc', enabled: state.oidc.enabled, state: state.oidc.enabled ? 'ok' : 'disabled', last_sync_at: null },
        { name: 'ldap', kind: 'ldap', enabled: state.ldap.enabled, state: state.ldap.enabled ? 'ok' : 'disabled', last_sync_at: state.ldap.enabled ? iso(-120_000) : null },
      ];
      return HttpResponse.json({ items });
    }),

    http.get(api('/identity-providers/oidc'), ({ request }) => guard(request) ?? (isAdmin() ? HttpResponse.json(state.oidc) : forbidden())),
    http.put(api('/identity-providers/oidc'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!isAdmin()) return forbidden();
      if (state.oidc.source === 'file') return fileManaged('OIDC');
      const b = (await request.json()) as OIDCInput;
      const f = validateOidc(b);
      if (Object.keys(f).length) return invalid(f);
      const { client_secret, clear_client_secret, ...rest } = b;
      state.oidc = {
        ...state.oidc,
        ...rest,
        client_secret_configured: clear_client_secret ? false : !!client_secret || state.oidc.client_secret_configured,
        source: 'database',
        updated_at: iso(),
        updated_by: 'd.ismoilov',
      };
      return HttpResponse.json(state.oidc);
    }),
    http.post(api('/identity-providers/oidc:test'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!isAdmin()) return forbidden();
      const b = (await request.json()) as { settings?: OIDCInput };
      const s = b.settings ?? state.oidc;
      if (/unreachable/.test(s.issuer_url)) {
        return HttpResponse.json(result([step('discovery', 'failed', `could not read ${s.issuer_url}/.well-known/openid-configuration: connection refused`)], ''));
      }
      return HttpResponse.json(
        result(
          [
            step('discovery', 'ok', 'discovery document fetched', 62),
            step('issuer', 'ok', 'issuer matches', 0),
            step('keys', 'ok', '2 signing key(s) published', 31),
            step('client_credentials', 'skipped', 'the provider does not allow the client_credentials check for this client (unauthorized_client)', 28),
          ],
          'Discovery ok, credentials are verified at first sign-in',
        ),
      );
    }),

    http.get(api('/identity-providers/ldap'), ({ request }) => guard(request) ?? (isAdmin() ? HttpResponse.json(state.ldap) : forbidden())),
    http.put(api('/identity-providers/ldap'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!isAdmin()) return forbidden();
      if (state.ldap.source === 'file') return fileManaged('LDAP');
      const b = (await request.json()) as LDAPInput;
      const f = validateLdap(b, state.ldap.bind_password_configured);
      if (Object.keys(f).length) return invalid(f);
      const { bind_password, clear_bind_password, ...rest } = b;
      state.ldap = {
        ...state.ldap,
        ...rest,
        bind_password_configured: clear_bind_password ? false : !!bind_password || state.ldap.bind_password_configured,
        source: 'database',
        updated_at: iso(),
        updated_by: 'd.ismoilov',
      };
      return HttpResponse.json(state.ldap);
    }),
    http.post(api('/identity-providers/ldap:test'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!isAdmin()) return forbidden();
      const b = (await request.json()) as { settings?: LDAPInput; username?: string };
      const s = b.settings ?? state.ldap;
      const steps = [step('connect', 'ok', `connected to ${s.url}`, 18), step('bind', 'ok', `service account bound as ${s.bind_dn}`, 12)];
      if (!b.username) {
        steps.push(step('lookup', 'skipped', 'enter a username to look a user up', 0));
        return HttpResponse.json(result(steps, 'Directory reachable and service account accepted'));
      }
      if (b.username === 'nobody') {
        steps.push(step('lookup', 'failed', `no entry under "${s.user_base_dn}" matches the user filter for "nobody"`, 9));
        return HttpResponse.json(result(steps, ''));
      }
      const dn = `CN=${b.username},${s.user_base_dn}`;
      steps.push(step('lookup', 'ok', `found ${dn} with 2 group(s)`, 9));
      return HttpResponse.json(
        result(steps, 'Directory reachable and service account accepted', {
          user: { dn, username: b.username, email: `${b.username}@acme.example`, display_name: b.username, groups: ['CN=Hangar Admins,OU=Groups,DC=acme,DC=example', 'Hangar Admins'], roles: ['Admin'] },
        }),
      );
    }),

    http.get(api('/proxies'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      const items = listProxies();
      const def = items.find((p) => p.default)?.name ?? '';
      if (!isAdmin()) {
        return HttpResponse.json({
          items: items.map((p) => ({ ...p, scheme: 'http', host: '', port: 0, username: '', no_proxy: [], redirect_hosts: [], ca_pem: '', repositories: 0, inheriting: 0 })),
          default: def,
          limited: true,
        });
      }
      return HttpResponse.json({ items, default: def, limited: false, environment_proxy: def ? undefined : state.environmentProxy || undefined });
    }),
    http.post(api('/proxies'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!isAdmin()) return forbidden();
      const b = (await request.json()) as OutboundProxyInput;
      const f = validateProxy(b, true);
      if (Object.keys(f).length) return invalid(f);
      if (state.proxies.some((p) => p.name === b.name)) return error(409, { code: 'already_exists', message: `proxy "${b.name}": already exists` });
      const { password, clear_password, name, ...rest } = b;
      void clear_password;
      if (b.default) state.proxies = state.proxies.map((p) => ({ ...p, default: false }));
      const created: OutboundProxy = {
        ...rest, name: name!, password_configured: !!password, source: 'database', file_fields: [], repositories: 0, inheriting: 0,
        created_at: iso(), updated_at: iso(), updated_by: 'd.ismoilov',
      };
      state.proxies.push(created);
      return HttpResponse.json(created, { status: 201 });
    }),
    http.get(api('/proxies/:name'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      const p = listProxies().find((x) => x.name === params.name);
      return p ? HttpResponse.json(p) : error(404, { code: 'not_found', message: `proxy "${String(params.name)}": not found` });
    }),
    http.put(api('/proxies/:name'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!isAdmin()) return forbidden();
      const i = state.proxies.findIndex((x) => x.name === params.name);
      if (i < 0) return error(404, { code: 'not_found', message: 'not found' });
      if (state.proxies[i]!.source === 'file') return fileManaged(`proxy "${String(params.name)}"`);
      const b = (await request.json()) as OutboundProxyInput;
      const f = validateProxy(b, false);
      if (Object.keys(f).length) return invalid(f);
      const { password, clear_password, name, ...rest } = b;
      void name;
      if (b.default) state.proxies = state.proxies.map((p) => ({ ...p, default: false }));
      const old = state.proxies[i]!;
      state.proxies[i] = { ...old, ...rest, password_configured: clear_password ? false : !!password || old.password_configured, updated_at: iso() };
      return HttpResponse.json({ ...state.proxies[i], ...usage(repositories(), old.name, !!b.default) });
    }),
    http.delete(api('/proxies/:name'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!isAdmin()) return forbidden();
      const p = state.proxies.find((x) => x.name === params.name);
      if (!p) return error(404, { code: 'not_found', message: 'not found' });
      const users = repositories().filter((r) => r.config.proxy?.http?.proxy_name === p.name).map((r) => r.name);
      if (users.length) {
        return error(409, {
          code: 'in_use',
          message: `proxy "${p.name}" is used by ${users.length} repositories (${users.join(', ')}); point them to another proxy first`,
          details: { repositories: users, total: users.length },
        });
      }
      state.proxies = state.proxies.filter((x) => x !== p);
      return new HttpResponse(null, { status: 204 });
    }),
    http.post(api('/proxies/:name'), async ({ request, params }) => {
      // POST /proxies/{name}:test — MSW sees ":test" as part of the name.
      const denied = guard(request, true);
      if (denied) return denied;
      if (!isAdmin()) return forbidden();
      const name = String(params.name).replace(/:test$/, '');
      const b = (await request.json()) as { url?: string; settings?: OutboundProxyInput };
      const p = b.settings ?? state.proxies.find((x) => x.name === name);
      if (!p) return error(404, { code: 'not_found', message: `proxy "${name}": not found` });
      const url = b.url || 'https://api.nuget.org/v3/index.json';
      if (/bad|invalid/.test(p.host)) {
        return HttpResponse.json(result([step('request', 'failed', `GET ${url} through http://${p.host}:${p.port} failed: connection refused`, 1003)], ''));
      }
      const msg = `GET ${url} through ${p.scheme}://${p.host}:${p.port}: HTTP 200 in 184 ms`;
      return HttpResponse.json(result([step('request', 'ok', msg, 184)], msg, { status_code: 200 }));
    }),
  ];
}
