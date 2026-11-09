import type { ListRepositoriesQuery } from '../api/types';
import type {
  BlobStore,
  Format,
  Repository,
  RepositoryInput,
  RepositoryStatus,
  RepositoryType,
  UpstreamAuth,
  UpstreamBlockedReason,
  WritePolicy,
} from '../api/types';
import { formatBytes, type Tone } from './format';
import { toYaml, YamlComment, type YamlValue } from './yaml';

export const ALL_FORMATS: Format[] = ['docker', 'npm', 'maven', 'pypi', 'go', 'helm', 'nuget', 'cargo', 'apt', 'rpm', 'raw'];
export const REPO_TYPES: RepositoryType[] = ['hosted', 'proxy', 'group'];

export const isFormat = (v: unknown): v is Format => ALL_FORMATS.includes(v as Format);
export const isRepoType = (v: unknown): v is RepositoryType => REPO_TYPES.includes(v as RepositoryType);

/** Orders the repository list supports; a leading "-" reverses. */
export type RepoSort = NonNullable<ListRepositoriesQuery['sort']>;
export type RepoSortKey = 'name' | 'type' | 'format' | 'last_activity';
const REPO_SORTS: RepoSort[] = ['name', '-name', 'type', '-type', 'format', '-format', 'last_activity', '-last_activity'];
export const isRepoSort = (v: unknown): v is RepoSort => REPO_SORTS.includes(v as RepoSort);

/** One-line hint under each format in the picker. */
export const FORMAT_HINT: Record<Format, string> = {
  docker: 'OCI images',
  npm: 'JavaScript',
  maven: 'Java, Kotlin',
  pypi: 'Python',
  go: 'Go modules',
  helm: 'Charts',
  nuget: '.NET',
  cargo: 'Rust',
  apt: 'Debian, Ubuntu',
  rpm: 'RHEL, Rocky, Fedora',
  raw: 'Any file',
};

/** Public upstream each proxy starts from; the user can change it. */
export const DEFAULT_REMOTE: Record<Format, string> = {
  docker: 'https://registry-1.docker.io',
  npm: 'https://registry.npmjs.org',
  maven: 'https://repo1.maven.org/maven2/',
  pypi: 'https://pypi.org',
  go: 'https://proxy.golang.org',
  helm: '',
  nuget: 'https://api.nuget.org',
  cargo: 'https://index.crates.io',
  apt: 'http://deb.debian.org/debian',
  rpm: 'https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os/',
  raw: '',
};

/** Formats whose clients can publish through a group (it forwards writes to one hosted member). */
export const GROUP_WRITE_FORMATS: Format[] = ['docker', 'npm', 'cargo'];

export const TYPE_META: Record<RepositoryType, { label: string; tone: Tone; explain: string; card: string }> = {
  hosted: {
    label: 'Hosted',
    tone: 'info',
    explain: 'Your team publishes here. Packages never leave your servers.',
    card: 'your own packages',
  },
  proxy: {
    label: 'Proxy',
    tone: 'peach',
    explain: 'Caches a public registry so builds stay fast and keep working offline.',
    card: 'cached upstreams',
  },
  group: {
    label: 'Group',
    tone: 'violet',
    explain: 'Combines several repositories behind one URL for clients.',
    card: 'one URL for many',
  },
};

export const STATUS_META: Record<RepositoryStatus, { label: string; tone: 'ok' | 'warn' | 'bad' | 'neutral' }> = {
  online: { label: 'Online', tone: 'ok' },
  slow_upstream: { label: 'Slow upstream', tone: 'warn' },
  blocked: { label: 'Blocked by policy', tone: 'bad' },
  offline: { label: 'Offline', tone: 'neutral' },
};

/** A remote that refuses the proxy's credentials (401 or 403), as opposed to one that cannot be reached. */
export function upstreamAuthProblem(reason: UpstreamBlockedReason | undefined): { label: string; hint: string } | null {
  if (reason === 'auth_failed') return { label: 'authentication failed (401)', hint: 'check the proxy credentials' };
  if (reason === 'access_denied') return { label: 'access denied (403)', hint: 'check the proxy credentials' };
  return null;
}

