import { describe, expect, it } from 'vitest';
import { initialRepoForm, repositoryYaml, toRepositoryInput, validateRepoForm, type RepoForm } from './repositories';
import { toYaml, YamlComment } from './yaml';

const form = (patch: Partial<RepoForm>): RepoForm => ({ ...initialRepoForm('npm', 'hosted'), name: 'npm-internal', blobStore: 'fs-local', ...patch });

describe('repository form rules', () => {
  it('accepts a complete hosted repository', () => {
    expect(validateRepoForm(form({}))).toEqual({});
  });

  it('checks the name pattern the server enforces', () => {
    expect(validateRepoForm(form({ name: 'a' })).name).toMatch(/2–63/);
    expect(validateRepoForm(form({ name: '-npm' })).name).toBeDefined();
    expect(validateRepoForm(form({ name: 'NPM' })).name).toBeDefined();
    expect(validateRepoForm(form({ name: 'x'.repeat(64) })).name).toBeDefined();
    expect(validateRepoForm(form({ name: '9-lives' })).name).toBeUndefined();
  });

  it('accepts Go durations only', () => {
    const proxy = (ttl: string) => validateRepoForm(form({ type: 'proxy', metadataTtl: ttl })).metadataTtl;
    for (const ok of ['0', '0s', '90s', '1h30m', '24h', '1.5h', '500ms']) expect(proxy(ok)).toBeUndefined();
    for (const bad of ['7d', '1 h', 'h', '24']) expect(proxy(bad)).toBeDefined();
  });

  it('allows writes through a group only to a hosted member', () => {
    const types = new Map([['npm-internal', 'hosted'], ['npmjs-proxy', 'proxy']] as const);
    const g = (writeMember: string) =>
      validateRepoForm(form({ type: 'group', name: 'npm-all', members: ['npm-internal', 'npmjs-proxy'], writeMember }), { memberTypes: types });
    expect(g('npm-internal').writeMember).toBeUndefined();
    expect(g('npmjs-proxy').writeMember).toBe('Writes can only go to a hosted member.');
    expect(g('other').writeMember).toBe('The write member must be one of the members.');
  });

  it('only sends a write member for formats that publish through groups', () => {
    const f = form({ format: 'maven', type: 'group', name: 'maven-all', members: ['a'], writeMember: 'a' });
    expect(toRepositoryInput(f).config.group).toEqual({ members: ['a'] });
  });
});

describe('YAML preview', () => {
  it('quotes strings a YAML reader would take for something else', () => {
    expect(toYaml({ a: 'yes', b: '1.0', c: '24h', d: '', e: '#x', f: 'a: b', g: '2026-01-01', h: 'npm-proxy', i: '0755' })).toBe(
      'a: "yes"\nb: "1.0"\nc: 24h\nd: ""\ne: "#x"\nf: "a: b"\ng: "2026-01-01"\nh: npm-proxy\ni: "0755"\n',
    );
  });

  it('writes lists, nested maps, comments and literal blocks', () => {
    expect(toYaml({ m: ['a', 'n'], e: [], nested: { k: true, c: new YamlComment('note') }, pem: 'L1\nL2\n' })).toBe(
      'm:\n  - a\n  - "n"\ne: []\nnested:\n  k: true\n  # note\npem: |\n  L1\n  L2\n',
    );
  });

  it('describes the repository in the shape the API accepts, without secrets', () => {
    const f = form({
      type: 'proxy',
      name: 'npm-proxy',
      remoteUrl: 'https://user:pw@registry.example/npm',
      authKind: 'basic',
      authUsername: 'ci',
      authPassword: 'hunter2',
      proxyMode: 'url',
      proxyUrl: 'http://u:p@proxy:3128',
    });
    const y = repositoryYaml(toRepositoryInput(f));
    expect(y).not.toContain('hunter2');
    expect(y).not.toContain(':pw@');
    expect(y).not.toContain('u:p@');
    expect(y).toContain('  proxy:\n    remote_url: https://xxxxx:xxxxx@registry.example/npm\n');
    expect(y).toContain('      kind: basic\n      username: ci\n      # password: write-only, not shown\n');
    expect(y.startsWith('name: npm-proxy\nformat: npm\ntype: proxy\nblob_store: fs-local\nonline: true\nconfig:\n')).toBe(true);
  });
});
