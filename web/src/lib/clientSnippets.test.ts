import { describe, expect, it } from 'vitest';
import { clientLines, clientSetup, installCommands } from './clientSnippets';

const text = (lines: { text: string; comment?: boolean }[]) => lines.map((l) => l.text);
const code = (lines: { text: string; comment?: boolean }[]) => lines.filter((l) => !l.comment).map((l) => l.text);

describe('client snippets follow the repository type', () => {
  it('Maven mirrors only to a proxy or group; a hosted repository is added by URL', () => {
    const group = clientSetup({ format: 'maven', repository: 'maven-all', url: 'https://hangar.example.com/repository/maven-all/', type: 'group' });
    expect(text(group.setup)).toContain('    <mirror><id>maven-all</id><mirrorOf>*</mirrorOf><url>https://hangar.example.com/repository/maven-all/</url></mirror>');
    expect(group.publish).toEqual([]);

    const hosted = clientSetup({ format: 'maven', repository: 'maven-releases', url: 'https://hangar.example.com/repository/maven-releases/', type: 'hosted', publishes: true });
    const xml = text(hosted.setup).join('\n');
    expect(xml).not.toContain('<mirror>');
    expect(xml).toContain('<repositories><repository><id>maven-releases</id><url>https://hangar.example.com/repository/maven-releases/</url></repository></repositories>');
    expect(xml).toContain('<activeProfiles><activeProfile>maven-releases</activeProfile></activeProfiles>');
    expect(text(hosted.publish).join('\n')).toContain('<repository><id>maven-releases</id><url>https://hangar.example.com/repository/maven-releases/</url></repository>');

    // An unknown type is never a mirror for everything.
    expect(text(clientSetup({ format: 'maven', repository: 'm', url: 'https://h/repository/m/' }).setup).join('\n')).not.toContain('<mirror>');
  });

  it('keeps the shell export out of the settings.xml block', () => {
    const s = clientSetup({ format: 'maven', repository: 'maven-all', url: 'https://h/repository/maven-all/', type: 'group' });
    expect(text(s.shell ?? [])).toEqual(['export HANGAR_TOKEN=<token>']);
    expect(code(s.setup)[0]).toBe('<settings>');
    // The single-block form keeps it, commented.
    expect(clientLines({ format: 'maven', repository: 'maven-all', url: 'https://h/repository/maven-all/', type: 'group' })[0]).toEqual({
      text: '# export HANGAR_TOKEN=<token>',
      comment: true,
    });
  });

  it('Go over plain http says what is needed instead of a netrc that is never sent', () => {
    const locked = clientSetup({ format: 'go', repository: 'go-proxy', url: 'http://localhost:8080/repository/go-proxy/', type: 'proxy' });
    const all = text(locked.setup).join('\n');
    expect(all).toContain('sends credentials over HTTPS only');
    expect(all).toContain('turn on anonymous read for go-proxy');
    expect(all).not.toContain('machine ');
    const open = clientSetup({ format: 'go', repository: 'go-proxy', url: 'http://localhost:8080/repository/go-proxy/', type: 'proxy', anonymousRead: true });
    expect(code(open.setup)[0]).toBe('go env -w GOPROXY=http://localhost:8080/repository/go-proxy,direct');
    expect(text(open.setup).join('\n')).not.toContain('HTTPS only');
    // Over https the netrc machine is host[:port], which the go command matches.
    const tls = clientSetup({ format: 'go', repository: 'go-proxy', url: 'https://hangar.example.com:8443/repository/go-proxy/' });
    expect(text(tls.setup)).toContain('machine hangar.example.com:8443 login token password <token>');
  });

  it('cargo never names a registry crates-io, which is its own source', () => {
    const s = clientSetup({ format: 'cargo', repository: 'crates-io', url: 'https://h/repository/crates-io/', type: 'proxy' });
    expect(text(s.setup)).toEqual(expect.arrayContaining(['[registries.hangar-crates-io]', 'replace-with = "hangar-crates-io"']));
    expect(installCommands({ format: 'cargo', url: 'https://h/repository/crates-io/', repository: 'crates-io' }, { name: 'itoa', version: '1.0.11' })[0]!.lines[0]!.text).toBe(
      'cargo add itoa@1.0.11 --registry hangar-crates-io',
    );
  });

  it('opts in to plain http where clients require it', () => {
    const nuget = clientSetup({ format: 'nuget', repository: 'n', url: 'http://localhost:8080/repository/n/' });
    expect(text(nuget.setup).join('\n')).toContain('--allow-insecure-connections');
    expect(text(clientSetup({ format: 'nuget', repository: 'n', url: 'https://h/repository/n/' }).setup).join('\n')).not.toContain('insecure');

    const pip = clientSetup({ format: 'pypi', repository: 'p', url: 'http://pkg.internal:8080/repository/p/' });
    expect(text(pip.setup)).toContain('pip config set global.trusted-host pkg.internal');
    expect(text(clientSetup({ format: 'pypi', repository: 'p', url: 'http://127.0.0.1:8080/repository/p/' }).setup)).toHaveLength(1);

    const gradle = installCommands({ format: 'maven', url: 'http://localhost:8080/repository/m/' }, { namespace: 'g', name: 'a', version: '1' }).find((x) => x.id === 'gradle')!;
    expect(text(gradle.lines)).toContain('        isAllowInsecureProtocol = true');
  });

  it('install commands carry the signed-in user and the credentials they need', () => {
    const raw = installCommands({ format: 'raw', url: 'https://h/repository/raw/', username: 'admin' }, { namespace: 'app/1.0.0', name: 'a.txt', version: '' });
    expect(raw[0]!.lines[0]!.text).toBe('curl -u admin:<token> -fLO https://h/repository/raw/app/1.0.0/a.txt');
    const pip = installCommands({ format: 'pypi', url: 'https://h/repository/p/' }, { name: 'x', version: '1' });
    expect(pip[0]!.lines[0]!.text).toBe('pip install x==1 --index-url https://__token__:<token>@h/repository/p/simple/');
    const gradle = installCommands({ format: 'maven', url: 'https://h/repository/m/', username: 'ci' }, { namespace: 'g', name: 'a', version: '1' }).find((x) => x.id === 'gradle')!;
    expect(text(gradle.lines)).toContain('        credentials { username = "ci"; password = System.getenv("HANGAR_TOKEN") }');
  });
});
