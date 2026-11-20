import { describe, expect, it } from 'vitest';
import { allows, canAdminRepo, globMatch, parsePrivilege, pathMatch } from './privileges';

/**
 * Go's own table for path.Match (src/path/match_test.go): the server matches
 * privilege patterns with it, so the client must agree on every row.
 */
const GO_MATCH_TESTS: [pattern: string, name: string, match: boolean, badPattern: boolean][] = [
  ['abc', 'abc', true, false],
  ['*', 'abc', true, false],
  ['*c', 'abc', true, false],
  ['a*', 'a', true, false],
  ['a*', 'abc', true, false],
  ['a*', 'ab/c', false, false],
  ['a*/b', 'abc/b', true, false],
  ['a*/b', 'a/c/b', false, false],
  ['a*b*c*d*e*/f', 'axbxcxdxe/f', true, false],
  ['a*b*c*d*e*/f', 'axbxcxdxexxx/f', true, false],
  ['a*b*c*d*e*/f', 'axbxcxdxe/xxx/f', false, false],
  ['a*b*c*d*e*/f', 'axbxcxdxexxx/fff', false, false],
  ['a*b?c*x', 'abxbbxdbxebxczzx', true, false],
  ['a*b?c*x', 'abxbbxdbxebxczzy', false, false],
  ['ab[c]', 'abc', true, false],
  ['ab[b-d]', 'abc', true, false],
  ['ab[e-g]', 'abc', false, false],
  ['ab[^c]', 'abc', false, false],
  ['ab[^b-d]', 'abc', false, false],
  ['ab[^e-g]', 'abc', true, false],
  ['a\\*b', 'a*b', true, false],
  ['a\\*b', 'ab', false, false],
  ['a?b', 'a☺b', true, false],
  ['a[^a]b', 'a☺b', true, false],
  ['a???b', 'a☺b', false, false],
  ['a[^a][^a][^a]b', 'a☺b', false, false],
  ['[a-ζ]*', 'α', true, false],
  ['*[a-ζ]', 'A', false, false],
  ['a?b', 'a/b', false, false],
  ['a*b', 'a/b', false, false],
  ['[\\]a]', ']', true, false],
  ['[\\-]', '-', true, false],
  ['[x\\-]', 'x', true, false],
  ['[x\\-]', '-', true, false],
  ['[x\\-]', 'z', false, false],
  ['[\\-x]', 'x', true, false],
  ['[\\-x]', '-', true, false],
  ['[\\-x]', 'a', false, false],
  ['[]a]', ']', false, true],
  ['[-]', '-', false, true],
  ['[x-]', 'x', false, true],
  ['[x-]', '-', false, true],
  ['[x-]', 'z', false, true],
  ['[-x]', 'x', false, true],
  ['[-x]', '-', false, true],
  ['[-x]', 'a', false, true],
  ['\\', 'a', false, true],
  ['[a-b-c]', 'a', false, true],
  ['[', 'a', false, true],
  ['[^', 'a', false, true],
  ['[^bc', 'a', false, true],
  ['a[', 'a', false, true],
  ['a[', 'ab', false, true],
  ['a[', 'x', false, true],
  ['a/b[', 'x', false, true],
  ['*x', 'xxx', true, false],
];

describe('pathMatch (Go path.Match)', () => {
  it.each(GO_MATCH_TESTS)('%s vs %s → match %s, bad pattern %s', (pattern, name, match, error) => {
    expect(pathMatch(pattern, name)).toEqual({ match, error });
  });

  it.each([
    ['npm-*', 'npm-hosted', true],
    ['npm-*', 'docker-hosted', false],
    ['docker-?', 'docker-a', true],
    ['docker-?', 'docker-ab', false],
    ['exact.name', 'exactXname', false],
    // Only "^" negates a class; "!" is an ordinary member, as in Go.
    ['npm-[!a]*', 'npm-a1', true],
    ['npm-[!a]*', 'npm-b1', false],
    ['[^a-c]-repo', 'b-repo', false],
    ['[unclosed', 'u', false],
  ])('repository pattern %s vs %s → %s', (pattern, name, want) => {
    expect(globMatch(pattern, name)).toBe(want);
  });
});

