/**
 * Mock routing rules (PUT/DELETE /repositories/{name}/routing,
 * POST .../routing:test, GET .../routing:suggest), following
 * internal/core/routing.go: proxies take allow/block, groups per-member
 * exclusive/block, block wins, exclusive patterns reserve names for the
 * members listing them.
 */
import { http, HttpResponse } from 'msw';
import type { ApiError, Repository, RoutingBlock, RoutingRules, RoutingTest } from '../api/types';
import { allows } from '../lib/can';
import { session } from './data';

const api = (path: string) => `*/api/v1${path}`;
type Guard = (request: Request, mutating?: boolean) => Response | null;

const error = (status: number, body: ApiError) => HttpResponse.json(body, { status });

/** The name as the rules compare it (core.RoutingName). */
export function routingName(format: string, name: string): string {
  if (format === 'nuget') return name.toLowerCase();
  if (format === 'pypi') return name.toLowerCase().replace(/[-_.]+/g, '-');
  if (format === 'cargo') return name.toLowerCase().replace(/_/g, '-');
  return name;
}

function glob(pattern: string, name: string): boolean {
  const re = pattern.replace(/[.+^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*').replace(/\?/g, '.');
  return new RegExp(`^${re}$`, 's').test(name);
}

/** core.MatchRoutingPattern. */
export function matchPattern(format: string, pattern: string, name: string): boolean {
  if (format === 'go') {
    const p = pattern.replace(/\/$/, '');
    const n = p.split('/').length;
    const elems = name.split('/');
    return elems.length >= n && glob(p.replace(/\*/g, '[^/]*'), elems.slice(0, n).join('/'));
  }
  const p = routingName(format, pattern);
  let s = routingName(format, name);
  if (format === 'maven' && !p.includes(':')) s = s.split(':')[0]!;
  return glob(p, s);
}

const first = (format: string, ps: readonly string[] | undefined, name: string) => (ps ?? []).find((p) => matchPattern(format, p, name));

function proxyBlock(r: Repository, name: string): RoutingBlock | undefined {
  const rt = r.config.routing;
  if (r.type !== 'proxy' || !rt) return undefined;
  const b = first(r.format, rt.block, name);
  if (b) return { repository: r.name, rule: 'block', pattern: b, name };
  if (rt.allow?.length && !first(r.format, rt.allow, name)) return { repository: r.name, rule: 'allow', name };
  return undefined;
}

function memberBlock(g: Repository, member: string, name: string): RoutingBlock | undefined {
  const ms = g.config.routing?.members ?? [];
  const own = ms.find((m) => m.member === member);
  const b = first(g.format, own?.block, name);
  if (b) return { repository: g.name, member, rule: 'block', pattern: b, name };
  const owners = ms.filter((m) => first(g.format, m.exclusive, name));
  if (!owners.length || owners.some((m) => m.member === member)) return undefined;
  return { repository: g.name, member, rule: 'exclusive', pattern: first(g.format, owners[0]!.exclusive, name), owners: owners.map((m) => m.member), name };
}

function validate(r: Repository, rules: RoutingRules): string | null {
  const all = [...(rules.allow ?? []), ...(rules.block ?? []), ...(rules.members ?? []).flatMap((m) => [...(m.exclusive ?? []), ...(m.block ?? [])])];
  const bad = all.find((p) => !p.trim() || /\s/.test(p));
  if (bad !== undefined) return `routing pattern ${JSON.stringify(bad)}: must not be empty or contain spaces`;
  if (r.type === 'proxy' && rules.members?.length) return 'routing members apply to group repositories only';
  if (r.type === 'group' && (rules.allow?.length || rules.block?.length)) return 'routing allow and block apply to proxy repositories; groups take per-member rules';
  const members = r.config.group?.members ?? [];
  const stray = (rules.members ?? []).find((m) => !members.includes(m.member));
  if (stray) return `routing rules for "${stray.member}": not a member of the group (remove its rules before removing the member)`;
  return null;
}

export function routingHandlers(guard: Guard, repositories: () => readonly Repository[]) {
  const find = (name: string) => repositories().find((x) => x.name === name);
  const resolve = (request: Request, name: string, admin: boolean): Repository | Response => {
    const denied = guard(request, request.method !== 'GET');
    if (denied) return denied;
    const r = find(name);
    if (!r) return error(404, { code: 'not_found', message: `repository "${name}" not found` });
    if (r.type === 'hosted') {
      return error(400, { code: 'invalid_argument', message: `repository "${name}" is a hosted repository; routing rules apply to proxy and group repositories` });
    }
    if (admin && !allows(session.privileges, 'repo', name, 'admin') && !allows(session.privileges, 'system', 'repos', 'admin')) {
      return error(403, { code: 'forbidden', message: 'permission denied' });
    }
    return r;
  };
  const save = (r: Repository, rules: RoutingRules | null) => {
    const config = { ...r.config };
    if (rules) config.routing = rules;
    else delete config.routing;
    Object.assign(r, { config });
  };

  return [
    http.put(api('/repositories/:name/routing'), async ({ request, params }) => {
      const r = resolve(request, String(params.name), true);
      if (r instanceof Response) return r;
      const rules = (await request.json()) as RoutingRules;
      const msg = validate(r, rules);
      if (msg) return error(400, { code: 'invalid_argument', message: `repository "${r.name}": ${msg}` });
      save(r, rules);
      return HttpResponse.json({ allow: rules.allow ?? [], block: rules.block ?? [], members: rules.members ?? [] });
    }),
    http.delete(api('/repositories/:name/routing'), ({ request, params }) => {
      const r = resolve(request, String(params.name), true);
      if (r instanceof Response) return r;
      save(r, null);
      return new HttpResponse(null, { status: 204 });
    }),
    http.post(api('/repositories/:name/routing\\:test'), async ({ request, params }) => {
      const r = resolve(request, String(params.name), false);
      if (r instanceof Response) return r;
      const { name } = (await request.json()) as { name?: string };
      if (!name) return error(400, { code: 'invalid_argument', message: 'name is required' });
      const out: RoutingTest = { repository: r.name, format: r.format, name, normalized: routingName(r.format, name), allowed: false, members: [] };
      if (r.type === 'proxy') {
        out.block = proxyBlock(r, name);
        out.allowed = !out.block;
        return HttpResponse.json(out);
      }
      const walk = (g: Repository, via: string[], inherited: RoutingBlock | undefined, depth: number) => {
        for (const name0 of g.config.group?.members ?? []) {
          const m = find(name0);
          if (!m) continue;
          const block = inherited ?? memberBlock(g, m.name, name) ?? (m.type === 'proxy' ? proxyBlock(m, name) : undefined);
          out.members.push({ repository: m.name, type: m.type, via: [...via, g.name], allowed: !block, online: m.online ?? true, block });
          if (m.type === 'group' && depth < 8) walk(m, [...via, g.name], block, depth + 1);
        }
      };
      walk(r, [], undefined, 0);
      const served = out.members.find((h) => h.allowed && h.online && h.type !== 'group');
      out.allowed = !!served;
      out.served_by = served?.repository;
      return HttpResponse.json(out);
    }),
    http.get(api('/repositories/:name/routing\\:suggest'), ({ request, params }) => {
      const r = resolve(request, String(params.name), true);
      if (r instanceof Response) return r;
      const hosted =
        r.type === 'group'
          ? (r.config.group?.members ?? []).map(find).filter((m): m is Repository => m?.type === 'hosted').map((m) => ({ source: m, member: m.name }))
          : repositories().filter((x) => x.format === r.format && x.type === 'hosted').map((m) => ({ source: m, member: undefined }));
      const pattern = r.format === 'npm' ? '@acme/*' : r.format === 'nuget' ? 'Acme.*' : r.format === 'maven' ? 'io.acme' : r.format === 'go' ? 'git.acme.io/platform' : 'acme-*';
      const suggestions = hosted.map(({ source, member }) => ({ pattern, matches: 3, examples: [pattern.replace('*', 'core')], source: source.name, member }));
      return HttpResponse.json({ repository: r.name, suggestions, truncated: false });
    }),
  ];
}
