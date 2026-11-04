import type { Repository, RepositoryInput } from '../api/types';
import { clientLines, type ClientTarget, type CodeLine } from './clientSnippets';
import { initialRepoForm, toRepositoryInput, type ProxyMode, type RepoForm } from './repositories';

/** The settings form pre-filled from a repository as the API returns it (secrets are never returned). */
export function repoToForm(r: Repository): RepoForm {
  const c = r.config;
  const p = c.proxy;
  const http = p?.http;
  const proxyMode: ProxyMode = !http?.proxy || http.proxy === 'inherit' ? 'inherit' : http.proxy === 'none' ? 'none' : 'url';
  return {
    ...initialRepoForm(r.format, r.type),
    name: r.name,
    description: r.description ?? '',
    blobStore: r.blob_store ?? '',
    writePolicy: c.hosted?.write_policy ?? 'allow',
    remoteUrl: p?.remote_url ?? '',
    metadataTtl: p?.metadata_ttl ?? '',
    contentTtl: p?.content_ttl ?? '',
    negativeTtl: p?.negative_cache_ttl ?? '',
    authKind: p?.auth?.kind ?? 'none',
    authUsername: p?.auth?.username ?? '',
    authHeader: p?.auth?.header_name ?? '',
    proxyMode,
    proxyUrl: proxyMode === 'url' ? (http?.proxy ?? '') : '',
    caBundle: (http?.ca_bundle ?? '').trimEnd(),
    members: c.group?.members ? [...c.group.members] : [],
    writeMember: c.group?.write_member ?? '',
    cleanupPolicy: c.cleanup_policies?.[0] ?? '',
    scanOnArrival: !!c.scan_on_arrival,
    quarantineCritical: !!c.quarantine_critical,
    anonymousRead: !!c.anonymous_read,
  };
}

/**
 * The PUT body: the form's settings on top of the stored repository, so
 * settings this form does not edit (connector, format-specific options, TLS
 * and timeout overrides, circuit breaker, extra cleanup policies) survive.
 * Name, format, type and blob store are immutable and always sent as stored.
 * Empty secrets are left out: the server keeps the stored ones.
 */
export function toRepositoryUpdate(f: RepoForm, r: Repository, online: boolean = r.online ?? true): RepositoryInput {
  const base = toRepositoryInput({ ...f, name: r.name, format: r.format, type: r.type, blobStore: r.blob_store ?? '' });
  const old = r.config;
  const config = { ...base.config };
  if (old.connector) config.connector = old.connector;
  if (old.format) config.format = old.format;

  const extraPolicies = (old.cleanup_policies ?? []).slice(1);
  const policies = [...new Set([...(f.cleanupPolicy ? [f.cleanupPolicy] : []), ...extraPolicies])];
  if (policies.length) config.cleanup_policies = policies;
  else delete config.cleanup_policies;

  if (config.proxy) {
    const p = { ...config.proxy };
    const op = old.proxy;
    if (op?.circuit_failures !== undefined) p.circuit_failures = op.circuit_failures;
    if (op?.circuit_open_for) p.circuit_open_for = op.circuit_open_for;
    const oh = op?.http;
    if (oh && (oh.insecure_skip_verify !== undefined || oh.connect_timeout || oh.read_timeout)) {
      p.http = {
        ...(p.http ?? {}),
        ...(oh.insecure_skip_verify !== undefined ? { insecure_skip_verify: oh.insecure_skip_verify } : {}),
        ...(oh.connect_timeout ? { connect_timeout: oh.connect_timeout } : {}),
        ...(oh.read_timeout ? { read_timeout: oh.read_timeout } : {}),
      };
    }
    if (p.auth) {
      const a = { ...p.auth };
      if (!a.password) delete a.password;
      if (!a.token) delete a.token;
      p.auth = a;
    }
    config.proxy = p;
  }

  const input: RepositoryInput = {
    name: r.name,
    format: r.format,
    type: r.type,
    description: f.description.trim(),
    online,
    config,
  };
  if (r.blob_store) input.blob_store = r.blob_store;
  return input;
}

/** The stored settings as an update body: what the online/offline switch sends. */
export const storedInput = (r: Repository, online: boolean) => toRepositoryUpdate(repoToForm(r), r, online);

/** Whether the form differs from the stored repository. */
export function isDirty(f: RepoForm, r: Repository): boolean {
  const a = toRepositoryUpdate(f, r);
  const b = toRepositoryUpdate(repoToForm(r), r);
  return JSON.stringify(a) !== JSON.stringify(b) || !!f.authPassword || !!f.authToken;
}

/** Whether clients can upload through this repository. */
export const acceptsUploads = (r: Repository) => r.type === 'hosted' || (r.type === 'group' && !!r.config.group?.write_member);

/** The client target for one repository, built from its server-provided URL. */
export const repoClientTarget = (r: Repository, username?: string): ClientTarget => ({
  format: r.format,
  repository: r.name,
  url: r.url,
  type: r.type,
  publishes: acceptsUploads(r),
  username,
});

/**
 * Copyable client configuration for one repository: how to read from it, and
 * for hosted repositories (and groups with a write member) how to publish.
 * Credentials are placeholders; tokens come from My account.
 */
export function repoClientConfig(r: Repository, username?: string): CodeLine[] {
  return clientLines(repoClientTarget(r, username));
}
