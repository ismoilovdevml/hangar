import type { SearchSort } from '../api/types';

/**
 * The search box's query language, kept in one string (the `q` URL parameter)
 * so the box, the facets and shared links always agree. Mirrors the server's
 * parser (internal/search/parse.go): whitespace-separated tokens; `key:value`
 * with a known key is a qualifier, everything else is free text.
 */

export type QualifierKey = 'format' | 'repo' | 'sha256' | 'cve' | 'license' | 'security' | 'unused' | 'published';

export const QUALIFIERS: { key: QualifierKey; hint: string; example: string }[] = [
  { key: 'format', hint: 'Package format', example: 'format:npm' },
  { key: 'repo', hint: 'Repository name', example: 'repo:npm-hosted' },
  { key: 'sha256', hint: 'Digest prefix, 4–64 hex digits', example: 'sha256:9f2c' },
  { key: 'cve', hint: 'Advisory id', example: 'cve:CVE-2021-44228' },
  { key: 'license', hint: 'Licence (* wildcard)', example: 'license:MIT' },
  { key: 'security', hint: 'pending, clean, findings, quarantined', example: 'security:findings' },
  { key: 'unused', hint: 'Not downloaded for (h, d, w)', example: 'unused:>90d' },
  { key: 'published', hint: 'Before, after or on a date', example: 'published:<2026-01-01' },
];

const KEYS = new Set<string>(QUALIFIERS.map((q) => q.key));

export const SECURITY_STATES = ['pending', 'clean', 'findings', 'quarantined', 'failed', 'unsupported'] as const;
export type SecurityState = (typeof SECURITY_STATES)[number];

export interface Token {
  text: string;
  start: number;
  end: number;
  key?: QualifierKey;
  value?: string;
}

function parseToken(text: string, start: number): Token {
  const colon = text.indexOf(':');
  const key = colon > 0 ? text.slice(0, colon).toLowerCase() : '';
  return KEYS.has(key)
    ? { text, start, end: start + text.length, key: key as QualifierKey, value: text.slice(colon + 1) }
    : { text, start, end: start + text.length };
}

export function tokenize(q: string): Token[] {
  const out: Token[] = [];
  const re = /\S+/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(q))) out.push(parseToken(m[0], m.index));
  return out;
}

/** Values of one qualifier, in order (`format:npm format:maven` → ["npm", "maven"]). */
export function qualifierValues(q: string, key: QualifierKey): string[] {
  return tokenize(q)
    .filter((t) => t.key === key && t.value)
    .map((t) => (key === 'format' || key === 'security' ? t.value!.toLowerCase() : t.value!));
}

/** Replaces every `key:` token with one token per value, appended at the end. */
export function setQualifier(q: string, key: QualifierKey, values: string[]): string {
  const kept = tokenize(q)
    .filter((t) => t.key !== key)
    .map((t) => t.text);
  return [...kept, ...values.map((v) => `${key}:${v}`)].join(' ');
}

export function toggleQualifierValue(q: string, key: QualifierKey, value: string): string {
  const current = qualifierValues(q, key);
  const next = current.includes(value) ? current.filter((v) => v !== value) : [...current, value];
  return setQualifier(q, key, next);
}

/** The free-text part, as the server matches it. */
export function freeText(q: string): string {
  return tokenize(q)
    .filter((t) => !t.key)
    .map((t) => t.text)
    .join(' ');
}

/** Lower-case letter/digit runs of the free text: what results are highlighted with. */
export function highlightTerms(q: string): string[] {
  const pieces = freeText(q)
    .toLowerCase()
    .split(/[^\p{L}\p{N}]+/u)
    .filter(Boolean);
  // Longest first, so "ledger" wins over "led" at the same position.
  return [...new Set(pieces)].sort((a, b) => b.length - a.length);
}

export interface Segment {
  text: string;
  match: boolean;
}

