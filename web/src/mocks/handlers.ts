import { delay, http, HttpResponse } from 'msw';
import type { ApiError, LoginRequest, OverviewRange } from '../api/types';
import * as data from './data';

const api = (path: string) => `*/api/v1${path}`;

const state = { signedIn: true, failedLogins: 0 };
const MAX_FAILED_LOGINS = 5;

export function resetMockState(): void {
  state.signedIn = true;
  state.failedLogins = 0;
}

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });

const RANGES: OverviewRange[] = ['1h', 'today', '7d', '30d'];

/** Interval between mocked live "component.published" events. */
export const LIVE_INTERVAL_MS = 25_000;

function eventStream(): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  let timer: ReturnType<typeof setInterval> | undefined;
  let n = 0;
  return new ReadableStream({
    start(controller) {
      controller.enqueue(enc.encode('retry: 5000\n: connected\n\n'));
      timer = setInterval(() => {
        const seed = data.LIVE_SEEDS[n % data.LIVE_SEEDS.length]!;
        n++;
        const payload = { ...seed, id: `live_${Date.now()}`, published_at: new Date().toISOString() };
        controller.enqueue(enc.encode(`event: component.published\ndata: ${JSON.stringify(payload)}\n\n`));
      }, LIVE_INTERVAL_MS);
    },
    cancel() {
      clearInterval(timer);
    },
  });
}

export const handlers = [
  http.get(api('/system/status'), () => HttpResponse.json(data.systemStatus)),
  http.get(api('/auth/providers'), () => HttpResponse.json(data.authProviders)),

  http.get(api('/auth/session'), () =>
    state.signedIn
      ? HttpResponse.json(data.session)
      : error(401, { code: 'unauthenticated', message: 'Sign in to continue.' }),
  ),

  http.post(api('/auth/login'), async ({ request }) => {
    await delay(200);
    const body = (await request.json()) as LoginRequest;
    if (state.failedLogins >= MAX_FAILED_LOGINS) {
      return error(423, { code: 'locked', message: 'Too many failed attempts.', retry_after: 900 });
    }
    if (!body.username || body.password === 'wrong' || body.password.length < 4) {
      state.failedLogins++;
      return error(401, { code: 'invalid_credentials', message: 'Wrong username or password.' });
    }
    state.failedLogins = 0;
    state.signedIn = true;
    return HttpResponse.json(data.session);
  }),

  http.post(api('/auth/logout'), () => {
    state.signedIn = false;
    return new HttpResponse(null, { status: 204 });
  }),

  http.get(api('/metrics/overview'), ({ request }) => {
    const range = new URL(request.url).searchParams.get('range') as OverviewRange | null;
    if (!range || !RANGES.includes(range)) {
      return error(400, { code: 'invalid_range', message: 'range must be one of 1h, today, 7d, 30d' });
    }
    return HttpResponse.json(data.overview(range));
  }),

  http.get(api('/components'), ({ request }) => {
    const limit = Number(new URL(request.url).searchParams.get('limit') ?? 20);
    return HttpResponse.json({ items: data.recentComponents().slice(0, limit), next_cursor: null });
  }),

  http.get(api('/upstreams/health'), () => HttpResponse.json({ items: data.upstreams, next_cursor: null })),
  http.get(api('/me/client-endpoints'), () => HttpResponse.json({ items: data.clientEndpoints, next_cursor: null })),

  http.get(api('/events'), () =>
    new HttpResponse(eventStream(), {
      headers: { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'keep-alive' },
    }),
  ),
];
