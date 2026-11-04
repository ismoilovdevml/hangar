import type {
  BlobStore,
  Format,
  Repository,
  RepositoryInput,
  RepositoryStatus,
  RepositoryType,
  UpstreamAuth,
  WritePolicy,
} from '../api/types';
import { formatBytes, type Tone } from './format';
import { toYaml, YamlComment, type YamlValue } from './yaml';

export const ALL_FORMATS: Format[] = ['docker', 'npm', 'maven', 'pypi', 'go', 'helm', 'nuget', 'cargo', 'apt', 'raw'];
export const REPO_TYPES: RepositoryType[] = ['hosted', 'proxy', 'group'];

export const isFormat = (v: unknown): v is Format => ALL_FORMATS.includes(v as Format);
export const isRepoType = (v: unknown): v is RepositoryType => REPO_TYPES.includes(v as RepositoryType);

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
  apt: 'Debian',
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
  raw: '',
};

/** Formats whose clients can publish through a group (it forwards writes to one hosted member). */
export const GROUP_WRITE_FORMATS: Format[] = ['docker', 'npm'];

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
export type ProxyMode = 'inherit' | 'none' | 'url';

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
  proxyUrl: string;
  caBundle: string;
  members: string[];
  writeMember: string;
  cleanupPolicy: string;
  scanOnArrival: boolean;
  quarantineCritical: boolean;
  anonymousRead: boolean;
}

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
    proxyUrl: '',
    caBundle: '',
    members: [],
    writeMember: '',
    cleanupPolicy: '',
    scanOnArrival: true,
    quarantineCritical: true,
    anonymousRead: false,
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
  | 'proxyUrl'
  | 'caBundle'
  | 'members'
  | 'writeMember';

export type RepoFormErrors = Partial<Record<RepoFormField, string>>;

function isUrl(s: string, schemes: string[]): boolean {
  try {
    const u = new URL(s);
    return schemes.includes(u.protocol) && !!u.host;
  } catch {
    return false;
  }
}

/** Client-side checks that mirror the server's; the server stays the authority. */
export function validateRepoForm(f: RepoForm, ctx: { memberTypes?: ReadonlyMap<string, RepositoryType> } = {}): RepoFormErrors {
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
      if (!f.authPassword) e.authPassword = 'Enter the upstream password.';
    }
    if (f.authKind === 'bearer' && !f.authToken) e.authToken = 'Enter the token.';
    if (f.authKind === 'header') {
      if (!/^[A-Za-z0-9-]+$/.test(f.authHeader.trim())) e.authHeader = 'Enter a header name such as Private-Token.';
      if (!f.authToken) e.authToken = 'Enter the header value.';
    }
    if (f.proxyMode === 'url' && !isUrl(f.proxyUrl.trim(), ['http:', 'https:', 'socks5:'])) {
      e.proxyUrl = 'Use an http://, https:// or socks5:// proxy address.';
    }
    if (f.caBundle.trim() && !f.caBundle.includes('-----BEGIN CERTIFICATE-----')) {
      e.caBundle = 'Paste PEM certificates (-----BEGIN CERTIFICATE----- …).';
    }
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
    if (f.proxyMode === 'url') http.proxy = f.proxyUrl.trim();
    if (f.caBundle.trim()) http.ca_bundle = f.caBundle.trim() + '\n';
    if (Object.keys(http).length) proxy.http = http;
    input.config.proxy = proxy;
  }
  if (type === 'group') {
    input.config.group = { members: [...f.members] };
    if (f.writeMember && GROUP_WRITE_FORMATS.includes(format)) input.config.group.write_member = f.writeMember;
  }
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
      proxy.http = { proxy: p.http.proxy ? redactUrl(p.http.proxy) : undefined, ca_bundle: p.http.ca_bundle };
    }
    cfg.proxy = proxy;
  }
  if (config.group) cfg.group = { members: config.group.members, write_member: config.group.write_member };
  cfg.anonymous_read = config.anonymous_read ?? false;
  cfg.scan_on_arrival = config.scan_on_arrival ?? false;
  cfg.quarantine_critical = config.quarantine_critical ?? false;
  if (config.cleanup_policies?.length) cfg.cleanup_policies = config.cleanup_policies;

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