/** Splits `text` into matched and unmatched runs (case-insensitive, non-overlapping). */
export function highlight(text: string, terms: string[]): Segment[] {
  if (!terms.length || !text) return [{ text, match: false }];
  const lower = text.toLowerCase();
  const marks = new Array<boolean>(text.length).fill(false);
  for (const term of terms) {
    let i = lower.indexOf(term);
    while (i >= 0) {
      for (let k = i; k < i + term.length; k++) marks[k] = true;
      i = lower.indexOf(term, i + term.length);
    }
  }
  const out: Segment[] = [];
  for (let i = 0; i < text.length; i++) {
    const last = out[out.length - 1];
    if (last && last.match === marks[i]) last.text += text[i];
    else out.push({ text: text[i]!, match: marks[i]! });
  }
  return out;
}

/* ---------------------------------------------------------------- suggestions */

export interface Suggestion {
  /** What the token under the caret becomes. */
  insert: string;
  label: string;
  hint?: string;
  /** Completing a key keeps the caret right after the colon; values add a space. */
  partial: boolean;
}

export interface SuggestContext {
  formats: string[];
  repositories: string[];
  today?: Date;
}

/** The token the caret is in (or just after); null between tokens. */
export function tokenAt(q: string, caret: number): Token | null {
  return tokenize(q).find((t) => caret >= t.start && caret <= t.end) ?? null;
}

const MAX_SUGGESTIONS = 8;

export function suggest(q: string, caret: number, ctx: SuggestContext): Suggestion[] {
  const tok = tokenAt(q, caret);
  if (!tok) return [];
  if (tok.key) {
    const v = (tok.value ?? '').toLowerCase();
    const values = (list: string[], hint?: (s: string) => string) =>
      list
        .filter((x) => x.toLowerCase().startsWith(v) && x.toLowerCase() !== v)
        .slice(0, MAX_SUGGESTIONS)
        .map((x) => ({ insert: `${tok.key}:${x}`, label: `${tok.key}:${x}`, hint: hint?.(x), partial: false }));
    switch (tok.key) {
      case 'format':
        return values(ctx.formats);
      case 'repo':
        return values(ctx.repositories);
      case 'security':
        return values([...SECURITY_STATES]);
      case 'unused':
        return values(['>30d', '>90d', '>180d', '>365d'], (x) => `not downloaded for ${x.slice(1, -1)} days`);
      case 'published': {
        const d = (ctx.today ?? new Date()).toISOString().slice(0, 10);
        return values([`<${d}`, `>${d}`, d], (x) => (x.startsWith('<') ? 'before' : x.startsWith('>') ? 'after' : 'on'));
      }
      default:
        return [];
    }
  }
  const t = tok.text.toLowerCase();
  return QUALIFIERS.filter((qq) => qq.key.startsWith(t) && t.length > 0)
    .map((qq) => ({ insert: `${qq.key}:`, label: `${qq.key}:`, hint: qq.hint, partial: true }));
}

/** Applies a suggestion to the token at the caret; returns the new query and caret. */
export function applySuggestion(q: string, caret: number, s: Suggestion): { q: string; caret: number } {
  const tok = tokenAt(q, caret);
  if (!tok) return { q, caret };
  const before = q.slice(0, tok.start);
  const rest = q.slice(tok.end);
  if (s.partial) return { q: before + s.insert + rest, caret: before.length + s.insert.length };
  const sep = rest.startsWith(' ') ? '' : ' ';
  return { q: before + s.insert + sep + rest, caret: before.length + s.insert.length + 1 };
}

export const SEARCH_SORTS: { id: SearchSort; label: string }[] = [
  { id: 'relevance', label: 'Best match' },
  { id: '-downloads', label: 'Most downloaded' },
  { id: '-published_at', label: 'Newest' },
];
export const isSearchSort = (v: unknown): v is SearchSort => SEARCH_SORTS.some((s) => s.id === v);

/** Normalises whitespace so equal queries share one URL and cache entry. */
export const normalizeQuery = (q: string) => q.trim().split(/\s+/).filter(Boolean).join(' ');

/* ---------------------------------------------------------------- query problems */

