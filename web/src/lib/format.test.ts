import { describe, expect, it } from 'vitest';
import { formatBytes, formatCompact, formatSignedPercent, greeting, initials, relativeTime, splitBytes } from './format';
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
    expect(greeting(now)).toBe('Good morning');
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
  });
});
