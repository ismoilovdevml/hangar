import { describe, expect, it } from 'vitest';
import {
  allows,
  buildScopes,
  customExpiry,
  describeAudit,
  describePrivilege,
  describeUserAgent,
  EMPTY_SCOPES,
  expiryPresets,
  expiryText,
  globMatch,
  parseGoDuration,
  parsePrivilege,
  repoPatternError,
  tokenState,
  uncoveredScopes,
  userStatus,
} from './model';

const NOW = new Date('2026-11-04T10:00:00Z');
const DAY = 86_400_000;

describe('privileges', () => {
  it('parses and describes the privilege forms', () => {
    expect(parsePrivilege('repo:npm-*:write')).toEqual({ domain: 'repo', pattern: 'npm-*', action: 'write' });
    expect(parsePrivilege('system:*')).toEqual({ domain: 'system', pattern: '*', action: '*' });
    expect(parsePrivilege('repo:*:fly')).toBeNull();
    expect(parsePrivilege('team:x:read')).toBeNull();
    expect(describePrivilege('repo:npm-*:write')).toBe('Publish to repositories matching npm-*');
    expect(describePrivilege('repo:*:read')).toBe('Read every repository');
    expect(describePrivilege('repo:docker-prod:admin')).toBe('Administer repository docker-prod');
    expect(describePrivilege('system:audit:read')).toBe('Read access to Audit log');
    expect(describePrivilege('system:*')).toBe('Full control of every system area');
  });

  it('matches globs like the server and checks rights', () => {
    expect(globMatch('npm-*', 'npm-internal')).toBe(true);
    expect(globMatch('npm-*', 'docker-npm')).toBe(false);
    expect(globMatch('npm-[ab]?', 'npm-a1')).toBe(true);
    expect(globMatch('npm-[!a]*', 'npm-a1')).toBe(false);
    expect(globMatch('a.b', 'axb')).toBe(false);
    expect(allows(['repo:*:admin', 'system:*'], 'system', 'users', 'delete')).toBe(true);
    expect(allows(['repo:*:read'], 'system', 'users', 'read')).toBe(false);
    expect(allows(['system:users:read'], 'system', 'users', 'write')).toBe(false);
    expect(allows(undefined, 'repo', 'x', 'read')).toBe(false);
  });

  it('validates repository patterns', () => {
    expect(repoPatternError('npm-*')).toBeNull();
    expect(repoPatternError('')).toMatch(/Enter/);
    expect(repoPatternError('NPM')).toMatch(/lower-case/);
    expect(repoPatternError('npm-[ab')).toMatch(/Unbalanced/);
  });
});

describe('token scopes', () => {
  it('builds scopes from the builder', () => {
    expect(buildScopes(EMPTY_SCOPES)).toEqual({ scopes: ['repo:*:read'], error: null });
    expect(buildScopes({ ...EMPTY_SCOPES, repoMode: 'pattern', pattern: 'npm-*', actions: ['read', 'write'] }).scopes).toEqual([
      'repo:npm-*:read',
      'repo:npm-*:write',
    ]);
    expect(buildScopes({ ...EMPTY_SCOPES, repoMode: 'list', repos: ['a1', 'b2'], actions: ['read', 'admin'] }).scopes).toEqual([
      'repo:a1:admin',
      'repo:b2:admin',
    ]);
    expect(buildScopes({ ...EMPTY_SCOPES, repoMode: 'none', system: ['audit:read', '*:*'] }).scopes).toEqual(['system:audit:read', 'system:*']);
    expect(buildScopes({ ...EMPTY_SCOPES, repoMode: 'none' }).error).toMatch(/at least one scope/);
    expect(buildScopes({ ...EMPTY_SCOPES, actions: [] }).error).toMatch(/repository action/);
    expect(buildScopes({ ...EMPTY_SCOPES, repoMode: 'list' }).error).toMatch(/at least one repository/);
    expect(buildScopes({ ...EMPTY_SCOPES, repoMode: 'pattern', pattern: 'Bad Name' }).error).toMatch(/lower-case/);
  });

  it('flags scopes the owner’s roles do not grant', () => {
    expect(uncoveredScopes(['repo:npm-*:write', 'system:users:read'], ['repo:*:read', 'repo:npm-*:write'])).toEqual(['system:users:read']);
    expect(uncoveredScopes(['repo:docker-prod:delete'], ['repo:*:admin'])).toEqual([]);
    expect(uncoveredScopes(['system:audit:read'], ['system:*'])).toEqual([]);
    expect(uncoveredScopes(['repo:*:write'], ['repo:npm-*:write'])).toEqual(['repo:*:write']);
  });
});

