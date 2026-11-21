/**
 * MSW handlers for outgoing webhooks (api/openapi.yaml, tag webhooks). Like
 * the server, the stored URL is write-only (lists show it redacted), field
 * problems come back as details.fields, a URL that moves to another endpoint
 * needs the secret again, and deliveries are paged newest first. State is in
 * memory and reset between tests.
 */
import { http, HttpResponse } from 'msw';
import type { ApiError } from '../api/types';
import { allows } from '../lib/privileges';
import type { DeliveryState, Webhook, WebhookDelivery, WebhookEvent, WebhookFormat, WebhookInput, WebhookPayload } from '../pages/settings/webhooksApi';
import { recordMockAudit } from './ops';

const api = (path: string) => `*/api/v1${path}`;
const iso = (offsetMs = 0) => new Date(Date.now() + offsetMs).toISOString();
const MIN = 60_000;
const INSTANCE = 'https://pkg.acme.io';
const EVENTS: WebhookEvent[] = ['component.published', 'component.deleted', 'component.quarantined', 'component.released', 'repository.created', 'repository.deleted', 'scan.finding'];
const FORMATS: WebhookFormat[] = ['generic', 'slack', 'teams'];
const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$/;
const REPO_RE = /^[a-z0-9*?[\]^!-]{1,64}$/;
const MAX_ATTEMPTS = 8;
const DEFAULT_LIMIT = 50;

/** A webhook as stored: the full URL and the secret stay here. */
interface Stored extends Omit<Webhook, 'url' | 'secret_configured'> {
  fullUrl: string;
  secret: string;
}

export function redactUrl(raw: string): string {
  try {
    const u = new URL(raw);
    const rest = (u.pathname !== '' && u.pathname !== '/') || u.search ? '/***' : '';
    return `${u.protocol}//${u.host}${rest}`;
  } catch {
    return '***';
  }
}

function endpoint(raw: string): string {
  try {
    const u = new URL(raw);
    return `${u.protocol}//${u.hostname}:${u.port || (u.protocol === 'https:' ? '443' : '80')}`;
  } catch {
    return '(invalid URL)';
  }
}

const show = (h: Stored): Webhook => {
  const { fullUrl, secret, ...rest } = h;
  return { ...rest, url: redactUrl(fullUrl), secret_configured: secret !== '' };
};

let seq = 0;
const uuid = () => {
  seq++;
  return `00000000-0000-4000-8000-${seq.toString(16).padStart(12, '0')}`;
};

function payload(event: string, repository: string, subject: string, at: string, data: Record<string, unknown>): WebhookPayload {
  return { id: `ev_${uuid().slice(-8)}`, event, occurred_at: at, actor: 'ci-publisher', repository, subject, data, instance: INSTANCE };
}

interface MockDelivery extends WebhookDelivery {
  payload: WebhookPayload;
}

function delivery(h: Stored, minutesAgo: number, event: WebhookEvent | 'ping', repository: string, subject: string, outcome: { state: DeliveryState; code?: number; ms?: number; error?: string; attempts?: number }): MockDelivery {
  const created = iso(-minutesAgo * MIN);
  const done = outcome.state === 'succeeded' || outcome.state === 'failed';
  const p = payload(event, repository, subject, created, event === 'ping' ? { webhook: h.name } : { repository, name: subject.split('@')[0], version: subject.split('@')[1] ?? '' });
  return {
    id: uuid(),
    webhook_id: h.id,
    event_id: p.id,
    event,
    repository,
    subject,
    state: outcome.state,
    attempts: outcome.attempts ?? (outcome.state === 'pending' ? 0 : 1),
    max_attempts: event === 'ping' ? 1 : MAX_ATTEMPTS,
    next_attempt_at: outcome.state === 'pending' ? iso(2 * MIN) : null,
    last_status_code: outcome.code ?? null,
    last_latency_ms: outcome.ms ?? null,
    last_error: outcome.error ?? '',
    redelivery_of: null,
    created_at: created,
    completed_at: done ? iso(-minutesAgo * MIN + (outcome.ms ?? 50)) : null,
    payload: p,
  };
}

const PACKAGES = ['@acme/ui@4.2.0', '@acme/api-client@1.9.3', 'left-pad@1.3.0', '@acme/tokens@0.8.1', 'react@18.3.1', '@acme/charts@2.0.0-rc.1'];