describe('parsePrivilege (core.ParsePrivilege)', () => {
  it.each([
    ['repo:npm-*:write', { domain: 'repo', pattern: 'npm-*', action: 'write' }],
    ['system:users:admin', { domain: 'system', pattern: 'users', action: 'admin' }],
    ['system:*', { domain: 'system', pattern: '*', action: '*' }],
    ['system:*:*', { domain: 'system', pattern: '*', action: '*' }],
    ['repo:*:*', { domain: 'repo', pattern: '*', action: '*' }],
  ])('parses %s', (s, want) => {
    expect(parsePrivilege(s)).toEqual(want);
  });

  it.each(['repo:*', 'repo:*:fly', 'team:x:read', 'repo::read', 'repo:[x-:read', 'system:users:read:extra', 'nonsense', 'repo:'])('refuses %s', (s) => {
    expect(parsePrivilege(s)).toBeNull();
  });
});

describe('allows (core.Privilege.Allows over every privilege)', () => {
  const cases: [privileges: string[] | undefined, domain: 'repo' | 'system', name: string, action: 'read' | 'write' | 'delete' | 'admin', want: boolean][] = [
    // admin and * imply every action, on the matching domain only.
    [['repo:*:admin'], 'repo', 'npm-hosted', 'delete', true],
    [['repo:npm-*:*'], 'repo', 'npm-hosted', 'write', true],
    [['repo:*:admin'], 'system', 'repos', 'admin', false],
    [['system:*'], 'system', 'blobstores', 'admin', true],
    [['system:*'], 'repo', 'npm-hosted', 'delete', false],
    [['system:*:*'], 'system', 'blobstores', 'admin', true],
    [['system:*:read'], 'system', 'audit', 'read', true],
    [['system:*:read'], 'system', 'audit', 'write', false],
    // read, write and delete do not imply each other or admin.
    [['repo:*:read', 'repo:*:write'], 'repo', 'npm-hosted', 'delete', false],
    [['repo:npm-hosted:delete'], 'repo', 'npm-hosted', 'delete', true],
    [['repo:npm-hosted:delete'], 'repo', 'npm-other', 'delete', false],
    [['system:users:read'], 'system', 'users', 'write', false],
    [['system:users:write'], 'system', 'users', 'admin', false],
    // Glob patterns as path.Match reads them.
    [['repo:npm-?:read'], 'repo', 'npm-a', 'read', true],
    [['repo:npm-[ab]:read'], 'repo', 'npm-c', 'read', false],
    [['repo:npm-[!a]*:read'], 'repo', 'npm-a1', 'read', true],
    [['repo:npm.x:read'], 'repo', 'npmax', 'read', false],
    [['system:tasks:admin'], 'system', 'tasks', 'write', true],
    // Malformed privileges grant nothing; neither does having none.
    [['nonsense', 'repo:', 'repo:[x-:read', 'repo:*:fly'], 'repo', 'x', 'read', false],
    [undefined, 'repo', 'x', 'read', false],
    [[], 'system', 'users', 'read', false],
  ];
  it.each(cases)('%j on %s:%s %s → %s', (privileges, domain, name, action, want) => {
    expect(allows(privileges, domain, name, action)).toBe(want);
  });

  it('lets repo admins and system:repos admins administer a repository', () => {
    expect(canAdminRepo(['repo:npm-*:admin'], 'npm-hosted')).toBe(true);
    expect(canAdminRepo(['system:repos:admin'], 'npm-hosted')).toBe(true);
    expect(canAdminRepo(['repo:*:delete'], 'npm-hosted')).toBe(false);
  });
});
