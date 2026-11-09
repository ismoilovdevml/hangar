import { describe, expect, it } from 'vitest';
import { formatBytes, formatCompact, formatSignedPercent, initials, relativeTime, splitBytes } from './format';
import { clientSetup } from './clientSnippets';

describe('format helpers', () => {
  it('formats bytes with SI units', () => {
    expect(splitBytes(4.82e12)).toEqual({ value: '4.82', unit: 'TB' });
    expect(formatBytes(12e12)).toBe('12 TB');
    expect(formatBytes(812e9)).toBe('812 GB');
    expect(formatBytes(2.1e12)).toBe('2.1 TB');
    expect(formatBytes(0)).toBe('0 B');
  });

  it('formats counts, trends, names and times', () => {
    expect(formatCompact(18_400_000)).toBe('18.4M');
    expect(formatCompact(950)).toBe('950');
    expect(formatSignedPercent(6)).toBe('+6%');
    expect(formatSignedPercent(-2.4)).toBe('−2%');
    expect(initials('Dilshod Ismoilov')).toBe('DI');
    const now = new Date('2026-11-03T09:00:00');
    expect(relativeTime('2026-11-03T08:59:30', now)).toBe('Just now');
    expect(relativeTime('2026-11-03T08:56:00', now)).toBe('4 min ago');
  });

  it('builds client setup lines from server-provided URLs', () => {
    const text = (lines: { text: string }[]) => lines.map((l) => l.text);
    const nuget = clientSetup({ format: 'nuget', repository: 'nuget-group', url: 'https://hangar.example.com/repository/nuget-group/', username: 'dilshod' });
    expect(text(nuget.setup)).toContain(
      'dotnet nuget add source https://hangar.example.com/repository/nuget-group/index.json -n nuget-group -u dilshod -p <token> --store-password-in-clear-text',
    );
    expect(nuget.publish).toEqual([]);

    const hosted = clientSetup({ format: 'nuget', repository: 'nuget-hosted', url: 'https://hangar.example.com/repository/nuget-hosted', type: 'hosted', publishes: true });
    expect(text(hosted.setup).join('\n')).toContain('-u <username> -p <token>');
    expect(text(hosted.publish)).toEqual(['dotnet nuget push <package>.nupkg -s https://hangar.example.com/repository/nuget-hosted/index.json -k <token>']);

    const npm = clientSetup({ format: 'npm', repository: 'npm-group', url: 'https://hangar.example.com/repository/npm-group/' });
    expect(text(npm.setup)).toEqual(
      expect.arrayContaining(['registry=https://hangar.example.com/repository/npm-group/', '//hangar.example.com/repository/npm-group/:_authToken=<token>']),
    );

    const pypi = clientSetup({ format: 'pypi', repository: 'pypi-group', url: 'https://hangar.example.com/repository/pypi-group/' });
    expect(text(pypi.setup)).toEqual(['pip config set global.index-url https://__token__:<token>@hangar.example.com/repository/pypi-group/simple/']);

    // From /me/client-endpoints docker comes as a bare registry host: log in, no guessed image path.
    const docker = clientSetup({ format: 'docker', repository: 'docker-group', host: 'hangar.example.com', username: 'ci' });
    expect(text(docker.setup)).toEqual(["echo '<token>' | docker login hangar.example.com -u ci --password-stdin"]);

    const maven = clientSetup({ format: 'maven', repository: 'maven-releases', url: 'https://hangar.example.com/repository/maven-releases/', type: 'hosted', publishes: true });
    expect(text(maven.setup).join('\n')).not.toContain('<mirror>');
    expect(text(maven.publish)).toContain('mvn deploy');

    const raw = clientSetup({ format: 'raw', repository: 'raw-hosted', url: 'https://hangar.example.com/repository/raw-hosted/', publishes: true, username: 'u' });
    expect(text(raw.publish)).toEqual(['curl -u u:<token> --upload-file ./<file> https://hangar.example.com/repository/raw-hosted/<path>/<file>']);

    const gomod = clientSetup({ format: 'go', repository: 'go-hosted', url: 'https://hangar.example.com/repository/go-hosted/', publishes: true });
    expect(text(gomod.setup)).toEqual(
      expect.arrayContaining(['go env -w GOPROXY=https://hangar.example.com/repository/go-hosted,direct', 'machine hangar.example.com login token password <token>']),
    );
    expect(text(gomod.publish).join('\n')).toContain('--upload-file <version>.zip https://hangar.example.com/repository/go-hosted/<module>/@v/<version>.zip');

    const crates = clientSetup({ format: 'cargo', repository: 'cargo-group', url: 'https://hangar.example.com/repository/cargo-group/', type: 'group' });
    expect(text(crates.setup)).toEqual(
      expect.arrayContaining([
        'global-credential-providers = ["cargo:token"]',
        '[registries.cargo-group]',
        'index = "sparse+https://hangar.example.com/repository/cargo-group/index/"',
        'replace-with = "cargo-group"',
        "echo '<token>' | cargo login --registry cargo-group",
      ]),
    );
    expect(text(crates.setup).join('\n')).toContain('CARGO_REGISTRIES_CARGO_GROUP_TOKEN');
    const crateHost = clientSetup({ format: 'cargo', repository: 'cargo-hosted', url: 'https://hangar.example.com/repository/cargo-hosted', type: 'hosted', publishes: true });
    expect(text(crateHost.setup).join('\n')).not.toContain('replace-with');
    expect(text(crateHost.publish)).toEqual(['cargo publish --registry cargo-hosted']);

    const deb = clientSetup({ format: 'apt', repository: 'apt-hosted', url: 'https://hangar.example.com/repository/apt-hosted/', type: 'hosted', publishes: true });
    expect(text(deb.setup)).toEqual(
      expect.arrayContaining([
        'curl -fsSL -u <username>:<token> https://hangar.example.com/repository/apt-hosted/public.gpg | sudo tee /etc/apt/keyrings/apt-hosted.gpg > /dev/null',
        'URIs: https://hangar.example.com/repository/apt-hosted/',
        'Signed-By: /etc/apt/keyrings/apt-hosted.gpg',
        "echo 'machine hangar.example.com login <username> password <token>' | sudo tee /etc/apt/auth.conf.d/apt-hosted.conf > /dev/null",
      ]),
    );
    expect(text(deb.publish).join('\n')).toContain('--upload-file ./<package>.deb "https://hangar.example.com/repository/apt-hosted/upload?distribution=stable&component=main"');
    const debProxy = clientSetup({ format: 'apt', repository: 'apt-proxy', url: 'https://hangar.example.com/repository/apt-proxy/', type: 'proxy' });
    expect(text(debProxy.setup).join('\n')).toContain('Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg');
    expect(text(debProxy.setup).join('\n')).not.toContain('public.gpg');

    const rpmHosted = clientSetup({ format: 'rpm', repository: 'rpm-hosted', url: 'https://hangar.example.com/repository/rpm-hosted/', type: 'hosted', publishes: true });
    expect(text(rpmHosted.setup)).toEqual(
      expect.arrayContaining([
        '[rpm-hosted]',
        'baseurl=https://hangar.example.com/repository/rpm-hosted/',
        'repo_gpgcheck=1',
        'gpgkey=https://hangar.example.com/repository/rpm-hosted/public.key',
        'username=<username>',
        'password=<token>',
      ]),
    );
    expect(text(rpmHosted.publish)).toEqual(['curl -u <username>:<token> --upload-file ./<package>.rpm https://hangar.example.com/repository/rpm-hosted/<package>.rpm']);
  });
});
