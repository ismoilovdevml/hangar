import { describe, expect, it } from 'vitest';
import type { ComponentDetail, Repository } from '../api/types';
import { downloadHref, installOptions, securityPill } from './packages';
import { isDirty, repoClientConfig, repoToForm, storedInput, toRepositoryUpdate } from './repoSettings';
import { validateRepoForm } from './repositories';

const proxy: Repository = {
  id: 'r1',
  name: 'npmjs-proxy',
  format: 'npm',
  type: 'proxy',
  description: '',
  blob_store: 'default',
  online: true,
  config: {
    proxy: {
      remote_url: 'https://registry.npmjs.org',
      metadata_ttl: '30m0s',
      content_ttl: '0s',
      negative_cache_ttl: '10m0s',
      auth: { kind: 'bearer' },
      http: { proxy: 'http://xxxxx:xxxxx@proxy:3128', insecure_skip_verify: true, read_timeout: '30s' },
      circuit_failures: 5,
    },
    cleanup_policies: ['keep-20', 'snapshots'],
    connector: { host: 'npm.example.com' },
    format: { strict: true },
    scan_on_arrival: true,
  },
  url: 'https://pkg.example.com/repository/npmjs-proxy/',
  status: 'online',
  size_bytes: 1,
  component_count: 1,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
};

describe('repository settings round-trip', () => {
  it('pre-fills the form and sends back an unchanged repository unchanged', () => {
    const f = repoToForm(proxy);
    expect(f).toMatchObject({ name: 'npmjs-proxy', authKind: 'bearer', proxyMode: 'url', proxyUrl: 'http://xxxxx:xxxxx@proxy:3128', cleanupPolicy: 'keep-20' });
    expect(isDirty(f, proxy)).toBe(false);
    const body = toRepositoryUpdate(f, proxy);
    // Settings the form does not edit survive; the stored secret is not sent (the server keeps it).
    expect(body.config.connector).toEqual({ host: 'npm.example.com' });
    expect(body.config.format).toEqual({ strict: true });
    expect(body.config.cleanup_policies).toEqual(['keep-20', 'snapshots']);
    expect(body.config.proxy).toMatchObject({ circuit_failures: 5, auth: { kind: 'bearer' } });
    expect(body.config.proxy?.auth).not.toHaveProperty('token');
    expect(body.config.proxy?.http).toMatchObject({ proxy: 'http://xxxxx:xxxxx@proxy:3128', insecure_skip_verify: true, read_timeout: '30s' });
    expect(body.blob_store).toBe('default');
  });

  it('does not require the stored token again unless the credentials change', () => {
    const f = repoToForm(proxy);
    expect(validateRepoForm(f, { storedAuth: proxy.config.proxy?.auth })).toEqual({});
    expect(validateRepoForm({ ...f, authKind: 'header', authHeader: 'Private-Token' }, { storedAuth: proxy.config.proxy?.auth }).authToken).toBeDefined();
  });

  it('clears a description and replaces the first cleanup policy', () => {
    const r = { ...proxy, description: 'old' };
    const body = toRepositoryUpdate({ ...repoToForm(r), description: '', cleanupPolicy: 'other' }, r);
    expect(body.description).toBe('');
    expect(body.config.cleanup_policies).toEqual(['other', 'snapshots']);
  });

  it('flips only the online flag for the offline switch', () => {
    const body = storedInput(proxy, false);
    expect(body.online).toBe(false);
    expect(body.config.proxy?.metadata_ttl).toBe('30m0s');
  });

  it('writes client configuration, with publish steps for hosted repositories', () => {
    expect(repoClientConfig(proxy).map((l) => l.text)).toContain('registry=https://pkg.example.com/repository/npmjs-proxy/');
    expect(repoClientConfig(proxy).map((l) => l.text)).not.toContain('# Publish');
    const hosted: Repository = { ...proxy, name: 'npm-hosted', type: 'hosted', url: 'https://pkg.example.com/repository/npm-hosted/', config: { hosted: { write_policy: 'allow_once' } } };
    const text = repoClientConfig(hosted).map((l) => l.text);
    expect(text).toContain('npm publish --registry https://pkg.example.com/repository/npm-hosted/');
    expect(text.join('\n')).not.toMatch(/hgr_/);
    const docker: Repository = { ...hosted, format: 'docker', url: 'cr.example.com/docker-hosted' };
    expect(repoClientConfig(docker, 'dilshod').map((l) => l.text)).toEqual(
      expect.arrayContaining([
        "echo '<token>' | docker login cr.example.com -u dilshod --password-stdin",
        'docker push cr.example.com/docker-hosted/<image>:<tag>',
      ]),
    );
  });
});

const detail = (over: Partial<ComponentDetail>): ComponentDetail => ({
  id: 'c', format: 'npm', namespace: '@acme', name: 'ui', version: '1.2.3', repository: 'npm-hosted',
  publisher: { kind: 'user', display_name: 'admin' }, published_at: '2026-01-01T00:00:00Z', scan: { state: 'pending', critical: 0, high: 0, medium: 0, low: 0 },
  size_bytes: 1, checksums: {}, install_command: 'npm install @acme/ui@1.2.3 --registry https://pkg/repository/npm-hosted/', signed: false, quarantined: false,
  ...over,
});

describe('package helpers', () => {
  it('offers the server command first, then other clients of the format', () => {
    const npm = installOptions(detail({}), 'https://pkg/repository/npm-hosted/');
    expect(npm.map((o) => o.label)).toEqual(['npm', 'pnpm', 'Yarn']);
    expect(npm[1]!.lines[0]!.text).toBe('pnpm add @acme/ui@1.2.3 --registry https://pkg/repository/npm-hosted/');
    const mvn = installOptions(detail({ format: 'maven', namespace: 'com.acme', name: 'core', install_command: 'mvn x' }), 'https://pkg/repository/m/');
    expect(mvn.find((o) => o.id === 'gradle')!.lines.map((l) => l.text)).toContain('    implementation("com.acme:core:1.2.3")');
    expect(installOptions(detail({ format: 'go', install_command: 'go get x' }), 'u').map((o) => o.id)).toEqual(['default']);
    expect(installOptions(detail({}), undefined)).toHaveLength(1);
  });

  it('downloads through this origin so the session cookie applies', () => {
    expect(downloadHref('http://localhost:8105/repository/npm-hosted/a.tgz')).toBe('/repository/npm-hosted/a.tgz');
    expect(downloadHref('https://cdn.example.com/a.tgz')).toBe('https://cdn.example.com/a.tgz');
  });

  it('spells out the worst severity', () => {
    expect(securityPill({ state: 'findings', critical: 0, high: 2, medium: 1, low: 0 })).toEqual({ label: '2 high', tone: 'warn' });
    expect(securityPill('pending').label).toBe('Not scanned');
  });
});
