import type { ClientEndpoint } from '../api/types';

export interface CodeLine {
  text: string;
  comment?: boolean;
}

/** Comment heading per format: the client tool people recognise. */
const TOOL: Record<ClientEndpoint['format'], string> = {
  docker: 'Docker',
  npm: 'npm',
  pypi: 'pip',
  maven: 'Maven',
  go: 'Go',
  helm: 'Helm',
  nuget: 'dotnet',
  cargo: 'Cargo',
  apt: 'APT',
  raw: 'curl',
};

function trimSlash(url: string): string {
  return url.endsWith('/') ? url : `${url}/`;
}

/** Turns the server-provided endpoints into copyable client configuration lines. */
export function clientSnippet(endpoints: ClientEndpoint[], max = 3): CodeLine[] {
  const lines: CodeLine[] = [];
  for (const e of endpoints.slice(0, max)) {
    lines.push({ text: `# ${TOOL[e.format]}`, comment: true });
    switch (e.format) {
      case 'docker':
        lines.push({ text: `docker login ${e.endpoint}` });
        break;
      case 'npm':
        lines.push({ text: `npm config set registry ${trimSlash(e.endpoint)}` });
        break;
      case 'pypi':
        lines.push({ text: `pip config set global.index-url \\` });
        lines.push({ text: e.endpoint });
        break;
      case 'maven':
        lines.push({ text: `# add to ~/.m2/settings.xml <mirror><url>`, comment: true });
        lines.push({ text: e.endpoint });
        break;
      case 'go':
        lines.push({ text: `go env -w GOPROXY=${e.endpoint},direct` });
        break;
      case 'helm':
        lines.push({ text: `helm repo add ${e.repository} ${e.endpoint}` });
        break;
      case 'nuget':
        lines.push({ text: `dotnet nuget add source ${e.endpoint} -n ${e.repository}` });
        break;
      case 'cargo':
        lines.push({ text: `# ~/.cargo/config.toml: [registries.${e.repository}]`, comment: true });
        lines.push({ text: `index = "sparse+${trimSlash(e.endpoint)}"` });
        break;
      case 'apt':
        lines.push({ text: `echo "deb ${e.endpoint} stable main" | sudo tee /etc/apt/sources.list.d/${e.repository}.list` });
        break;
      case 'raw':
        lines.push({ text: `curl -fLO ${trimSlash(e.endpoint)}<path>` });
        break;
    }
  }
  return lines;
}
