/**
 * A small Server-Sent Events client over fetch, used instead of EventSource so
 * the app controls reconnects: EventSource gives up for good on a non-200
 * answer (401 after the session expires, 503 while the event bus restarts) and
 * cannot resend Last-Event-ID after such a stop.
 */

export interface SseMessage {
  id: string | undefined;
  event: string;
  data: string;
}

export interface SseParser {
  push(chunk: string): void;
  /** Last `id:` seen (sticky across messages, as in the SSE spec). */
  readonly lastEventId: string | undefined;
  /** Last `retry:` value in milliseconds. */
  readonly retry: number | undefined;
}

/** Incremental parser for the text/event-stream format (WHATWG HTML §9.2.6). */
export function createSseParser(onMessage: (m: SseMessage) => void): SseParser {
  let buffer = '';
  let event = '';
  let data: string[] = [];
  let lastEventId: string | undefined;
  let retry: number | undefined;

  const dispatch = () => {
    if (data.length > 0) {
      onMessage({ id: lastEventId, event: event || 'message', data: data.join('\n') });
    }
    event = '';
    data = [];
  };

  const line = (l: string) => {
    if (l === '') return dispatch();
    if (l.startsWith(':')) return; // comment / keep-alive ping
    const colon = l.indexOf(':');
    const field = colon < 0 ? l : l.slice(0, colon);
    let value = colon < 0 ? '' : l.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    switch (field) {
      case 'event':
        event = value;
        break;
      case 'data':
        data.push(value);
        break;
      case 'id':
        if (!value.includes('\0')) lastEventId = value;
        break;
      case 'retry':
        if (/^\d+$/.test(value)) retry = Number(value);
        break;
    }
  };

  return {
    push(chunk: string) {
      buffer += chunk;
      // Keep a trailing "\r" in the buffer: it may be the first half of "\r\n".
      const re = /\r\n|\r(?!$)|\n/g;
      let start = 0;
      let m: RegExpExecArray | null;
      while ((m = re.exec(buffer))) {
        line(buffer.slice(start, m.index));
        start = m.index + m[0].length;
      }
      buffer = buffer.slice(start);
    },
    get lastEventId() {
      return lastEventId;
    },
    get retry() {
      return retry;
    },
  };
}

export type StreamStatus = 'connecting' | 'open' | 'reconnecting' | 'closed' | 'unavailable';

export interface StreamOptions {
  url: string;
  onMessage: (m: SseMessage) => void;
  onStatus?: (s: StreamStatus) => void;
  /** The server answered 401: the session is gone. The stream stops. */
  onUnauthorized?: () => void;
  /** Initial reconnect delay; the server's `retry:` field overrides it. */
  retryMs?: number;
  maxRetryMs?: number;
  fetchImpl?: typeof fetch;
  /** Last-Event-ID to resume from on the first connect. */
  lastEventId?: string;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const t = setTimeout(done, ms);
    function done() {
      clearTimeout(t);
      signal.removeEventListener('abort', done);
      resolve();
    }
    signal.addEventListener('abort', done);
  });
}

/**
 * Opens the stream and keeps it open: reconnects with exponential backoff
 * (plus jitter) and resumes with Last-Event-ID. Returns a function that closes it.
 */
export function openEventStream(opts: StreamOptions): () => void {
  const ctrl = new AbortController();
  const doFetch = opts.fetchImpl ?? ((...a: Parameters<typeof fetch>) => fetch(...a));
  const maxRetry = opts.maxRetryMs ?? 60_000;
  let lastId = opts.lastEventId;
  let baseRetry = opts.retryMs ?? 3_000;

  const run = async () => {
    let attempt = 0;
    while (!ctrl.signal.aborted) {
      opts.onStatus?.(attempt === 0 ? 'connecting' : 'reconnecting');
      try {
        const headers: Record<string, string> = { Accept: 'text/event-stream' };
        if (lastId) headers['Last-Event-ID'] = lastId;
        const res = await doFetch(opts.url, {
          headers,
          credentials: 'same-origin',
          cache: 'no-store',
          signal: ctrl.signal,
        });
        if (res.status === 401) {
          opts.onStatus?.('closed');
          opts.onUnauthorized?.();
          return;
        }
        if (res.status === 501 || res.status === 404) {
          // This server has no event stream: nothing to retry.
          opts.onStatus?.('unavailable');
          return;
        }
        if (!res.ok || !res.body) throw new Error(`event stream: HTTP ${res.status}`);
        opts.onStatus?.('open');
        attempt = 0;
        const parser = createSseParser((m) => {
          if (m.id !== undefined) lastId = m.id;
          opts.onMessage(m);
        });
        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          parser.push(decoder.decode(value, { stream: true }));
          if (parser.retry !== undefined) baseRetry = parser.retry;
        }
      } catch {
        if (ctrl.signal.aborted) return;
      }
      attempt++;
      const backoff = Math.min(maxRetry, baseRetry * 2 ** Math.min(attempt - 1, 8));
      opts.onStatus?.('reconnecting');
      await sleep(backoff * (0.8 + Math.random() * 0.4), ctrl.signal);
    }
  };
  void run();
  return () => ctrl.abort();
}
