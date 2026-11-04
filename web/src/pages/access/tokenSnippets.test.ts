import { describe, expect, it } from 'vitest';
import { repositories } from '../../mocks/data';
import { automationSnippet, clientSnippets, curlSnippet, envSnippet, serverBaseUrl } from './tokenSnippets';

const SECRET = 'hgr_TestSecretValue';
const text = (lines: { text: string }[]) => lines.map((l) => l.text).join('\n');

describe('token reveal snippets', () => {
  it('uses the server URL, else the page origin', () => {
    expect(serverBaseUrl('https://hangar.example.com/', 'http://localhost:5173')).toBe('https://hangar.example.com');
    expect(serverBaseUrl('', 'http://localhost:5173')).toBe('http://localhost:5173');
    expect(serverBaseUrl(undefined, 'http://localhost:5173/')).toBe('http://localhost:5173');
  });

  it('puts the real URL and secret into curl, env and automation blocks', () => {
    const base = 'https://hangar.example.com';
    expect(text(curlSnippet(base, SECRET))).toContain(`curl -fsS -H "Authorization: Bearer ${SECRET}" ${base}/api/v1/auth/session`);
    expect(text(envSnippet(base, SECRET))).toBe(`HANGAR_URL=${base}\nHANGAR_TOKEN=${SECRET}`);
    const auto = text(automationSnippet(base, SECRET));
    expect(auto).toContain(`${base}/llms.txt`);
    expect(auto).toContain(`${base}/api/v1/openapi.yaml`);
    expect(auto).toContain(`HANGAR_TOKEN=${SECRET}`);
  });

  it('configures one client per format the token can read, publishing where it can write', () => {
    const repos = repositories();
    const all = clientSnippets(repos, ['repo:*:read', 'repo:npm-internal:write'], false, 'ci-bot', SECRET);
    expect(all.map((s) => s.format)).toEqual(['npm', 'nuget', 'pypi', 'maven', 'docker']);
    const npm = all.find((s) => s.format === 'npm')!;
    expect(npm).toMatchObject({ repository: 'npm-internal', publishes: true });
    expect(text(npm.lines)).toContain(`:_authToken=${SECRET}`);
    expect(text(npm.lines)).toContain('npm publish');
    const docker = all.find((s) => s.format === 'docker')!;
    expect(docker.publishes).toBe(false);
    expect(text(docker.lines)).toContain(`echo '${SECRET}' | docker login`);
    expect(text(docker.lines)).toContain('-u ci-bot');

    // Read-only tokens never get publish commands; formats out of scope are left out.
    const ro = clientSnippets(repos, ['repo:npm-*:write', 'repo:npm-*:read'], true, 'ci-bot', SECRET);
    expect(ro.map((s) => s.format)).toEqual(['npm']);
    expect(ro[0]!.publishes).toBe(false);
    expect(clientSnippets(repos, ['system:audit:read'], false, 'x', SECRET)).toEqual([]);
  });
});
