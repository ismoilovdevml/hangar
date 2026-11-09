import type { ApiError } from './types';

export const API_BASE = '/api/v1';

/** Cookie the server sets next to the HttpOnly session cookie (double-submit token). */
export const CSRF_COOKIE = 'hangar_csrf';
/** Header the server reads the token from on mutating requests, and echoes it in. */
export const CSRF_HEADER = 'X-CSRF-Token';

export class ApiRequestError extends Error {
  readonly status: number;
  readonly body: ApiError | null;
  /** Seconds until the action may be retried (423 lockout, 429 rate limit), from the body or Retry-After. */
  readonly retryAfter: number | null;

  constructor(status: number, body: ApiError | null, retryAfter: number | null = null) {
    super(body?.message ?? `Request failed with status ${status}`);
    this.name = 'ApiRequestError';
    this.status = status;
    this.body = body;
    this.retryAfter = retryAfter ?? body?.retry_after ?? null;
  }

  get code(): string | undefined {
    return this.body?.code;
  }
}

/* ---------------------------------------------------------------- CSRF */

let csrfFromHeader: string | null = null;

function readCookie(name: string): string | null {
  if (typeof document === 'undefined') return null;
  for (const part of document.cookie.split(';')) {
    const eq = part.indexOf('=');
    if (eq < 0) continue;
    if (part.slice(0, eq).trim() === name) {
      const v = decodeURIComponent(part.slice(eq + 1).trim());
      return v || null;
    }
  }
  return null;
}

/** The token to send on mutating requests: the cookie first, else the last one the server echoed. */
export function csrfToken(): string | null {
  return readCookie(CSRF_COOKIE) ?? csrfFromHeader;
}

/** Forgets the remembered token (sign-out). Exported for tests. */
export function resetCsrfToken(): void {
  csrfFromHeader = null;
}

function rememberCsrf(res: Response): void {
  const t = res.headers.get(CSRF_HEADER);
  if (t) csrfFromHeader = t;
}

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);

/* ---------------------------------------------------------------- 401 */

type UnauthorizedHandler = () => void;
let onUnauthorized: UnauthorizedHandler | null = null;

/**
 * Called when an API call answers 401 (session expired or revoked). The app
 * registers a handler that sends the user to /login; returns the unregister function.
 */
export function setUnauthorizedHandler(fn: UnauthorizedHandler | null): () => void {
  onUnauthorized = fn;
  return () => {
    if (onUnauthorized === fn) onUnauthorized = null;
  };
}

/** Reports a 401 seen outside apiFetch (the event stream). */
export function signalUnauthorized(): void {
  onUnauthorized?.();
}

/** Error code of every call made while the signed-in user must change their password. */
export const PASSWORD_CHANGE_REQUIRED = 'password_change_required';

let onPasswordChangeRequired: (() => void) | null = null;

/**
 * Called when an API call answers 403 `password_change_required` (an
 * administrator required a new password while this session was open). The app
 * sends the user to the change-password page; returns the unregister function.
 */
export function setPasswordChangeHandler(fn: (() => void) | null): () => void {
  onPasswordChangeRequired = fn;
  return () => {
    if (onPasswordChangeRequired === fn) onPasswordChangeRequired = null;
  };
}

/* ---------------------------------------------------------------- fetch */

type Query = Record<string, string | number | boolean | null | undefined>;

export function buildUrl(path: string, query?: Query): string {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v !== undefined && v !== null && v !== '') qs.set(k, String(v));
  }
  const s = qs.toString();
  return `${API_BASE}${path}${s ? `?${s}` : ''}`;
}

export interface ApiFetchInit {
  method?: string;
  query?: Query;
  body?: unknown;
  /** A body sent as is (a file), with its own headers, instead of JSON `body`. */
  raw?: { body: BodyInit; headers?: Record<string, string> };
  signal?: AbortSignal;
  /**
   * Leave a 401 to the caller instead of redirecting to sign-in: the login call
   * (401 = wrong password) and the session probe the router makes itself.
   */
  allowUnauthorized?: boolean;
}

function parseRetryAfter(res: Response): number | null {
  const h = res.headers.get('Retry-After');
  if (!h) return null;
  const secs = Number(h);
  if (Number.isFinite(secs)) return Math.max(0, Math.ceil(secs));
  const at = Date.parse(h);
  return Number.isNaN(at) ? null : Math.max(0, Math.ceil((at - Date.now()) / 1000));
}

async function send(path: string, init: ApiFetchInit): Promise<Response> {
  const method = (init.method ?? 'GET').toUpperCase();
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (init.body !== undefined) headers['Content-Type'] = 'application/json';
  if (init.raw) Object.assign(headers, { 'Content-Type': 'application/octet-stream' }, init.raw.headers);
  if (!SAFE_METHODS.has(method)) {
    const token = csrfToken();
    if (token) headers[CSRF_HEADER] = token;
  }
  const res = await fetch(buildUrl(path, init.query), {
    method,
    credentials: 'same-origin',
    headers,
    body: init.raw ? init.raw.body : init.body !== undefined ? JSON.stringify(init.body) : undefined,
    signal: init.signal,
  });
  rememberCsrf(res);
  return res;
}

async function toError(res: Response): Promise<ApiRequestError> {
  let body: ApiError | null = null;
  try {
    body = (await res.json()) as ApiError;
  } catch {
    body = null;
  }
  return new ApiRequestError(res.status, body, parseRetryAfter(res));
}

/** Refreshes the CSRF token from GET /auth/session (the server echoes it in a header). */
async function refreshCsrf(signal?: AbortSignal): Promise<boolean> {
  try {
    const res = await fetch(buildUrl('/auth/session'), {
      credentials: 'same-origin',
      headers: { Accept: 'application/json' },
      signal,
    });
    rememberCsrf(res);
    return res.ok;
  } catch {
    return false;
  }
}

export async function apiFetch<T>(path: string, init: ApiFetchInit = {}): Promise<T> {
  let res = await send(path, init);
  // A stale token (another tab signed in again) gets one refresh and retry.
  if (res.status === 403 && !SAFE_METHODS.has((init.method ?? 'GET').toUpperCase())) {
    const err = await toError(res.clone());
    if (err.code === 'csrf_failed' && (await refreshCsrf(init.signal))) {
      res = await send(path, init);
    }
  }
  if (!res.ok) {
    const err = await toError(res);
    if (err.status === 401 && !init.allowUnauthorized) onUnauthorized?.();
    if (err.status === 403 && err.code === PASSWORD_CHANGE_REQUIRED) onPasswordChangeRequired?.();
    throw err;
  }
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}