function seed() {
  seq = 0;
  const base = { created_at: iso(-40 * 86_400_000), updated_at: iso(-6 * 86_400_000), updated_by: 'd.ismoilov' };
  const hooks: Stored[] = [
    {
      ...base,
      id: uuid(),
      name: 'ci-trigger',
      fullUrl: 'https://ci.acme.example/hooks/hangar?token=s3cr3t',
      secret: 'whsec-ci',
      events: ['component.published', 'component.deleted'],
      repositories: [],
      format: 'generic',
      enabled: true,
      consecutive_failures: 0,
      disabled_reason: '',
      last_delivery_at: iso(-3 * MIN),
      last_status_code: 200,
    },
    {
      ...base,
      id: uuid(),
      name: 'releases to Slack',
      fullUrl: 'https://hooks.slack.com/services/T024BE7LD/B08ACME/abcdefghijklmnop',
      secret: '',
      events: ['component.published'],
      repositories: ['npm-*', 'maven-releases'],
      format: 'slack',
      enabled: true,
      consecutive_failures: 2,
      disabled_reason: '',
      last_delivery_at: iso(-26 * MIN),
      last_status_code: 503,
    },
    {
      ...base,
      id: uuid(),
      name: 'security in Teams',
      fullUrl: 'https://acme.webhook.office.com/webhookb2/7d1e/IncomingWebhook/9f2c',
      secret: '',
      events: ['scan.finding', 'component.quarantined'],
      repositories: [],
      format: 'teams',
      enabled: false,
      consecutive_failures: 20,
      disabled_reason: 'disabled after 20 failed attempts in a row; last error: HTTP 410 Gone',
      last_delivery_at: iso(-2 * 86_400_000),
      last_status_code: 410,
    },
  ];
  const [ci, slack, teams] = hooks as [Stored, Stored, Stored];
  const deliveries: MockDelivery[] = [];
  // ci-trigger: more than one page, mostly succeeded, one still pending.
  for (let i = 0; i < 32; i++) {
    const pkg = PACKAGES[i % PACKAGES.length]!;
    const failed = i === 7 || i === 19;
    deliveries.push(
      delivery(ci, 3 + i * 47, i % 5 === 4 ? 'component.deleted' : 'component.published', 'npm-internal', pkg, failed ? { state: 'failed', code: 502, ms: 3012, error: 'HTTP 502 Bad Gateway', attempts: MAX_ATTEMPTS } : { state: 'succeeded', code: 200, ms: 40 + (i % 7) * 13 }),
    );
  }
  deliveries.unshift(delivery(ci, 0.5, 'component.published', 'npm-internal', '@acme/ui@4.2.1', { state: 'pending' }));
  deliveries.push(
    delivery(slack, 26, 'component.published', 'npm-internal', '@acme/ui@4.2.0', { state: 'pending', code: 503, ms: 210, error: 'HTTP 503 Service Unavailable', attempts: 2 }),
    delivery(slack, 300, 'component.published', 'maven-releases', 'com.acme:billing@3.4.0', { state: 'succeeded', code: 200, ms: 180 }),
    delivery(teams, 2 * 1440, 'scan.finding', 'nuget-proxy', 'System.Security.Cryptography.Xml@10.0.8', { state: 'failed', code: 410, ms: 95, error: 'HTTP 410 Gone', attempts: MAX_ATTEMPTS }),
  );
  return { hooks, deliveries };
}

let state = seed();

export function resetWebhooksState(): void {
  state = seed();
}

/** Test helpers. */
export const webhooksState = {
  get webhooks(): readonly Webhook[] {
    return state.hooks.map(show);
  },
  /** The stored, unredacted URL and secret of a webhook. */
  stored(name: string): { url: string; secret: string } | undefined {
    const h = state.hooks.find((x) => x.name === name);
    return h && { url: h.fullUrl, secret: h.secret };
  },
};

type Guard = (request: Request, mutating?: boolean) => Response | null;
const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });
const forbidden = () => error(403, { code: 'forbidden', message: 'forbidden' });
const notFound = (what: string) => error(404, { code: 'not_found', message: `${what}: not found` });
const invalid = (fields: Record<string, string>) =>
  error(400, {
    code: 'invalid_argument',
    message: `invalid webhook: ${Object.keys(fields)
      .sort()
      .map((k) => `${k}: ${fields[k]}`)
      .join('; ')}`,
    details: { fields },
  });

function urlProblem(raw: string): string | null {
  if (!raw) return null;
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return 'enter an absolute http:// or https:// URL';
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return 'enter an absolute http:// or https:// URL';
  if (u.username || u.password) return 'must not contain user information; use the secret to authenticate deliveries';
  if (u.hash) return 'must not contain a fragment (#...)';
  if (raw.includes('/***')) return 'is the redacted URL; leave it empty to keep the stored one';
  if (/^(localhost|127\.|10\.|192\.168\.|169\.254\.)/.test(u.hostname)) {
    return `is refused by the outbound address policy: ${u.hostname} is an internal or reserved address (an operator can allow it with http.allow_private_networks)`;
  }
  return null;
}

