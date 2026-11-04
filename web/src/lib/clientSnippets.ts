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
const withCredentials = (url: string, user: string) => url.replace(/^(https?:\/\/)/, `$1${user}:${TOKEN}@`);
const c = (text: string): CodeLine => ({ text, comment: true });
const l = (text: string): CodeLine => ({ text });

/**
 * Copyable client configuration for one repository, matching the format
 * guides in docs/guides. Credentials are placeholders: a personal access token
 * goes where `<token>` is.
 */
export function clientSetup(t: ClientTarget): ClientSetup {
  const user = t.username || '<username>';
  const repo = t.repository;
  const url = t.url ? slash(t.url) : '';
  switch (t.format) {
    case 'nuget': {
      const index = `${url}index.json`;
      return {
        setup: [
          c('# The service index is public; restoring packages needs the token'),
          l(`dotnet nuget add source ${index} -n ${repo} -u ${user} -p ${TOKEN} --store-password-in-clear-text`),
        ],
        publish: t.publishes ? [l(`dotnet nuget push <package>.nupkg -s ${index} -k ${TOKEN}`)] : [],
      };
    }
    case 'npm':
      return {
        setup: [c('# .npmrc in the project or your home directory'), l(`registry=${url}`), l(`${noScheme(url)}:_authToken=${TOKEN}`)],
        publish: t.publishes ? [l(`npm publish --registry ${url}`)] : [],
      };
    case 'pypi':
      return {
        setup: [l(`pip config set global.index-url ${withCredentials(`${url}simple/`, '__token__')}`)],
        publish: t.publishes ? [l(`twine upload --repository-url ${url} -u __token__ -p ${TOKEN} dist/*`)] : [],
      };
    case 'maven': {
      const server = `    <server><id>${repo}</id><username>${user}</username><password>\${env.HANGAR_TOKEN}</password></server>`;
      // A hosted repository holds only your own artifacts, so it is not a mirror for everything.
      const mirror = t.type !== 'hosted';
      return {
        setup: [
          c(`# export HANGAR_TOKEN=${TOKEN}`),
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
          l(`echo '${TOKEN}' | docker login ${host} -u ${user} --password-stdin`),
          ...(image ? [l(`docker pull ${image}/<image>:<tag>`)] : []),
        ],
        publish:
          t.publishes && image ? [l(`docker tag <image>:<tag> ${image}/<image>:<tag>`), l(`docker push ${image}/<image>:<tag>`)] : [],
      };
    }
    case 'raw':
      return {
        setup: [l(`curl -u ${user}:${TOKEN} -fLO ${url}<path>`)],
        publish: t.publishes ? [l(`curl -u ${user}:${TOKEN} --upload-file ./<file> ${url}<path>/<file>`)] : [],
      };
    case 'helm':
      return { setup: [l(`helm repo add ${repo} ${url} --username ${user} --password ${TOKEN}`)], publish: [] };
    case 'go':
      return {
        setup: [l(`go env -w GOPROXY=${url},direct`), c(`# ~/.netrc: machine ${dockerHost(url)} login ${user} password ${TOKEN}`)],
        publish: [],
      };
    case 'cargo':
      return {
        setup: [c(`# ~/.cargo/config.toml: [registries.${repo}]`), l(`index = "sparse+${url}"`)],
        publish: [],
      };
    case 'apt':
      return { setup: [l(`echo "deb ${url} stable main" | sudo tee /etc/apt/sources.list.d/${repo}.list`)], publish: [] };
  }
}

/** Set-up and publish lines in one block, the publish part under its own heading. */
export function clientLines(t: ClientTarget): CodeLine[] {
  const { setup, publish } = clientSetup(t);
  return publish.length ? [...setup, c('# Publish'), ...publish] : setup;
}
