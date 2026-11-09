import type { Format, RepositoryType } from '../api/types';

export interface CodeLine {
  text: string;
  comment?: boolean;
}

/**
 * One repository a client should be pointed at. Everything comes from the
 * server (`repository.url` or `/me/client-endpoints`); no host is hard-coded.
 */
export interface ClientTarget {
  format: Format;
  repository: string;
  /** The repository URL clients use: `https://host/repository/<repo>/`, or `host/<repo>` for docker. */
  url?: string;
  /** Docker only: the registry host to log in to when no image URL is known. */
  host?: string;
  type?: RepositoryType;
  /** Hosted repositories, and groups with a write member, also accept uploads. */
  publishes?: boolean;
  /** Who is signed in; a placeholder is shown when unknown. */
  username?: string;
  /** A real token to put in the snippets (shown once after creating one); a placeholder otherwise. */
  token?: string;
}

export interface ClientSetup {
  /** Read access: configure the client with the repository and a token. */
  setup: CodeLine[];
  /** Upload commands; empty when the repository does not accept uploads. */
  publish: CodeLine[];
}

const TOKEN = '<token>';
const slash = (u: string) => (u.endsWith('/') ? u : `${u}/`);
const noScheme = (u: string) => u.replace(/^https?:/, '');
/** The registry host docker clients log in to: the image URL up to the first slash. */
const dockerHost = (url: string) => url.replace(/^https?:\/\//, '').split('/')[0] ?? url;
/** `https://host/path` with `user:token@` inserted after the scheme. */
const withCredentials = (url: string, user: string, token = TOKEN) => url.replace(/^(https?:\/\/)/, `$1${user}:${token}@`);
const c = (text: string): CodeLine => ({ text, comment: true });
const l = (text: string): CodeLine => ({ text });

/**
 * Copyable client configuration for one repository, matching the format
 * guides in docs/guides. Credentials are placeholders: a personal access token
 * goes where `<token>` is.
 */
export function clientSetup(t: ClientTarget): ClientSetup {
  const user = t.username || '<username>';
  const tok = t.token || TOKEN;
  const repo = t.repository;
  const url = t.url ? slash(t.url) : '';
  switch (t.format) {
    case 'nuget': {
      const index = `${url}index.json`;
      return {
        setup: [
          c('# The service index is public; restoring packages needs the token'),
          l(`dotnet nuget add source ${index} -n ${repo} -u ${user} -p ${tok} --store-password-in-clear-text`),
        ],
        publish: t.publishes ? [l(`dotnet nuget push <package>.nupkg -s ${index} -k ${tok}`)] : [],
      };
    }
    case 'npm':
      return {
        setup: [c('# .npmrc in the project or your home directory'), l(`registry=${url}`), l(`${noScheme(url)}:_authToken=${tok}`)],
        publish: t.publishes ? [l(`npm publish --registry ${url}`)] : [],
      };
    case 'pypi':
      return {
        setup: [l(`pip config set global.index-url ${withCredentials(`${url}simple/`, '__token__', tok)}`)],
        publish: t.publishes ? [l(`twine upload --repository-url ${url} -u __token__ -p ${tok} dist/*`)] : [],
      };
    case 'maven': {
      const server = `    <server><id>${repo}</id><username>${user}</username><password>\${env.HANGAR_TOKEN}</password></server>`;
      // A hosted repository holds only your own artifacts, so it is not a mirror for everything.
      const mirror = t.type !== 'hosted';
      return {
        setup: [
          c(`# export HANGAR_TOKEN=${tok}`),
          c('<!-- ~/.m2/settings.xml -->'),
          l('<settings>'),
          l('  <servers>'),
          l(server),
          l('  </servers>'),
          ...(mirror
            ? [l('  <mirrors>'), l(`    <mirror><id>${repo}</id><mirrorOf>*</mirrorOf><url>${url}</url></mirror>`), l('  </mirrors>')]
            : []),
          l('</settings>'),
        ],
        publish: t.publishes
          ? [
              c('<!-- pom.xml -->'),
              l('<distributionManagement>'),
              l(`  <repository><id>${repo}</id><url>${url}</url></repository>`),
              l('</distributionManagement>'),
              l('mvn deploy'),
            ]
          : [],
      };
    }
    case 'docker': {
      const image = t.url?.replace(/\/+$/, '');
      const host = t.host ?? (image ? dockerHost(image) : '');
      return {
        setup: [
          l(`echo '${tok}' | docker login ${host} -u ${user} --password-stdin`),
          ...(image ? [l(`docker pull ${image}/<image>:<tag>`)] : []),
        ],
        publish:
          t.publishes && image ? [l(`docker tag <image>:<tag> ${image}/<image>:<tag>`), l(`docker push ${image}/<image>:<tag>`)] : [],
      };
    }
    case 'raw':
      return {
        setup: [l(`curl -u ${user}:${tok} -fLO ${url}<path>`)],
        publish: t.publishes ? [l(`curl -u ${user}:${tok} --upload-file ./<file> ${url}<path>/<file>`)] : [],
      };
    case 'helm':
      return {
        setup: [l(`helm repo add ${repo} ${url} --username ${user} --password ${tok}`), l('helm repo update')],
        publish: t.publishes
          ? [
              l(`helm cm-push ./<chart>-<version>.tgz ${repo}`),
              c('# or without the plugin'),
              l(`curl -u ${user}:${tok} --upload-file ./<chart>-<version>.tgz ${url}<chart>-<version>.tgz`),
            ]
          : [],
      };
    case 'go': {
      const proxy = url.replace(/\/+$/, '');
      return {
        setup: [
          l(`go env -w GOPROXY=${proxy},direct`),
          c('# Private modules: fetch them through Hangar, never ask sum.golang.org'),
          l('go env -w GOPRIVATE=<module prefix> GONOPROXY=none GONOSUMDB=<module prefix>'),
          c('# ~/.netrc (the go command sends it over HTTPS only)'),
          l(`machine ${dockerHost(url)} login token password ${tok}`),
        ],
        publish: t.publishes
          ? [c('# A module zip built with golang.org/x/mod/zip (see the Go guide)'), l(`curl -u token:${tok} --upload-file <version>.zip ${url}<module>/@v/<version>.zip`)]
          : [],
      };
    }
    case 'cargo': {
      // Proxies and groups stand in for crates.io; a hosted repository is named per dependency.
      const replace = t.type !== 'hosted';
      const env = `CARGO_REGISTRIES_${repo.toUpperCase().replace(/-/g, '_')}_TOKEN`;
      return {
        setup: [
          c('# ~/.cargo/config.toml'),
          l('[registry]'),
          l('global-credential-providers = ["cargo:token"]'),
          l(`[registries.${repo}]`),
          l(`index = "sparse+${url}index/"`),
          ...(replace ? [l('[source.crates-io]'), l(`replace-with = "${repo}"`)] : []),
          c(`# Then store the token (or export ${env}=${tok})`),
          l(`echo '${tok}' | cargo login --registry ${repo}`),
        ],
        publish: t.publishes ? [l(`cargo publish --registry ${repo}`)] : [],
      };
    }
    case 'apt': {
      // A proxy passes the remote's signed metadata through: trust the remote's archive key.
      const proxy = t.type === 'proxy';
      const keyring = proxy ? '/usr/share/keyrings/debian-archive-keyring.gpg' : `/etc/apt/keyrings/${repo}.gpg`;
      // apt sends auth.conf credentials over plain http only when the entry names the scheme.
      const machine = url.startsWith('http://') ? `http://${dockerHost(url)}` : dockerHost(url);
      return {
        setup: [
          ...(proxy
            ? [c("# Signed by the remote archive: use its keyring (Ubuntu: ubuntu-archive-keyring.gpg)")]
            : [l(`curl -fsSL -u ${user}:${tok} ${url}public.gpg | sudo tee ${keyring} > /dev/null`)]),
          l(`sudo tee /etc/apt/sources.list.d/${repo}.sources <<'EOF'`),
          l('Types: deb'),
          l(`URIs: ${url}`),
          l(`Suites: ${proxy ? 'bookworm' : 'stable'}`),
          l('Components: main'),
          l(`Signed-By: ${keyring}`),
          l('EOF'),
          c('# Private repositories: credentials for apt (mode 0600)'),
          l(`echo 'machine ${machine} login ${user} password ${tok}' | sudo tee /etc/apt/auth.conf.d/${repo}.conf > /dev/null`),
          l(`sudo chmod 600 /etc/apt/auth.conf.d/${repo}.conf && sudo apt-get update`),
        ],
        publish: t.publishes
          ? [l(`curl -u ${user}:${tok} --upload-file ./<package>.deb "${url}upload?distribution=stable&component=main"`)]
          : [],
      };
    }
    case 'rpm': {
      // A proxy serves the remote's metadata and packages, signed (if at all) by the remote.
      const proxy = t.type === 'proxy';
      return {
        setup: [
          l(`sudo tee /etc/yum.repos.d/${repo}.repo <<'EOF'`),
          l(`[${repo}]`),
          l(`name=${repo}`),
          l(`baseurl=${url}`),
          l('enabled=1'),
          ...(proxy
            ? [l('gpgcheck=1'), l('repo_gpgcheck=0'), l('gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9')]
            : [
                c('# repomd.xml is signed by Hangar; set gpgcheck=1 when your packages are signed'),
                l('gpgcheck=0'),
                l('repo_gpgcheck=1'),
                l(`gpgkey=${url}public.key`),
              ]),
          l(`username=${user}`),
          l(`password=${tok}`),
          l('EOF'),
          l(`sudo dnf makecache --repo ${repo}`),
        ],
        publish: t.publishes ? [l(`curl -u ${user}:${tok} --upload-file ./<package>.rpm ${url}<package>.rpm`)] : [],
      };
    }
  }
}

/** Set-up and publish lines in one block, the publish part under its own heading. */
export function clientLines(t: ClientTarget): CodeLine[] {
  const { setup, publish } = clientSetup(t);
  return publish.length ? [...setup, c('# Publish'), ...publish] : setup;
}

/** One package version to install. */
export interface PackageVersion {
  /** Scope, groupId or image path prefix; empty when the format has none. */
  namespace?: string;
  name: string;
  version: string;
}

/** Install commands for one exact version with one client. */
export interface InstallCommand {
  id: string;
  label: string;
  lines: CodeLine[];
}

/**
 * Commands that install `p` from the repository `t`, one per common client
 * of the format (npm, pnpm and Yarn; dotnet and PackageReference; Maven and
 * Gradle; pip and uv; docker and Podman; curl). Empty without a repository URL.
 */
export function installCommands(t: Pick<ClientTarget, 'format' | 'url'> & { repository?: string }, p: PackageVersion): InstallCommand[] {
  if (!t.url) return [];
  const url = slash(t.url);
  const v = p.version;
  const full = p.namespace ? `${p.namespace}/${p.name}` : p.name;
  switch (t.format) {
    case 'npm':
      return [
        { id: 'npm', label: 'npm', lines: [l(`npm install ${full}@${v} --registry ${url}`)] },
        { id: 'pnpm', label: 'pnpm', lines: [l(`pnpm add ${full}@${v} --registry ${url}`)] },
        { id: 'yarn', label: 'Yarn', lines: [l(`yarn config set npmRegistryServer ${url}`), l(`yarn add ${full}@${v}`)] },
      ];
    case 'nuget':
      return [
        { id: 'dotnet', label: 'dotnet CLI', lines: [l(`dotnet add package ${p.name} --version ${v} --source ${url}index.json`)] },
        { id: 'csproj', label: 'PackageReference', lines: [c('<!-- in the project file -->'), l(`<PackageReference Include="${p.name}" Version="${v}" />`)] },
      ];
    case 'maven':
      return [
        {
          id: 'maven',
          label: 'Maven',
          lines: [
            c('<!-- pom.xml -->'),
            l('<dependency>'),
            l(`  <groupId>${p.namespace ?? ''}</groupId>`),
            l(`  <artifactId>${p.name}</artifactId>`),
            l(`  <version>${v}</version>`),
            l('</dependency>'),
          ],
        },
        {
          id: 'gradle',
          label: 'Gradle',
          lines: [
            c('// build.gradle.kts'),
            l('repositories {'),
            l(`    maven { url = uri("${url}") }`),
            l('}'),
            l('dependencies {'),
            l(`    implementation("${p.namespace ?? ''}:${p.name}:${v}")`),
            l('}'),
          ],
        },
      ];
    case 'pypi':
      return [
        { id: 'pip', label: 'pip', lines: [l(`pip install ${p.name}==${v} --index-url ${url}simple/`)] },
        { id: 'uv', label: 'uv', lines: [l(`uv pip install ${p.name}==${v} --index-url ${url}simple/`)] },
      ];
    case 'docker': {
      const image = `${t.url.replace(/\/+$/, '')}/${full}:${v}`;
      return [
        { id: 'docker', label: 'Docker', lines: [l(`docker pull ${image}`)] },
        { id: 'podman', label: 'Podman', lines: [l(`podman pull ${image}`)] },
      ];
    }
    case 'cargo':
      return [
        { id: 'cargo', label: 'cargo add', lines: [l(`cargo add ${p.name}@${v} --registry ${t.repository ?? '<registry>'}`)] },
        { id: 'toml', label: 'Cargo.toml', lines: [c('# [dependencies]'), l(`${p.name} = { version = "${v}", registry = "${t.repository ?? '<registry>'}" }`)] },
      ];
    case 'apt':
      return [{ id: 'apt', label: 'apt', lines: [l(`sudo apt-get install ${p.name}=${v}`)] }];
    case 'rpm':
      return [{ id: 'dnf', label: 'dnf', lines: [l(`sudo dnf install ${p.name}-${v}`)] }];
    case 'raw':
      return [{ id: 'curl', label: 'curl', lines: [l(`curl -u <username>:${TOKEN} -fLO ${url}${full.replace(/^\/+/, '')}`)] }];
    case 'helm': {
      // `helm repo add` names the repository after the Hangar repository (see the set-up snippet).
      const repo = url.replace(/\/+$/, '').split('/').pop() ?? '<repo>';
      return [
        { id: 'helm', label: 'Helm', lines: [l(`helm install <release> ${repo}/${p.name} --version ${v}`)] },
        { id: 'pull', label: 'Pull', lines: [l(`helm pull ${repo}/${p.name} --version ${v}`)] },
      ];
    }
    default:
      return [];
  }
}
