import { ApiRequestError } from '../api/client';

/**
 * Turns failures into words for people. The API answers `{code, message}`;
 * the message is written for operators and may carry a chain of Go error
 * context ("repository "x": remote url …: blocked by …"). The UI leads with a
 * plain sentence chosen by status and code, and keeps the server's own text
 * for a "Details" disclosure.
 */

/** The server's message tidied up: no Go sentinel suffix, no doubled tail, sentence case, a full stop. */
export function cleanServerMessage(msg: string): string {
  let m = msg.trim();
  // Servers before v0.1.9 ended validation errors in the sentinel's text.
  m = m.replace(/: invalid argument$/, '');
  // …and wrapped some sentinels twice ("already exists: already exists").
  const tail = /^(.*): ([^:]+): \2$/.exec(m);
  if (tail) m = `${tail[1]}: ${tail[2]}`;
  if (!m) return '';
  m = m.charAt(0).toUpperCase() + m.slice(1);
  return /[.!?)]$/.test(m) ? m : `${m}.`;
}

export interface ErrorDescription {
  /** One plain sentence about what went wrong. */
  message: string;
  /** What the server or browser said, for the Details disclosure; null when it adds nothing. */
  detail: string | null;
}

/** The raw text behind a failure: "HTTP 409 already_exists: create user "bob": already exists". */
export function rawDetail(err: unknown): string | null {
  if (err instanceof ApiRequestError) {
    const head = `HTTP ${err.status}${err.code ? ` ${err.code}` : ''}`;
    return err.body?.message ? `${head}: ${err.body.message}` : head;
  }
  if (err instanceof Error) return `${err.name}: ${err.message}`;
  return err === undefined || err === null ? null : String(err);
}

/** A sentence by status and code; null when the server's own (cleaned) message says it better. */
function byStatus(err: ApiRequestError): string | null {
  switch (err.code) {
    case 'read_only_token':
      return 'You signed in with a read-only token, which cannot change anything.';
    case 'csrf_failed':
      return 'The page was open too long to make changes safely. Reload it and try again.';
    case 'offline':
      return 'The repository is offline. Bring it online to use it.';
    case 'upstream_unavailable':
      return 'A remote the server depends on did not answer. Try again later.';
    case 'not_implemented':
      return 'This server does not support this yet.';
  }
  switch (err.status) {
    case 401:
      return 'Your session has ended. Sign in again.';
    case 403:
      return 'You do not have permission to do this.';
    case 404:
      return 'It does not exist, or you cannot see it.';
    case 413:
      return 'The upload is larger than the server accepts.';
    case 423:
      return err.retryAfter ? `Locked after repeated wrong passwords. Try again in ${Math.ceil(err.retryAfter / 60)} min.` : 'Locked for now. Try again later.';
    case 429:
      return err.retryAfter ? `Too many requests. Try again in ${err.retryAfter} s.` : 'Too many requests. Try again in a moment.';
    case 499:
      return 'The request was cancelled.';
    case 502:
    case 504:
      return 'The server could not reach a service it depends on. Try again later.';
    case 503:
      return 'The server is not available right now. Try again in a moment.';
  }
  if (err.status >= 500) return `The server answered with an error (${err.status}). The server log has the cause.`;
  return null;
}

/** What went wrong in a plain sentence, plus the raw text for Details. */
export function describeError(err: unknown): ErrorDescription {
  const detail = rawDetail(err);
  if (err instanceof ApiRequestError) {
    if (!err.body) {
      // Not the API's JSON error body: a proxy page, an HTML error, an empty answer.
      return { message: byStatus(err) ?? `The server sent an answer the app cannot read (HTTP ${err.status}).`, detail };
    }
    const own = err.body.message ? cleanServerMessage(err.body.message) : '';
    const plain = byStatus(err);
    // 400, 404 and 409 messages are specific (which field, which name): they lead. Others get a plain sentence.
    if ([400, 404, 409, 422].includes(err.status) && own) return { message: own, detail };
    // A 500 says "internal error" by design; other server-side messages ("database is unavailable",
    // "node hangar-3 does not answer") are worth reading.
    if (err.status >= 500) {
      return { message: own && !/^internal( server)? error\.$/i.test(own) ? own : (plain ?? own), detail };
    }
    return { message: plain ?? (own || `The request failed (HTTP ${err.status}).`), detail };
  }
  if (err instanceof TypeError && /fetch|network|load failed/i.test(err.message)) {
    return { message: 'Cannot reach the server. Check your connection and try again.', detail };
  }
  if (err instanceof SyntaxError) return { message: 'The server sent an answer the app cannot read.', detail };
  if (err instanceof Error) return { message: 'Something went wrong while showing this.', detail };
  return { message: 'Something went wrong.', detail };
}

/** Just the sentence, for places that have room for one line. */
export const errorSentence = (err: unknown) => describeError(err).message;