/** The status badge of a repository; a credentials problem is named as such. */
export function repoStatusMeta(r: Pick<Repository, 'status' | 'status_reason'>): {
  label: string;
  tone: 'ok' | 'warn' | 'bad' | 'neutral';
  hint?: string;
} {
  const auth = r.status === 'blocked' ? upstreamAuthProblem(r.status_reason) : null;
  if (auth) return { label: auth.label.charAt(0).toUpperCase() + auth.label.slice(1), tone: 'bad', hint: 'Check the proxy credentials.' };
  return STATUS_META[r.status];
}

export const WRITE_POLICY_LABEL: Record<WritePolicy, string> = {
  allow: 'Allow — re-uploads replace a version',
  allow_once: 'Block — versions are immutable',
  deny: 'Read-only — no uploads',
};

/** Second line under a repository name: its description, else what it points at. */
export function repoSubtitle(r: Repository): string {
  if (r.description) return r.description;
  if (r.type === 'proxy' && r.config.proxy?.remote_url) {
    try {
      return new URL(r.config.proxy.remote_url).host;
    } catch {
      return r.config.proxy.remote_url;
    }
  }
  if (r.type === 'group' && r.config.group?.members.length) return r.config.group.members.join(' + ');
  return '';
}

/** The repository URL as shown in the table: without scheme and host when it is this server's. */
export function displayUrl(url: string, here: string = typeof location !== 'undefined' ? location.host : ''): string {
  if (!/^https?:\/\//.test(url)) return url;
  try {
    const u = new URL(url);
    return u.host === here ? `${u.pathname}${u.search}` : url.replace(/^https?:\/\//, '');
  } catch {
    return url;
  }
}

export function blobStoreOption(b: BlobStore): string {
  const space =
    b.capacity_bytes !== null && b.capacity_bytes !== undefined
      ? `${formatBytes(Math.max(0, b.capacity_bytes - b.used_bytes))} free`
      : `${formatBytes(b.used_bytes)} used`;
  const state = b.state === 'ok' || b.state === 'unknown' ? '' : ` (${b.state.replace('_', ' ')})`;
  return `${b.name} · ${space}${state}`;
}

export const blobStoreWritable = (b: BlobStore) => b.state !== 'read_only' && b.state !== 'unavailable';

/* ---------------------------------------------------------------- form */

export type AuthKind = 'none' | UpstreamAuth['kind'];
/** inherit = platform default proxy; named = proxyName; none = direct; url = a proxy URL (stored before named proxies). */
export type ProxyMode = 'inherit' | 'named' | 'none' | 'url';

export interface RepoForm {
  format: Format | undefined;
  type: RepositoryType | undefined;
  name: string;
  description: string;
  blobStore: string;
  writePolicy: WritePolicy;
  remoteUrl: string;
  metadataTtl: string;
  contentTtl: string;
  negativeTtl: string;
  authKind: AuthKind;
  authUsername: string;
  authPassword: string;
  authToken: string;
  authHeader: string;
  proxyMode: ProxyMode;
  proxyName: string;
  proxyUrl: string;
  caBundle: string;
  members: string[];
  writeMember: string;
  cleanupPolicy: string;
  scanOnArrival: boolean;
  quarantineCritical: boolean;
  anonymousRead: boolean;
  /** Proxy: other hosts the remote links to (config.proxy.related_hosts). */
  relatedHosts: string[];
  /* Format settings (config.format); each applies to one format and type, see formatSettingKeys. */
  /** APT hosted/group: architectures listed besides those of uploaded packages; empty = amd64, arm64. */
  aptArchitectures: string[];
  /** APT and RPM hosted/group: which key signs the metadata. */
  signingKey: SigningKeyChoice;
  /** RPM hosted/group: directory depth that holds repodata/ (0 to 4), as typed. */
  rpmRepodataDepth: string;
  /** Go proxy: checksum databases passed through under sumdb/; empty = off. */
  goSumdbs: string[];
  /** Go proxy: where checksum database requests are forwarded. */
  goSumdbVia: SumdbVia;
  /** Cargo proxy: crate download template; empty = the remote's config.json "dl". */
  cargoDownloadUrl: string;
  /** APT and RPM hosted/proxy: the distribution release advisories are checked in (config.scanning.distro); empty = unknown or detected. */
  distro: string;
}

export type SigningKeyChoice = 'repository' | 'shared';
export type SumdbVia = 'database' | 'remote';

/** Server defaults (internal/formats/apt, gomod). */
export const APT_DEFAULT_ARCHITECTURES = ['amd64', 'arm64'];
export const GO_DEFAULT_SUMDBS = ['sum.golang.org'];
/** internal/formats/rpm MaxRepodataDepth. */
export const RPM_MAX_REPODATA_DEPTH = 4;
/** Formats whose hosted and group repositories sign their metadata. */
export const SIGNED_FORMATS: Format[] = ['apt', 'rpm'];

/**
 * The config.format keys the form edits for a format and type. Keys of other
 * types (a stored sumdbs on a hosted Go repository) are left as stored.
 */
export function formatSettingKeys(format: Format | undefined, type: RepositoryType | undefined): string[] {
  if (format === 'apt' && (type === 'hosted' || type === 'group')) return ['architectures', 'signing_key'];
  if (format === 'rpm' && (type === 'hosted' || type === 'group')) return ['repodata_depth', 'signing_key'];
  if (format === 'go' && type === 'proxy') return ['sumdbs', 'sumdb_via'];
  if (format === 'cargo' && type === 'proxy') return ['download_url'];
  return [];
}

/** Formats scanned against the advisories of a distribution release (config.scanning.distro). */
export const DISTRO_FORMATS: Format[] = ['apt', 'rpm'];

/** Whether the form edits config.scanning.distro: APT and RPM repositories that hold packages. */
export function hasDistroSetting(format: Format | undefined, type: RepositoryType | undefined): boolean {
  return !!format && DISTRO_FORMATS.includes(format) && (type === 'hosted' || type === 'proxy');
}

/** OSV distribution ecosystems per format (internal/core DistroFamilies) and the common releases (internal/scanning Releases). */
export const DISTRO_FAMILIES: Partial<Record<Format, string[]>> = {
  apt: ['Debian', 'Ubuntu'],
  rpm: ['AlmaLinux', 'Mageia', 'openSUSE', 'Red Hat', 'Rocky Linux', 'SUSE'],
};
export const DISTRO_RELEASES: Partial<Record<Format, string[]>> = {
  apt: ['Debian:11', 'Debian:12', 'Debian:13', 'Ubuntu:20.04:LTS', 'Ubuntu:22.04:LTS', 'Ubuntu:24.04:LTS', 'Ubuntu:26.04:LTS'],
  rpm: [
    'AlmaLinux:8',
    'AlmaLinux:9',
    'AlmaLinux:10',
    'Rocky Linux:8',
    'Rocky Linux:9',
    'Rocky Linux:10',
    'Red Hat:enterprise_linux:8::baseos',
    'Red Hat:enterprise_linux:8::appstream',
    'Red Hat:enterprise_linux:9::baseos',
    'Red Hat:enterprise_linux:9::appstream',
  ],
};

/** Why a scanning.distro value is refused, as the server checks it; null when valid or empty. */
export function distroError(format: Format | undefined, d: string): string | null {
  if (!d) return null;
  const families = (format && DISTRO_FAMILIES[format]) || [];
  if (d.trim() !== d || d.length > 100 || !/^[A-Za-z][A-Za-z0-9 ._+-]*(:[A-Za-z0-9 ._+-]*)*$/.test(d)) {
    return 'Use an OSV ecosystem such as Debian:12 or Rocky Linux:9.';
  }
  const [base = '', ...rest] = d.split(':');
  if (!families.some((f) => f.toLowerCase() === base.toLowerCase())) return `Use one of ${families.join(', ')}.`;
  if (!rest.join(':').replace(/[: ]/g, '')) return `Add the release, e.g. ${base}:${base === 'Debian' ? '12' : '9'}.`;
  return null;
}

/** Formats whose proxies get the related hosts field (the others keep what is stored). */
export const RELATED_HOSTS_FORMATS: Format[] = ['helm'];

export function initialRepoForm(format?: Format, type?: RepositoryType): RepoForm {
  return {
    format,
    type,
    name: '',
    description: '',
    blobStore: '',
    writePolicy: format === 'docker' ? 'allow' : 'allow_once',
    remoteUrl: format ? DEFAULT_REMOTE[format] : '',
    metadataTtl: '24h',
    contentTtl: '0s',
    negativeTtl: '10m',
    authKind: 'none',
    authUsername: '',
    authPassword: '',
    authToken: '',
    authHeader: '',
    proxyMode: 'inherit',
    proxyName: '',
    proxyUrl: '',
    caBundle: '',
    members: [],
    writeMember: '',
    cleanupPolicy: '',
    scanOnArrival: true,
    quarantineCritical: true,
    anonymousRead: false,
    relatedHosts: [],
    aptArchitectures: [],
    signingKey: 'repository',
    rpmRepodataDepth: '0',
    goSumdbs: [...GO_DEFAULT_SUMDBS],
    goSumdbVia: 'database',
    cargoDownloadUrl: '',
    distro: '',
  };
}

export const NAME_PATTERN = /^[a-z0-9][a-z0-9-]{1,62}$/;
/** Go duration strings as the server parses them ("24h", "1h30m", "90s", "0s"). */
const DURATION = /^(?:0|(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+)$/;

export type RepoFormField =
  | 'format'
  | 'type'
  | 'name'
  | 'blobStore'
  | 'remoteUrl'
  | 'metadataTtl'
  | 'contentTtl'
  | 'negativeTtl'
  | 'authUsername'
  | 'authPassword'
  | 'authToken'
  | 'authHeader'
  | 'proxyName'
  | 'proxyUrl'
  | 'caBundle'
  | 'members'
  | 'writeMember'
  | 'relatedHosts'
  | 'aptArchitectures'
  | 'rpmRepodataDepth'
  | 'goSumdbs'
  | 'cargoDownloadUrl'
  | 'distro';

export type RepoFormErrors = Partial<Record<RepoFormField, string>>;

function isUrl(s: string, schemes: string[]): boolean {
  try {
    const u = new URL(s);
    return schemes.includes(u.protocol) && !!u.host;
  } catch {
    return false;
  }
}

/** Debian architecture names (internal/formats/apt archRE); "all" is not listed. */
const DEBIAN_ARCH = /^[a-z0-9][a-z0-9-]*$/;

/** A checksum database name as internal/formats/gomod validSumDBName accepts it. */
function validSumdbName(s: string): boolean {
  return s.length > 0 && s.length <= 253 && !s.startsWith('.') && !s.endsWith('.') && !s.includes('..') && /^[a-z0-9.-]+$/.test(s);
}

/** A related_hosts entry as core.ValidateHostPattern accepts it: host, host:port or .domain. */
export function validHostPattern(p: string): boolean {
  if (/[/?#@ *]/.test(p)) return false;
  let h = p.replace(/^\./, '');
  const m = /^(.*):([^:]*)$/.exec(h);
  if (m && !p.startsWith('.')) {
    const port = Number(m[2]);
    if (!/^\d+$/.test(m[2]!) || port < 1 || port > 65535) return false;
    h = m[1]!;
  }
  return h !== '' && !/[/?#@ *:]/.test(h) && !h.startsWith('.');
}

/** The cargo download_url markers (internal/formats/cargo dlMarkers). */
const CARGO_MARKERS = ['{crate}', '{version}', '{prefix}', '{lowerprefix}', '{sha256-checksum}'];

/** Whether a cargo download template expands to an absolute http(s) URL. */
function validDownloadTemplate(t: string): boolean {
  let probe = t;
  for (const m of CARGO_MARKERS) probe = probe.split(m).join('probe');
  return isUrl(probe, ['http:', 'https:']);
}

/** A URL's scheme, host and port (default port explicit); '' when it does not parse. */
function urlOrigin(s: string): string {
  try {
    const u = new URL(s.trim());
    return `${u.protocol}//${u.hostname}:${u.port || (u.protocol === 'https:' ? '443' : u.protocol === 'http:' ? '80' : '')}`;
  } catch {
    return '';
  }
}

/**
 * Whether the remote address moves to another scheme, host or port than the
 * stored one: the server then never reuses the stored secret.
 */
export function remoteMoved(f: RepoForm, storedRemoteUrl: string | undefined): boolean {
  return !!storedRemoteUrl && urlOrigin(f.remoteUrl) !== urlOrigin(storedRemoteUrl);
}

/**
 * Whether the server already holds the upstream secret for the form's
 * credentials: it keeps a stored password or token when an update leaves it
 * empty, the kind, username and header name are unchanged and the remote
 * stays on the same host.
 */
export function secretStored(f: RepoForm, stored: UpstreamAuth | null | undefined, storedRemoteUrl?: string): boolean {
  return (
    !!stored &&
    stored.kind === f.authKind &&
    (stored.username ?? '') === f.authUsername.trim() &&
    (stored.header_name ?? '') === f.authHeader.trim() &&
    !remoteMoved(f, storedRemoteUrl)
  );
}

/** Client-side checks that mirror the server's; the server stays the authority. */
export function validateRepoForm(
  f: RepoForm,
  ctx: { memberTypes?: ReadonlyMap<string, RepositoryType>; storedAuth?: UpstreamAuth | null; storedRemoteUrl?: string } = {},
): RepoFormErrors {
  const keepSecret = secretStored(f, ctx.storedAuth, ctx.storedRemoteUrl);
  // A stored secret is not sent to a new host: it must be entered again.
  const again = !!ctx.storedAuth && remoteMoved(f, ctx.storedRemoteUrl) ? ' again: it is not sent to a new host' : '';
  const e: RepoFormErrors = {};
  if (!f.format) e.format = 'Pick a format.';
  if (!f.type) e.type = 'Pick how the repository behaves.';
  if (!f.name) e.name = 'Enter a name.';
  else if (!NAME_PATTERN.test(f.name)) {
    e.name = 'Use 2–63 lowercase letters, digits and dashes, starting with a letter or digit.';
  }
  if (f.type && f.type !== 'group' && !f.blobStore) e.blobStore = 'Choose where the bytes are kept.';

  if (f.type === 'proxy') {
    if (!f.remoteUrl.trim()) e.remoteUrl = 'Enter the address of the registry to cache.';
    else if (!isUrl(f.remoteUrl.trim(), ['http:', 'https:'])) e.remoteUrl = 'Use an absolute http:// or https:// address.';
    for (const k of ['metadataTtl', 'contentTtl', 'negativeTtl'] as const) {
      if (f[k].trim() && !DURATION.test(f[k].trim())) e[k] = 'Use a duration like 30m, 24h or 1h30m.';
    }
    if (f.authKind === 'basic') {
      if (!f.authUsername.trim()) e.authUsername = 'Enter the upstream username.';
      if (!f.authPassword && !keepSecret) e.authPassword = `Enter the upstream password${again}.`;
    }
    if (f.authKind === 'bearer' && !f.authToken && !keepSecret) e.authToken = `Enter the token${again}.`;
    if (f.authKind === 'header') {
      if (!/^[A-Za-z0-9-]+$/.test(f.authHeader.trim())) e.authHeader = 'Enter a header name such as Private-Token.';
      if (!f.authToken && !keepSecret) e.authToken = `Enter the header value${again}.`;
    }
    if (f.proxyMode === 'named' && !f.proxyName) e.proxyName = 'Pick a proxy.';
    if (f.proxyMode === 'url' && !isUrl(f.proxyUrl.trim(), ['http:', 'https:', 'socks5:'])) {
      e.proxyUrl = 'Use an http://, https:// or socks5:// proxy address.';
    }
    if (f.caBundle.trim() && !f.caBundle.includes('-----BEGIN CERTIFICATE-----')) {
      e.caBundle = 'Paste PEM certificates (-----BEGIN CERTIFICATE----- …).';
    }
  }

  if (f.type === 'proxy' && f.relatedHosts.some((h) => !validHostPattern(h))) {
    e.relatedHosts = 'Use host names, host:port or .domain (no scheme or path).';
  }

  const keys = formatSettingKeys(f.format, f.type);
  if (keys.includes('architectures')) {
    if (f.aptArchitectures.includes('all')) {
      e.aptArchitectures = 'Leave out "all": Architecture: all packages are listed under every architecture.';
    } else if (f.aptArchitectures.some((a) => !DEBIAN_ARCH.test(a))) {
      e.aptArchitectures = 'Use Debian architecture names such as amd64, arm64 or i386.';
    }
  }
  if (keys.includes('repodata_depth')) {
    const d = f.rpmRepodataDepth.trim();
    if (!/^\d+$/.test(d) || Number(d) > RPM_MAX_REPODATA_DEPTH) e.rpmRepodataDepth = `Enter a whole number from 0 to ${RPM_MAX_REPODATA_DEPTH}.`;
  }
  if (keys.includes('sumdbs') && f.goSumdbs.some((d) => !validSumdbName(d))) {
    e.goSumdbs = 'Use host names in lowercase, such as sum.golang.org.';
  }
  if (keys.includes('download_url') && f.cargoDownloadUrl.trim() && !validDownloadTemplate(f.cargoDownloadUrl.trim())) {
    e.cargoDownloadUrl = 'Use an absolute http:// or https:// address.';
  }

  if (hasDistroSetting(f.format, f.type)) {
    const msg = distroError(f.format, f.distro.trim());
    if (msg) e.distro = msg;
  }

  if (f.type === 'group') {
    if (f.members.length === 0) e.members = 'Add at least one member repository.';
    if (f.writeMember) {
      if (!f.members.includes(f.writeMember)) e.writeMember = 'The write member must be one of the members.';
      else if (ctx.memberTypes && ctx.memberTypes.get(f.writeMember) !== 'hosted') {
        e.writeMember = 'Writes can only go to a hosted member.';
      }
    }
  }
  return e;
}

/**
 * The config.format values the form sets for its format and type; defaults are
 * left out, so the server applies them (and an untouched form stays empty).
 */
export function formatSettings(f: RepoForm): Record<string, unknown> {
  const keys = formatSettingKeys(f.format, f.type);
  const out: Record<string, unknown> = {};
  if (keys.includes('architectures') && f.aptArchitectures.length) out.architectures = [...f.aptArchitectures];
  if (keys.includes('signing_key') && f.signingKey === 'shared') out.signing_key = 'shared';
  if (keys.includes('repodata_depth')) {
    const d = Number(f.rpmRepodataDepth.trim());
    if (Number.isInteger(d) && d > 0) out.repodata_depth = d;
  }
  if (keys.includes('sumdbs') && JSON.stringify(f.goSumdbs) !== JSON.stringify(GO_DEFAULT_SUMDBS)) out.sumdbs = [...f.goSumdbs];
  if (keys.includes('sumdb_via') && f.goSumdbVia === 'remote') out.sumdb_via = 'remote';
  if (keys.includes('download_url') && f.cargoDownloadUrl.trim()) out.download_url = f.cargoDownloadUrl.trim();
  return out;
}

/** The request body for POST /repositories. Secrets are included; never log it. */
export function toRepositoryInput(f: RepoForm): RepositoryInput {
  const format = f.format ?? 'raw';
  const type = f.type ?? 'hosted';
  const input: RepositoryInput = {
    name: f.name,
    format,
    type,
    online: true,
    config: {
      anonymous_read: f.anonymousRead,
      scan_on_arrival: f.scanOnArrival,
      quarantine_critical: f.quarantineCritical,
    },
  };
  if (f.description.trim()) input.description = f.description.trim();
  if (type !== 'group') input.blob_store = f.blobStore;
  if (f.cleanupPolicy) input.config.cleanup_policies = [f.cleanupPolicy];

  if (type === 'hosted') input.config.hosted = { write_policy: f.writePolicy };
  if (type === 'proxy') {
    const proxy: NonNullable<RepositoryInput['config']['proxy']> = { remote_url: f.remoteUrl.trim() };
    if (f.metadataTtl.trim()) proxy.metadata_ttl = f.metadataTtl.trim();
    if (f.contentTtl.trim()) proxy.content_ttl = f.contentTtl.trim();
    if (f.negativeTtl.trim()) proxy.negative_cache_ttl = f.negativeTtl.trim();
    if (f.authKind === 'basic') proxy.auth = { kind: 'basic', username: f.authUsername.trim(), password: f.authPassword };
    if (f.authKind === 'bearer') proxy.auth = { kind: 'bearer', token: f.authToken };
    if (f.authKind === 'header') proxy.auth = { kind: 'header', header_name: f.authHeader.trim(), token: f.authToken };
    const http: NonNullable<typeof proxy.http> = {};
    if (f.proxyMode === 'none') http.proxy = 'none';
    if (f.proxyMode === 'named') http.proxy_name = f.proxyName;
    if (f.proxyMode === 'url') http.proxy = f.proxyUrl.trim();
    if (f.caBundle.trim()) http.ca_bundle = f.caBundle.trim() + '\n';
    if (Object.keys(http).length) proxy.http = http;
    if (f.relatedHosts.length) proxy.related_hosts = [...f.relatedHosts];
    input.config.proxy = proxy;
  }
  if (type === 'group') {
    input.config.group = { members: [...f.members] };
    if (f.writeMember && GROUP_WRITE_FORMATS.includes(format)) input.config.group.write_member = f.writeMember;
  }
  const settings = formatSettings(f);
  if (Object.keys(settings).length) input.config.format = settings;
  if (hasDistroSetting(format, type) && f.distro.trim()) input.config.scanning = { distro: f.distro.trim() };
  return input;
}

/** Replaces credentials in a URL's userinfo, as the server does when it returns a config. */
function redactUrl(s: string): string {
  try {
    const u = new URL(s);
    if (!u.username && !u.password) return s;
    u.username = u.username ? 'xxxxx' : '';
    u.password = u.password ? 'xxxxx' : '';
    return u.toString();
  } catch {
    return s;
  }
}

/**
 * The same configuration as YAML, in the shape POST /repositories accepts with
 * Content-Type application/yaml. Write-only secrets are left out and marked.
 */
export function repositoryYaml(input: RepositoryInput): string {
  const { config } = input;
  const cfg: { [k: string]: YamlValue | undefined } = {};
  if (config.hosted) cfg.hosted = { write_policy: config.hosted.write_policy ?? 'allow' };
  if (config.proxy) {
    const p = config.proxy;
    const proxy: { [k: string]: YamlValue | undefined } = {
      remote_url: redactUrl(p.remote_url),
      metadata_ttl: p.metadata_ttl,
      content_ttl: p.content_ttl,
      negative_cache_ttl: p.negative_cache_ttl,
    };
    if (p.auth) {
      const a = p.auth;
      proxy.auth = {
        kind: a.kind,
        username: a.username,
        header_name: a.header_name,
        _secret: new YamlComment(`${a.kind === 'basic' ? 'password' : 'token'}: write-only, not shown`),
      };
    }
    if (p.http) {
      proxy.http = { proxy: p.http.proxy ? redactUrl(p.http.proxy) : undefined, proxy_name: p.http.proxy_name, ca_bundle: p.http.ca_bundle };
    }
    if (p.related_hosts?.length) proxy.related_hosts = p.related_hosts;
    cfg.proxy = proxy;
  }
  if (config.group) cfg.group = { members: config.group.members, write_member: config.group.write_member };
  cfg.anonymous_read = config.anonymous_read ?? false;
  cfg.scan_on_arrival = config.scan_on_arrival ?? false;
  cfg.quarantine_critical = config.quarantine_critical ?? false;
  if (config.scanning?.distro) cfg.scanning = { distro: config.scanning.distro };
  if (config.cleanup_policies?.length) cfg.cleanup_policies = config.cleanup_policies;
  if (config.format && Object.keys(config.format).length) cfg.format = config.format as { [k: string]: YamlValue };
  if (config.routing) {
    const r = config.routing;
    const routing: { [k: string]: YamlValue | undefined } = {};
    if (r.allow?.length) routing.allow = r.allow;
    if (r.block?.length) routing.block = r.block;
    const members = (r.members ?? [])
      .filter((m) => m.exclusive?.length || m.block?.length)
      .map((m) => ({ member: m.member, exclusive: m.exclusive?.length ? m.exclusive : undefined, block: m.block?.length ? m.block : undefined }));
    if (members.length) routing.members = members;
    if (Object.keys(routing).length) cfg.routing = routing;
  }

  return toYaml({
    name: input.name || '<name>',
    format: input.format,
    type: input.type,
    description: input.description,
    blob_store: input.blob_store || undefined,
    online: input.online ?? true,
    config: cfg,
  });
}

/** Client URL the server will build, assuming its base URL is this page's origin. */
export function previewUrl(format: Format | undefined, name: string, origin: string): string {
  const n = name || '<name>';
  if (format === 'docker') return `${new URL(origin).host}/${n}`;
  return `${origin.replace(/\/$/, '')}/repository/${n}/`;
}