function check(b: WebhookInput, url: string): Record<string, string> {
  const f: Record<string, string> = {};
  if (!NAME_RE.test(b.name ?? '')) f.name = "use 1 to 64 letters, digits, spaces, '.', '_' or '-', starting with a letter or digit";
  if (!FORMATS.includes(b.format ?? 'generic')) f.format = `choose one of ${FORMATS.join(', ')}`;
  if (!b.events?.length) f.events = 'choose at least one event';
  const badEvent = (b.events ?? []).find((e) => !EVENTS.includes(e));
  if (badEvent) f.events = `"${badEvent}" is not an event; choose from ${EVENTS.join(', ')}`;
  const badRepo = (b.repositories ?? []).find((r) => !REPO_RE.test(r));
  if (badRepo) f.repositories = `"${badRepo}": enter a repository name or a glob such as npm-*`;
  const u = urlProblem(url);
  if (u) f.url = u;
  return f;
}

function page(items: MockDelivery[], request: Request) {
  const p = new URL(request.url).searchParams;
  const st = p.get('state');
  if (st && !['pending', 'delivering', 'succeeded', 'failed'].includes(st)) {
    return error(400, { code: 'invalid_argument', message: `state "${st}": want pending, delivering, succeeded or failed` });
  }
  const filtered = items.filter((d) => !st || d.state === st).sort((a, b) => b.created_at.localeCompare(a.created_at));
  const offset = Number(/^o(\d+)$/.exec(p.get('cursor') ?? '')?.[1] ?? 0);
  const limit = Math.min(Number(p.get('limit')) || DEFAULT_LIMIT, 200);
  const slice = filtered.slice(offset, offset + limit).map(({ payload: _p, ...d }) => {
    void _p;
    return d;
  });
  const next = offset + limit < filtered.length ? `o${offset + limit}` : null;
  return HttpResponse.json({ items: slice, next_cursor: next });
}

function record(action: string, target: string, details?: Record<string, unknown>) {
  recordMockAudit({ at: iso(), actor: 'd.ismoilov', action, target, outcome: 'success', ip: '10.4.2.18', details });
}

