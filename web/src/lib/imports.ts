/** Helpers of the import and pre-warm task form. */

export type ImportKind = 'import' | 'prewarm';

export function isImportKind(v: unknown): v is ImportKind {
  return v === 'import' || v === 'prewarm';
}

/** Task names, as the server checks them. */
export const TASK_NAME_RE = /^[a-z0-9][a-z0-9-]{1,62}$/;

/** Splits a list typed one per line (or comma separated) into its entries. */
export function splitList(s: string): string[] {
  return s
    .split(/[\n,]/)
    .map((x) => x.trim())
    .filter((x) => x !== '' && !x.startsWith('#'));
}

export interface ImportHints {
  url: string;
  source: string;
  packages: string;
  /** The source can list its packages, so an empty list means all. */
  packagesOptional: boolean;
  versions: string;
  coordinates: string;
  lockfiles: string;
}

const HINTS: Record<string, ImportHints> = {
  npm: {
    url: 'https://registry.example.com/',
    source: 'The registry URL npm is configured with.',
    packages: 'lodash\n@acme/*',
    packagesOptional: true,
    versions: '^1.2.0',
    coordinates: 'lodash@4.17.21\n@acme/ui@2.0.1',
    lockfiles: 'package-lock.json, pnpm-lock.yaml, yarn.lock',
  },
  nuget: {
    url: 'https://feed.example.com/v3/index.json',
    source: 'The V3 service index (or a V2 feed URL).',
    packages: 'Acme.*',
    packagesOptional: true,
    versions: '>=2.0',
    coordinates: 'Newtonsoft.Json@13.0.3',
    lockfiles: 'packages.lock.json or a list of id@version',
  },
  maven: {
    url: 'https://repo.example.com/maven2/',
    source: 'The repository root. Patterns need directory listings.',
    packages: 'com.acme:core\ncom.acme.*:*',
    packagesOptional: true,
    versions: '>=1.0 <2.0',
    coordinates: 'com.acme:core:1.4.2',
    lockfiles: 'gradle.lockfile or a list of group:artifact:version',
  },
  pypi: {
    url: 'https://pypi.example.com/simple/',
    source: 'The simple index URL pip is configured with.',
    packages: 'requests\nacme-*',
    packagesOptional: true,
    versions: '>=2.0,<3',
    coordinates: 'requests==2.32.3',
    lockfiles: 'requirements.txt with pins, uv.lock, poetry.lock',
  },
  go: {
    url: 'https://goproxy.example.com/',
    source: 'A GOPROXY. It cannot list modules: name them.',
    packages: 'example.com/acme/lib',
    packagesOptional: false,
    versions: '>=v1.2.0',
    coordinates: 'golang.org/x/text@v0.21.0',
    lockfiles: 'go.sum',
  },
  cargo: {
    url: 'sparse+https://index.example.com/',
    source: 'The sparse index URL.',
    packages: 'serde\nacme-*',
    packagesOptional: true,
    versions: '^1.0',
    coordinates: 'serde@1.0.215',
    lockfiles: 'Cargo.lock',
  },
  helm: {
    url: 'https://charts.example.com/',
    source: 'The chart repository URL (the one helm repo add takes).',
    packages: 'web\nacme-*',
    packagesOptional: true,
    versions: '>=1.0.0',
    coordinates: 'nginx@18.2.0',
    lockfiles: 'Chart.lock',
  },
  docker: {
    url: 'https://registry.example.com/',
    source: 'The registry root. Without /v2/_catalog, name the images.',
    packages: 'team/app\nteam/*',
    packagesOptional: true,
    versions: 'v1.*',
    coordinates: 'library/nginx:1.27\nteam/app@sha256:…',
    lockfiles: 'a list of image:tag',
  },
  raw: {
    url: 'https://files.example.com/',
    source: 'The base URL; list the files under More options.',
    packages: 'tools/*',
    packagesOptional: true,
    versions: '',
    coordinates: 'tools/cli-1.0.tar.gz',
    lockfiles: 'a list of paths',
  },
  apt: {
    url: 'https://deb.example.com/debian/',
    source: 'The archive root (with dists/ and pool/). Suite under More options.',
    packages: 'hello\nlib*',
    packagesOptional: true,
    versions: '>=2.0',
    coordinates: 'pool/main/h/hello/hello_2.10-3_amd64.deb',
    lockfiles: 'a list of pool paths',
  },
  rpm: {
    url: 'https://rpm.example.com/el9/x86_64/',
    source: 'The repository directory that holds repodata/.',
    packages: 'hello\nlib*',
    packagesOptional: true,
    versions: '>=2.0',
    coordinates: 'Packages/h/hello-2.10-1.el9.x86_64.rpm',
    lockfiles: 'a list of package paths',
  },
};

const GENERIC: ImportHints = {
  url: 'https://registry.example.com/',
  source: 'The URL the format’s clients use for the source registry.',
  packages: 'name\nprefix-*',
  packagesOptional: true,
  versions: '>=1.0',
  coordinates: 'one coordinate per line',
  lockfiles: 'a lockfile or a list',
};

export function importHints(format: string): ImportHints {
  return HINTS[format] ?? GENERIC;
}
