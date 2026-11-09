import { describe, expect, it } from 'vitest';
import type { ComponentDetail, Repository } from '../api/types';
import { installCommands } from './clientSnippets';
import { downloadHref, securityPill } from './packages';
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
  last_activity_at: null,
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

describe('format settings round-trip', () => {
  const apt: Repository = {
    ...proxy,
    name: 'apt-hosted',
    format: 'apt',
    type: 'hosted',
    url: 'https://pkg.example.com/repository/apt-hosted/',
    config: { hosted: { write_policy: 'allow_once' }, format: { architectures: ['amd64', 'riscv64'], signing_key: 'repository', future: 1 } },
  };

  it('edits only the keys of its format and type, keeping unknown ones', () => {
    const f = repoToForm(apt);
    expect(f).toMatchObject({ aptArchitectures: ['amd64', 'riscv64'], signingKey: 'repository' });
    const body = toRepositoryUpdate({ ...f, aptArchitectures: [], signingKey: 'shared' }, apt);
    expect(body.config.format).toEqual({ future: 1, signing_key: 'shared' });
  });

  it('keeps settings of another type as stored', () => {
    const hostedGo: Repository = { ...apt, format: 'go', config: { hosted: { write_policy: 'allow_once' }, format: { sumdbs: [] } } };
    expect(toRepositoryUpdate(repoToForm(hostedGo), hostedGo).config.format).toEqual({ sumdbs: [] });
  });

  it('tells a Go proxy without sumdbs (default) from one with an empty list (off)', () => {
    const goProxy: Repository = { ...proxy, name: 'go-proxy', format: 'go', config: { proxy: { remote_url: 'https://proxy.golang.org' } } };
    const f = repoToForm(goProxy);
    expect(f.goSumdbs).toEqual(['sum.golang.org']);
    expect(isDirty(f, goProxy)).toBe(false);
    expect(toRepositoryUpdate(f, goProxy).config.format).toBeUndefined();
    expect(toRepositoryUpdate({ ...f, goSumdbs: [] }, goProxy).config.format).toEqual({ sumdbs: [] });
    const off: Repository = { ...goProxy, config: { ...goProxy.config, format: { sumdbs: [] } } };
    expect(repoToForm(off).goSumdbs).toEqual([]);
  });

  it('keeps proxy related hosts, offline mode and the request limit', () => {
    const r: Repository = { ...proxy, config: { ...proxy.config, proxy: { ...proxy.config.proxy!, related_hosts: ['.cdn.example'], offline: true, max_concurrent_requests: 8 } } };
    expect(toRepositoryUpdate(repoToForm(r), r).config.proxy).toMatchObject({ related_hosts: ['.cdn.example'], offline: true, max_concurrent_requests: 8 });
  });

  it('edits the distribution release of APT and RPM repositories only', () => {
    const r: Repository = { ...apt, config: { ...apt.config, scanning: { distro: 'Debian:12' } } };
    const f = repoToForm(r);
    expect(f.distro).toBe('Debian:12');
    expect(isDirty(f, r)).toBe(false);
    expect(toRepositoryUpdate(f, r).config.scanning).toEqual({ distro: 'Debian:12' });
    expect(toRepositoryUpdate({ ...f, distro: 'Ubuntu:24.04:LTS' }, r).config.scanning).toEqual({ distro: 'Ubuntu:24.04:LTS' });
    expect(toRepositoryUpdate({ ...f, distro: '' }, r).config.scanning).toBeUndefined();
    // Other formats keep whatever is stored.
    const npm: Repository = { ...proxy, config: { ...proxy.config, scanning: { distro: 'x' } } };
    expect(toRepositoryUpdate(repoToForm(npm), npm).config.scanning).toEqual({ distro: 'x' });
    expect(validateRepoForm({ ...f, distro: 'Debian:12' }).distro).toBeUndefined();
    for (const d of ['Rocky Linux:9', 'Debian', 'Debian:', 'deb;12']) expect(validateRepoForm({ ...f, distro: d }).distro).toBeDefined();
    const rpm = { ...f, format: 'rpm' as const };
    for (const d of ['Rocky Linux:9', 'AlmaLinux:9', 'Red Hat:enterprise_linux:9::baseos']) expect(validateRepoForm({ ...rpm, distro: d }).distro).toBeUndefined();
    expect(validateRepoForm({ ...rpm, distro: 'Debian:12' }).distro).toBeDefined();
  });

  it('validates format settings like the server', () => {
    const f = repoToForm(apt);
    expect(validateRepoForm({ ...f, aptArchitectures: ['all'] }).aptArchitectures).toMatch(/Leave out "all"/);
    expect(validateRepoForm({ ...f, aptArchitectures: ['AMD64'] }).aptArchitectures).toBeDefined();
    const rpm = { ...f, format: 'rpm' as const };
    expect(validateRepoForm({ ...rpm, rpmRepodataDepth: '4' }).rpmRepodataDepth).toBeUndefined();
    for (const d of ['5', '-1', '1.5', '']) expect(validateRepoForm({ ...rpm, rpmRepodataDepth: d }).rpmRepodataDepth).toBeDefined();
    const go = { ...repoToForm(proxy), format: 'go' as const };
    expect(validateRepoForm({ ...go, goSumdbs: ['sum.golang.org', 'sum.example.com'] }).goSumdbs).toBeUndefined();
    for (const d of ['Sum.golang.org', '.x', 'a..b', 'https://sum.golang.org']) expect(validateRepoForm({ ...go, goSumdbs: [d] }).goSumdbs).toBeDefined();
    const cargo = { ...go, format: 'cargo' as const };
    expect(validateRepoForm({ ...cargo, cargoDownloadUrl: 'https://dl.example/{crate}/{version}' }).cargoDownloadUrl).toBeUndefined();
    expect(validateRepoForm({ ...cargo, cargoDownloadUrl: '{crate}/{version}' }).cargoDownloadUrl).toBeDefined();
    for (const h of ['charts.example.com', 'host:8443', '.example.org']) expect(validateRepoForm({ ...go, relatedHosts: [h] }).relatedHosts).toBeUndefined();
    for (const h of ['https://x.example', 'x.example/path', 'host:0', '..x', '*.x']) expect(validateRepoForm({ ...go, relatedHosts: [h] }).relatedHosts).toBeDefined();
  });
});