/** Handlers; `guard` is the shared session/CSRF check, `privileges` those of the signed-in mock user. */
export function webhooksHandlers(guard: Guard, privileges: () => readonly string[]) {
  const can = (action: 'read' | 'admin') => allows(privileges(), 'system', 'webhooks', action);
  const find = (id: string) => state.hooks.find((h) => h.id === id);

  return [
    http.get(api('/webhooks'), ({ request }) => {
      const denied = guard(request);
      if (denied) return denied;
      if (!can('read')) return forbidden();
      const items = [...state.hooks].sort((a, b) => a.name.localeCompare(b.name)).map(show);
      return HttpResponse.json({ items });
    }),

    http.post(api('/webhooks'), async ({ request }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!can('admin')) return forbidden();
      const b = (await request.json()) as WebhookInput;
      const f = check(b, b.url ?? '');
      if (!b.url) f.url = 'is required';
      if (Object.keys(f).length) return invalid(f);
      if (state.hooks.some((h) => h.name === b.name)) return error(409, { code: 'already_exists', message: `webhook "${b.name}": already exists` });
      const h: Stored = {
        id: uuid(),
        name: b.name,
        fullUrl: b.url!,
        secret: b.clear_secret ? '' : (b.secret ?? ''),
        events: b.events,
        repositories: b.repositories ?? [],
        format: b.format ?? 'generic',
        enabled: b.enabled ?? true,
        consecutive_failures: 0,
        disabled_reason: '',
        last_delivery_at: null,
        last_status_code: null,
        created_at: iso(),
        updated_at: iso(),
        updated_by: 'd.ismoilov',
      };
      state.hooks.push(h);
      record('webhook.create', `webhook:${h.name}`, { url: redactUrl(h.fullUrl), events: h.events });
      return HttpResponse.json(show(h), { status: 201 });
    }),

    http.get(api('/webhooks/:id'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      if (!can('read')) return forbidden();
      const h = find(String(params.id));
      return h ? HttpResponse.json(show(h)) : notFound(`webhook "${String(params.id)}"`);
    }),

    http.put(api('/webhooks/:id'), async ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!can('admin')) return forbidden();
      const h = find(String(params.id));
      if (!h) return notFound(`webhook "${String(params.id)}"`);
      const b = (await request.json()) as WebhookInput;
      const url = b.url || h.fullUrl;
      const f = check(b, url);
      if (!f.url && h.secret && !b.secret && !b.clear_secret && endpoint(url) !== endpoint(h.fullUrl)) {
        f.secret = `url changed from ${endpoint(h.fullUrl)} to ${endpoint(url)}: the stored secret is only sent to the endpoint it was entered for; enter it again, or clear it`;
      }
      if (Object.keys(f).length) return invalid(f);
      if (state.hooks.some((x) => x !== h && x.name === b.name)) return error(409, { code: 'already_exists', message: `webhook "${b.name}": already exists` });
      const enabled = b.enabled ?? h.enabled;
      Object.assign(h, {
        name: b.name,
        fullUrl: url,
        secret: b.clear_secret ? '' : b.secret || h.secret,
        events: b.events,
        repositories: b.repositories ?? [],
        format: b.format ?? 'generic',
        consecutive_failures: enabled && !h.enabled ? 0 : h.consecutive_failures,
        disabled_reason: enabled ? '' : h.disabled_reason,
        enabled,
        updated_at: iso(),
      });
      record('webhook.update', `webhook:${h.name}`, { url: redactUrl(h.fullUrl) });
      return HttpResponse.json(show(h));
    }),

    http.delete(api('/webhooks/:id'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!can('admin')) return forbidden();
      const h = find(String(params.id));
      if (!h) return notFound(`webhook "${String(params.id)}"`);
      state.hooks = state.hooks.filter((x) => x !== h);
      state.deliveries = state.deliveries.filter((d) => d.webhook_id !== h.id);
      record('webhook.delete', `webhook:${h.name}`);
      return new HttpResponse(null, { status: 204 });
    }),

    // POST /webhooks/{id}:test — MSW sees ":test" as part of the id.
    http.post(api('/webhooks/:id'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!can('admin')) return forbidden();
      const id = String(params.id).replace(/:test$/, '');
      const h = find(id);
      if (!h) return notFound(`webhook "${id}"`);
      const gone = h.format === 'teams' && h.disabled_reason !== '';
      const d = delivery(h, 0, 'ping', '', '', gone ? { state: 'failed', code: 410, ms: 88, error: 'HTTP 410 Gone' } : { state: 'succeeded', code: 200, ms: 64 });
      state.deliveries.unshift(d);
      record('webhook.test', `webhook:${h.name}`);
      const { payload: _p, ...out } = d;
      void _p;
      return HttpResponse.json(out);
    }),

    http.get(api('/webhooks/:id/deliveries'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      if (!can('read')) return forbidden();
      const h = find(String(params.id));
      if (!h) return notFound(`webhook "${String(params.id)}"`);
      return page(
        state.deliveries.filter((d) => d.webhook_id === h.id),
        request,
      );
    }),

    http.get(api('/webhooks/:id/deliveries/:delivery'), ({ request, params }) => {
      const denied = guard(request);
      if (denied) return denied;
      if (!can('read')) return forbidden();
      const d = state.deliveries.find((x) => x.webhook_id === String(params.id) && x.id === String(params.delivery));
      return d ? HttpResponse.json(d) : notFound(`delivery "${String(params.delivery)}"`);
    }),

    // POST /webhooks/{id}/deliveries/{delivery_id}:redeliver
    http.post(api('/webhooks/:id/deliveries/:delivery'), ({ request, params }) => {
      const denied = guard(request, true);
      if (denied) return denied;
      if (!can('admin')) return forbidden();
      const h = find(String(params.id));
      const id = String(params.delivery).replace(/:redeliver$/, '');
      const old = state.deliveries.find((x) => x.webhook_id === String(params.id) && x.id === id);
      if (!h || !old) return notFound(`delivery "${id}"`);
      if (!h.enabled) return error(400, { code: 'invalid_argument', message: `webhook "${h.name}" is disabled; enable it first` });
      const d: MockDelivery = {
        ...old,
        id: uuid(),
        state: 'pending',
        attempts: 0,
        max_attempts: MAX_ATTEMPTS,
        next_attempt_at: iso(),
        last_status_code: null,
        last_latency_ms: null,
        last_error: '',
        redelivery_of: old.id,
        created_at: iso(),
        completed_at: null,
      };
      state.deliveries.unshift(d);
      record('webhook.redeliver', `webhook:${h.name}`, { delivery: old.id });
      const { payload: _p, ...out } = d;
      void _p;
      return HttpResponse.json(out, { status: 202 });
    }),
  ];
}
