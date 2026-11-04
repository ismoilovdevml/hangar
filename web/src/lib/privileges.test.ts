import { describe, expect, it } from 'vitest';
import { can, canAdminRepo, globMatch } from './privileges';

describe('globMatch (path.Match without slashes)', () => {
  it.each([
    ['*', 'anything', true],
    ['npm-*', 'npm-hosted', true],
    ['npm-*', 'docker-hosted', false],
    ['docker-?', 'docker-a', true],
    ['docker-?', 'docker-ab', false],
    ['[a-c]-repo', 'b-repo', true],
    ['[^a-c]-repo', 'b-repo', false],
    ['exact.name', 'exactXname', false],
    ['a\\*b', 'a*b', true],
    ['[unclosed', 'u', false],
  ])('%s vs %s → %s', (pattern, name, want) => {
    expect(globMatch(pattern, name)).toBe(want);
  });
});

describe('can', () => {
  it('lets admin and * imply every action, on the matching domain only', () => {
    expect(can(['repo:*:admin'], 'delete', 'repo', 'npm-hosted')).toBe(true);
    expect(can(['repo:npm-*:*'], 'write', 'repo', 'npm-hosted')).toBe(true);
    expect(can(['repo:*:admin'], 'admin', 'system', 'repos')).toBe(false);
    expect(can(['system:*'], 'admin', 'system', 'blobstores')).toBe(true);
    expect(can(['system:*'], 'delete', 'repo', 'npm-hosted')).toBe(false);
    expect(can(['system:*:*'], 'admin', 'system', 'blobstores')).toBe(true);
  });

  it('does not let read or write grant delete', () => {
    expect(can(['repo:*:read', 'repo:*:write'], 'delete', 'repo', 'npm-hosted')).toBe(false);
    expect(can(['repo:npm-hosted:delete'], 'delete', 'repo', 'npm-hosted')).toBe(true);
    expect(can(['repo:npm-hosted:delete'], 'delete', 'repo', 'npm-other')).toBe(false);
  });

  it('ignores malformed privileges', () => {
    expect(can(['nonsense', 'repo:'], 'read', 'repo', 'x')).toBe(false);
  });

  it('allows repository administration to repo admins and system:repos admins', () => {
    expect(canAdminRepo(['repo:npm-*:admin'], 'npm-hosted')).toBe(true);
    expect(canAdminRepo(['system:repos:admin'], 'npm-hosted')).toBe(true);
    expect(canAdminRepo(['repo:*:delete'], 'npm-hosted')).toBe(false);
  });
});