const detail = (over: Partial<ComponentDetail>): ComponentDetail => ({
  id: 'c', format: 'npm', namespace: '@acme', name: 'ui', version: '1.2.3', repository: 'npm-hosted',
  publisher: { kind: 'user', display_name: 'admin' }, published_at: '2026-01-01T00:00:00Z', scan: { state: 'pending', critical: 0, high: 0, medium: 0, low: 0 },
  size_bytes: 1, checksums: {}, install_command: 'npm install @acme/ui@1.2.3 --registry https://pkg/repository/npm-hosted/', signed: false, quarantined: false,
  ...over,
});

describe('package helpers', () => {
  it('installs one exact version with every common client of the format', () => {
    const d = detail({});
    const npm = installCommands({ format: 'npm', url: 'https://pkg/repository/npm-hosted/' }, d);
    expect(npm.map((o) => o.label)).toEqual(['npm', 'pnpm', 'Yarn']);
    expect(npm[1]!.lines[0]!.text).toBe('pnpm add @acme/ui@1.2.3 --registry https://pkg/repository/npm-hosted/');
    const gomod = installCommands({ format: 'go', url: 'https://pkg/repository/go-group/' }, { name: 'example.com/acme/greet', version: 'v1.2.0' });
    expect(gomod.map((c) => c.id)).toEqual(['go', 'gomod']);
    expect(gomod.flatMap((c) => c.lines.map((x) => x.text))).toEqual([
      'GOPROXY=https://pkg/repository/go-group,direct go get example.com/acme/greet@v1.2.0',
      '// go.mod',
      'require example.com/acme/greet v1.2.0',
    ]);
    const mvn = installCommands({ format: 'maven', url: 'https://pkg/repository/m/' }, { namespace: 'com.acme', name: 'core', version: '1.2.3' });
    expect(mvn.find((o) => o.id === 'gradle')!.lines.map((l) => l.text)).toContain('    implementation("com.acme:core:1.2.3")');
    expect(mvn.find((o) => o.id === 'maven')!.lines.map((l) => l.text)).toContain('  <artifactId>core</artifactId>');
    const nuget = installCommands({ format: 'nuget', url: 'https://pkg/repository/n' }, { name: 'Acme.Core', version: '2.1.0' });
    expect(nuget.map((o) => o.label)).toEqual(['dotnet CLI', 'PackageReference']);
    expect(nuget[0]!.lines[0]!.text).toBe('dotnet add package Acme.Core --version 2.1.0 --source https://pkg/repository/n/index.json');
    expect(installCommands({ format: 'pypi', url: 'https://pkg/repository/p/' }, { name: 'six', version: '1.16.0' }).map((o) => o.label)).toEqual(['pip', 'uv']);
    expect(installCommands({ format: 'docker', url: 'cr.example.com/docker-hosted' }, { name: 'app', version: '1.0' })[0]!.lines[0]!.text).toBe(
      'docker pull cr.example.com/docker-hosted/app:1.0',
    );
    const crate = installCommands({ format: 'cargo', url: 'https://pkg/repository/cargo-hosted/', repository: 'cargo-hosted' }, { name: 'serde', version: '1.0.0' });
    expect(crate.map((o) => o.lines.at(-1)!.text)).toEqual(['cargo add serde@1.0.0 --registry cargo-hosted', 'serde = { version = "1.0.0", registry = "cargo-hosted" }']);
    expect(installCommands({ format: 'npm' }, d)).toEqual([]);
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
