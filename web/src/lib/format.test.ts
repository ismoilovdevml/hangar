import { afterEach, describe, expect, it } from 'vitest';
import {
  formatBytes,
  formatClock,
  formatCompact,
  formatDate,
  formatDuration,
  formatShare,
  formatShortTime,
  formatSignedPercent,
  formatTime,
  formatUtc,
  initials,
  relativeFuture,
  relativeTime,
  shortGoDuration,
  splitBytes,
  tzOffsetMinutes,
  zoneLabel,
} from './format';
import { clientSetup } from './clientSnippets';

describe('format helpers', () => {
  it('formats bytes in IEC units', () => {
    expect(splitBytes(4.82 * 1024 ** 4)).toEqual({ value: '4.82', unit: 'TiB' });
    expect(formatBytes(12 * 1024 ** 4)).toBe('12 TiB');
    expect(formatBytes(812 * 1024 ** 3)).toBe('812 GiB');
    expect(formatBytes(1536)).toBe('1.5 KiB');
    expect(formatBytes(1000)).toBe('1000 B');
    expect(formatBytes(2 * 1024 ** 3)).toBe('2 GiB');
    expect(formatBytes(0)).toBe('0 B');
  });

  it('never rounds a non-zero share to 0%', () => {
    expect(formatShare(0.002)).toBe('<1%');
    expect(formatShare(0)).toBe('0%');
    expect(formatShare(0.42)).toBe('42%');
    expect(formatShare(0.998)).toBe('>99%');
    expect(formatShare(1)).toBe('100%');
  });

  it('formats durations without zero units', () => {
    expect(formatDuration(4_000)).toBe('4 s');
    expect(formatDuration(75_000)).toBe('1 min 15 s');
    expect(formatDuration(3_900_000)).toBe('1 h 5 min');
    expect(formatDuration(86_400_000)).toBe('1 d');
    expect(formatDuration(93_784_000)).toBe('1 d 2 h');
    expect(shortGoDuration('24h0m0s')).toBe('24h');
    expect(shortGoDuration('10m0s')).toBe('10m');
    expect(shortGoDuration('1h30m0s')).toBe('1h30m');
    expect(shortGoDuration('1h0m30s')).toBe('1h30s');
    expect(shortGoDuration('0s')).toBe('0s');
    expect(shortGoDuration('90s')).toBe('90s');
    expect(shortGoDuration('500ms')).toBe('500ms');
    expect(shortGoDuration('24h')).toBe('24h');
    expect(shortGoDuration('soon')).toBe('soon');
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

  describe('times in the browser zone', () => {
    const saved = process.env.TZ;
    afterEach(() => {
      process.env.TZ = saved;
    });

    it('shows Tashkent time with the zone and UTC on hover', () => {
      process.env.TZ = 'Asia/Tashkent';
      const at = '2026-11-11T07:23:05Z';
      expect(zoneLabel(new Date(at))).toBe('UTC+5');
      expect(tzOffsetMinutes(new Date(at))).toBe(300);
      expect(formatTime(at)).toBe('11 Nov 2026, 12:23 (UTC+5)');
      expect(formatTime(at, { seconds: true, zone: false })).toBe('11 Nov 2026, 12:23:05');
      expect(formatUtc(at)).toBe('11 Nov 2026, 07:23:05 UTC');
      expect(formatClock(at, { ms: true })).toBe('12:23:05.000');
      // 21:00 UTC is already the next day in Tashkent.
      expect(formatDate('2026-11-11T21:00:00Z')).toBe('12 Nov 2026');
      const now = new Date('2026-11-11T09:00:00Z');
      expect(formatShortTime(at, now)).toBe('12:23');
      expect(formatShortTime('2026-11-10T18:00:00Z', now)).toBe('10 Nov 23:00');
      expect(relativeFuture('2026-11-11T09:14:00Z', now)).toBe('In 14 min');
      expect(relativeFuture('2026-11-11T21:00:00Z', now)).toBe('Tomorrow 02:00');
    });

    it('labels other offsets', () => {
      process.env.TZ = 'Asia/Kolkata';
      expect(zoneLabel(new Date('2026-11-11T00:00:00Z'))).toBe('UTC+5:30');
      process.env.TZ = 'America/Sao_Paulo';
      expect(zoneLabel(new Date('2026-11-11T00:00:00Z'))).toBe('UTC-3');
      process.env.TZ = 'UTC';
      expect(formatTime('2026-11-11T07:23:05Z')).toBe('11 Nov 2026, 07:23 (UTC)');
    });
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