export interface QueryProblem {
  /** The token as typed. */
  token: string;
  /** error: the server refuses the query; warning: it runs, but probably not as meant. */
  level: 'error' | 'warning';
  message: string;
}

const example = (key: QualifierKey) => QUALIFIERS.find((q) => q.key === key)!.example;

/** The known qualifier closest to a mistyped key ("fromat" → format), if any is close. */
function closestKey(key: string): QualifierKey | null {
  const k = key.toLowerCase();
  let best: QualifierKey | null = null;
  let bestScore = Infinity;
  for (const q of QUALIFIERS) {
    const d = editDistance(k, q.key);
    if (d < bestScore) {
      bestScore = d;
      best = q.key;
    }
  }
  return bestScore <= 2 ? best : null;
}

function editDistance(a: string, b: string): number {
  const row = Array.from({ length: b.length + 1 }, (_, i) => i);
  for (let i = 1; i <= a.length; i++) {
    let prev = row[0]!;
    row[0] = i;
    for (let j = 1; j <= b.length; j++) {
      const cur = row[j]!;
      row[j] = Math.min(row[j]! + 1, row[j - 1]! + 1, prev + (a[i - 1] === b[j - 1] ? 0 : 1));
      prev = cur;
    }
  }
  return row[b.length]!;
}

/**
 * Every problem in a query, in plain words, the way the server's parser sees
 * it (errors), plus what it would silently take as free text (warnings): an
 * unknown `word:` key, a format this server does not have.
 */
export function queryProblems(q: string, ctx: { formats?: string[] } = {}): QueryProblem[] {
  const out: QueryProblem[] = [];
  for (const t of tokenize(q)) {
    if (!t.key) {
      const m = /^([a-z][a-z0-9_-]*):(.*)$/i.exec(t.text);
      // "word:" with an unknown key is searched as text; "https://…" and "a:b:c" coordinates are left alone.
      if (m && !m[2]!.startsWith('//') && !m[2]!.includes(':')) {
        const near = closestKey(m[1]!);
        out.push({
          token: t.text,
          level: 'warning',
          message: `${m[1]}: is not a search qualifier, so “${t.text}” is searched as text${near ? `. Did you mean ${near}:?` : '.'}`,
        });
      }
      continue;
    }
    const v = t.value ?? '';
    if (v === '') {
      out.push({ token: t.text, level: 'error', message: `${t.key}: needs a value, e.g. ${example(t.key)}.` });
      continue;
    }
    switch (t.key) {
      case 'format':
        if (ctx.formats?.length && !ctx.formats.includes(v.toLowerCase())) {
          out.push({ token: t.text, level: 'warning', message: `${v} is not a format of this server (${ctx.formats.join(', ')}).` });
        }
        break;
      case 'sha256':
        if (!/^[0-9a-f]{4,64}$/i.test(v)) out.push({ token: t.text, level: 'error', message: `sha256: takes 4 to 64 hex digits (0-9, a-f), e.g. ${example('sha256')}.` });
        break;
      case 'security':
        if (!(SECURITY_STATES as readonly string[]).includes(v.toLowerCase())) {
          out.push({ token: t.text, level: 'error', message: `security: is one of ${SECURITY_STATES.join(', ')}.` });
        }
        break;
      case 'unused': {
        const m = /^>?(\d+)([hdw])$/i.exec(v);
        if (!m || Number(m[1]) <= 0 || Number(m[1]) > 36500) {
          out.push({ token: t.text, level: 'error', message: `unused: takes a number of hours, days or weeks, e.g. ${example('unused')}.` });
        }
        break;
      }
      case 'published': {
        const d = v.replace(/^[<>]/, '');
        if (!/^\d{4}-\d{2}-\d{2}$/.test(d) || Number.isNaN(new Date(`${d}T00:00:00Z`).getTime())) {
          out.push({ token: t.text, level: 'error', message: `published: takes a date, optionally with < or >, e.g. ${example('published')}.` });
        }
        break;
      }
    }
  }
  return out;
}
