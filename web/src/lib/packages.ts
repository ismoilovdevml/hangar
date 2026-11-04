import type { ComponentDetail, ScanState, ScanSummary, SearchHit } from '../api/types';
import type { CodeLine } from './clientSnippets';
import type { Tone } from './format';
import { componentName } from './overview';

/** A search hit's package name in its format's idiom (older servers send only coordinates). */
export const hitName = (h: SearchHit) => componentName({ format: h.format, namespace: h.namespace, name: h.name ?? h.coordinates });

/** Raw files have no version; show a dash instead of an empty pill. */
export const hasVersion = (v: string | undefined): v is string => !!v && v.length > 0;

/**
 * Asset links point at the server's base URL. The web UI is served by the same
 * server, so a same-origin path carries the session cookie and downloads with
 * the signed-in user's permissions; other URLs are left as they are.
 */
export function downloadHref(url: string): string {
  try {
    const u = new URL(url);
    if (u.pathname.startsWith('/repository/')) return `${u.pathname}${u.search}`;
  } catch {
    // relative already
  }
  return url;
}

/**
 * The security state as a pill, with the worst severity spelled out. Search
 * hits carry only the state; "pending" also covers servers without a scanner.
 */
export function securityPill(scan: ScanSummary | ScanState): { label: string; tone: Tone } {
  const s: ScanSummary = typeof scan === 'string' ? { state: scan, critical: 0, high: 0, medium: 0, low: 0 } : scan;
  switch (s.state) {
    case 'clean':
      return { label: 'Clean', tone: 'ok' };
    case 'pending':
      return { label: 'Not scanned', tone: 'neutral' };
    case 'quarantined':
      return { label: 'Quarantined', tone: 'bad' };
    case 'findings':
      if (s.critical) return { label: `${s.critical} critical`, tone: 'bad' };
      if (s.high) return { label: `${s.high} high`, tone: 'warn' };
      if (s.medium) return { label: `${s.medium} medium`, tone: 'warn' };
      if (s.low) return { label: `${s.low} low`, tone: 'neutral' };
      return { label: 'Findings', tone: 'warn' };
  }
}

export interface InstallOption {
  id: string;
  label: string;
  lines: CodeLine[];
}

const withSlash = (u: string) => (u.endsWith('/') ? u : `${u}/`);

/**
 * Install commands for each common client of the package's format. The first
 * option is the server's own command; the others are built from the same
 * repository URL. Repository URL is the one the API reports (server.base_url).
 */
export function installOptions(c: ComponentDetail, repoUrl: string | undefined): InstallOption[] {
  const server: InstallOption = { id: 'default', label: SERVER_LABEL[c.format] ?? 'Command', lines: [{ text: c.install_command }] };
  if (!repoUrl) return [server];
  const url = withSlash(repoUrl);
  const v = c.version;
  switch (c.format) {
    case 'npm': {
      const npmFull = componentName(c);
      return [
        server,
        { id: 'pnpm', label: 'pnpm', lines: [{ text: `pnpm add ${npmFull}@${v} --registry ${url}` }] },
        {
          id: 'yarn',
          label: 'Yarn',
          lines: [{ text: `yarn config set npmRegistryServer ${url}` }, { text: `yarn add ${npmFull}@${v}` }],
        },
      ];
    }
    case 'pypi':
      return [
        server,
        { id: 'uv', label: 'uv', lines: [{ text: `uv pip install ${c.name}==${v} --index-url ${url}simple/` }] },
        {
          id: 'poetry',
          label: 'Poetry',
          lines: [
            { text: `poetry source add --priority=explicit hangar ${url}simple/` },
            { text: `poetry add --source hangar ${c.name}==${v}` },
          ],
        },
      ];
    case 'maven':
      return [
        server,
        {
          id: 'gradle',
          label: 'Gradle',
          lines: [
            { text: 'repositories {' },
            { text: `    maven { url = uri("${url}") }` },
            { text: '}' },
            { text: 'dependencies {' },
            { text: `    implementation("${c.namespace}:${c.name}:${v}")` },
            { text: '}' },
          ],
        },
        {
          id: 'pom',
          label: 'pom.xml',
          lines: [
            { text: '<dependency>' },
            { text: `  <groupId>${c.namespace}</groupId>` },
            { text: `  <artifactId>${c.name}</artifactId>` },
            { text: `  <version>${v}</version>` },
            { text: '</dependency>' },
          ],
        },
      ];
    case 'nuget':
      return [
        server,
        {
          id: 'csproj',
          label: 'PackageReference',
          lines: [{ text: `<PackageReference Include="${c.name}" Version="${v}" />` }],
        },
      ];
    case 'docker':
      return c.install_command.startsWith('docker pull ')
        ? [server, { id: 'podman', label: 'Podman', lines: [{ text: c.install_command.replace(/^docker pull /, 'podman pull ') }] }]
        : [server];
    case 'raw': {
      const path = c.namespace ? `${c.namespace}/${c.name}` : c.name;
      return [server, { id: 'wget', label: 'wget', lines: [{ text: `wget ${url}${path.replace(/^\/+/, '')}` }] }];
    }
    default:
      return [server];
  }
}

/* ---------------------------------------------------------------- browse tabs */

export const BROWSE_TABS = ['readme', 'versions', 'files', 'repositories', 'dependents', 'security'] as const;
export type BrowseTab = (typeof BROWSE_TABS)[number];
export const isBrowseTab = (v: unknown): v is BrowseTab => BROWSE_TABS.includes(v as BrowseTab);

const SERVER_LABEL: Partial<Record<string, string>> = {
  npm: 'npm',
  pypi: 'pip',
  maven: 'Maven',
  nuget: 'dotnet',
  docker: 'Docker',
  raw: 'curl',
  helm: 'Helm',
  go: 'Go',
  cargo: 'Cargo',
  apt: 'APT',
};