describe('expiry', () => {
  it('offers presets within the maximum lifetime', () => {
    expect(expiryPresets(366)).toEqual([7, 30, 90, 180, 365, 366]);
    expect(expiryPresets(90)).toEqual([7, 30, 90]);
    expect(expiryPresets(45)).toEqual([7, 30, 45]);
  });

  it('validates custom dates', () => {
    expect(customExpiry('2026-12-01', 365, NOW)).toEqual({ value: '2026-12-01T23:59:59Z', error: null });
    expect(customExpiry('2026-11-01', 365, NOW).error).toMatch(/future/);
    expect(customExpiry('2028-11-01', 365, NOW).error).toMatch(/At most 365 days/);
    expect(customExpiry('', 365, NOW).error).toMatch(/Pick a date/);
  });

  it('parses Go durations', () => {
    expect(parseGoDuration('336h0m0s')).toBe(14 * DAY);
    expect(parseGoDuration('1h30m')).toBe(5_400_000);
    expect(parseGoDuration('90s')).toBe(90_000);
    expect(parseGoDuration('soon')).toBeNull();
    expect(parseGoDuration(undefined)).toBeNull();
  });

  it('classifies and describes expiry', () => {
    expect(tokenState({ expires_at: new Date(NOW.getTime() + 30 * DAY).toISOString() }, NOW)).toBe('active');
    expect(tokenState({ expires_at: new Date(NOW.getTime() + 3 * DAY).toISOString() }, NOW)).toBe('expiring');
    expect(tokenState({ expires_at: new Date(NOW.getTime() - 1000).toISOString() }, NOW)).toBe('expired');
    expect(expiryText(new Date(NOW.getTime() + 28 * DAY).toISOString(), NOW)).toBe('in 28 days');
    expect(expiryText(new Date(NOW.getTime() - 3 * 3_600_000).toISOString(), NOW)).toBe('3 h ago');
  });
});

describe('users and audit', () => {
  it('derives the user status', () => {
    expect(userStatus({ disabled: false, locked_until: null }, NOW)).toBe('active');
    expect(userStatus({ disabled: true, locked_until: null }, NOW)).toBe('disabled');
    expect(userStatus({ disabled: false, locked_until: '2026-11-04T10:05:00Z' }, NOW)).toBe('locked');
    expect(userStatus({ disabled: false, locked_until: '2026-11-04T09:55:00Z' }, NOW)).toBe('active');
  });

  it('describes browsers and audit events in plain words', () => {
    expect(describeUserAgent('Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/537.36 Chrome/131.0 Safari/537.36')).toBe('Chrome on macOS');
    expect(describeUserAgent('Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0')).toBe('Firefox on Linux');
    expect(describeUserAgent('')).toBe('Unknown device');
    expect(describeAudit('role.bind', 'role:Publisher', { user: 'malika' })).toBe('granted role Publisher to malika');
    expect(describeAudit('token.create', 'token:t1', { name: 'laptop' })).toBe('created token “laptop”');
    expect(describeAudit('user.disable', 'user:bob')).toBe('disabled bob');
    expect(describeAudit('auth.login.failed', 'user:admin')).toBe('failed to sign in');
  });
});
