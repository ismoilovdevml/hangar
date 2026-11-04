/**
 * Ready-to-paste snippets for a newly created token, with the server's real
 * URL and the secret filled in. Pure functions only.
 */
import type { Repository } from '../../api/types';
import { clientLines, type CodeLine } from '../../lib/clientSnippets';
import { allows } from './model';
import { effectiveScopes } from './tokenScopes';

const c = (text: string): CodeLine => ({ text, comment: true });
const l = (text: string): CodeLine => ({ text });

/** The server's public URL: server.base_url, else the address the UI was loaded from. */
export function serverBaseUrl(baseUrl: string | undefined, origin: string): string {
  return (baseUrl || origin).replace(/\/+$/, '');
}

export function curlSnippet(base: string, secret: string): CodeLine[] {
  return [
    c('# Who is this token, and what may it do?'),
    l(`curl -fsS -H "Authorization: Bearer ${secret}" ${base}/api/v1/auth/session`),
    c('# List the repositories it can read'),
    l(`curl -fsS -H "Authorization: Bearer ${secret}" "${base}/api/v1/repositories?limit=100"`),
  ];
}

export function envSnippet(base: string, secret: string): CodeLine[] {
  return [l(`HANGAR_URL=${base}`), l(`HANGAR_TOKEN=${secret}`)];
}

/** Everything an automated client needs to start: where, how to authenticate, what to read. */
export function automationSnippet(base: string, secret: string): CodeLine[] {
  return [
    c('# Hangar REST API access'),
    l(`HANGAR_URL=${base}`),
    l(`HANGAR_TOKEN=${secret}`),
    c('# Authenticate every request with: Authorization: Bearer $HANGAR_TOKEN'),
    c(`# Start with the guide: ${base}/llms.txt`),
    c(`# Every endpoint: ${base}/api/v1/openapi.yaml`),
  ];
}

export const SNIPPET_FORMATS = ['npm', 'nuget', 'pypi', 'maven', 'docker'] as const;

export interface ClientSnippet {
  format: (typeof SNIPPET_FORMATS)[number];
  repository: string;
  publishes: boolean;
  lines: CodeLine[];
}

const TYPE_ORDER: Record<string, number> = { group: 0, hosted: 1, proxy: 2 };

/**
 * One client configuration per common format, for a repository the token
 * can read: a hosted one it can publish to when there is one, else a group,
 * else any. Formats it cannot reach are left out.
 */
export function clientSnippets(repos: readonly Repository[], scopes: string[], readOnly: boolean, owner: string, secret: string): ClientSnippet[] {
  const eff = effectiveScopes(scopes, readOnly);
  const out: ClientSnippet[] = [];
  for (const format of SNIPPET_FORMATS) {
    const readable = repos
      .filter((r) => r.format === format && r.online !== false && allows(eff, 'repo', r.name, 'read'))
      .sort((a, b) => (TYPE_ORDER[a.type] ?? 9) - (TYPE_ORDER[b.type] ?? 9) || a.name.localeCompare(b.name));
    if (readable.length === 0) continue;
    const writable = readable.find((r) => r.type === 'hosted' && allows(eff, 'repo', r.name, 'write'));
    const pick = writable ?? readable[0]!;
    const publishes = !!writable;
    out.push({
      format,
      repository: pick.name,
      publishes,
      lines: clientLines({ format, repository: pick.name, url: pick.url, type: pick.type, publishes, username: owner, token: secret }),
    });
  }
  return out;
}
