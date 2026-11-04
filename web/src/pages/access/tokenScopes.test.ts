import { describe, expect, it } from 'vitest';
import {
  DEFAULT_DRAFT,
  draftScopes,
  effectiveScopes,
  levelAllowed,
  networkError,
  ownerCanRepo,
  ownerLevel,
  parseNetworks,
  presetAvailability,
  rotatedLifetimeDays,
  shortSummary,
  summarizeScopes,
  type PermissionDraft,
} from './tokenScopes';

const draft = (d: Partial<PermissionDraft>): PermissionDraft => ({ ...DEFAULT_DRAFT, ...d });

describe('token presets', () => {
  it('maps every preset to scopes', () => {
    expect(draftScopes(draft({ preset: 'read' }))).toEqual({ scopes: ['repo:*:read'], error: null });
    expect(draftScopes(draft({ preset: 'publish', repos: ['nuget-internal', 'npm-*'] })).scopes).toEqual([
      'repo:nuget-internal:read',
      'repo:nuget-internal:write',
      'repo:npm-*:read',
      'repo:npm-*:write',
    ]);
    expect(draftScopes(draft({ preset: 'repo-admin', repos: ['docker-prod'] })).scopes).toEqual(['repo:docker-prod:admin']);
    expect(draftScopes(draft({ preset: 'security' })).scopes).toEqual(['repo:*:read', 'system:security:read', 'system:security:write']);
    expect(draftScopes(draft({ preset: 'admin' })).scopes).toEqual(['system:*', 'repo:*:admin']);
  });

  it('asks for repositories where the preset needs them', () => {
    expect(draftScopes(draft({ preset: 'publish' })).error).toMatch(/at least one repository/);
    expect(draftScopes(draft({ preset: 'repo-admin', repos: ['Bad Name'] })).error).toMatch(/lower-case/);
  });

  it('builds custom scopes from repositories, actions and system areas', () => {
    const d = draft({ preset: 'custom', repos: ['npm-*'], repoActions: ['read', 'write'], system: { audit: 'read', users: 'write', tokens: 'admin' } });
    expect(draftScopes(d).scopes).toEqual([
      'repo:npm-*:read',
      'repo:npm-*:write',
      'system:users:read',
      'system:users:write',
      'system:tokens:admin',
      'system:audit:read',
    ]);
    // admin covers the other actions.
    expect(draftScopes(draft({ preset: 'custom', repos: ['a1'], repoActions: ['read', 'admin'] })).scopes).toEqual(['repo:a1:admin']);
    expect(draftScopes(draft({ preset: 'custom', system: { audit: 'read' } })).scopes).toEqual(['system:audit:read']);
    expect(draftScopes(draft({ preset: 'custom' })).error).toMatch(/at least one permission/);
    expect(draftScopes(draft({ preset: 'custom', repos: ['a1'], repoActions: [] })).error).toMatch(/may do/);
  });
});

describe("the owner's limits", () => {
  const reader = ['repo:*:read'];
  const publisher = ['repo:*:read', 'repo:npm-*:write'];
  const admin = ['repo:*:admin', 'system:*'];

  it('grey out presets the owner cannot use, with a reason', () => {
    expect(presetAvailability('read', reader)).toEqual({ ok: true });
    expect(presetAvailability('publish', reader)).toMatchObject({ ok: false, reason: expect.stringMatching(/publish/) });
    expect(presetAvailability('publish', publisher)).toEqual({ ok: true });
    expect(presetAvailability('admin', publisher)).toMatchObject({ ok: false, reason: expect.stringMatching(/administrators/) });
    expect(presetAvailability('security', reader).ok).toBe(false);
    expect(presetAvailability('admin', admin)).toEqual({ ok: true });
    expect(presetAvailability('security', admin)).toEqual({ ok: true });
    expect(presetAvailability('custom', [])).toEqual({ ok: true });
  });

  it('knows the highest system level and repository rights', () => {
    expect(ownerLevel(admin, 'users')).toBe('admin');
    expect(ownerLevel(['system:audit:read'], 'audit')).toBe('read');
    expect(ownerLevel(['system:users:read', 'system:users:write'], 'users')).toBe('write');
    expect(ownerLevel(reader, 'users')).toBe('');
    expect(levelAllowed('read', 'write')).toBe(false);
    expect(levelAllowed('admin', 'write')).toBe(true);
    expect(levelAllowed('', '')).toBe(true);
    expect(ownerCanRepo(publisher, 'npm-internal', 'write')).toBe(true);
    expect(ownerCanRepo(publisher, 'docker-prod', 'write')).toBe(false);
    expect(ownerCanRepo(publisher, 'npm-*', 'write')).toBe(true);
    expect(ownerCanRepo(publisher, '*', 'write')).toBe(false);
  });
});

describe('plain-language summary', () => {
  it('says what the token can and cannot do', () => {
    const s = summarizeScopes(['repo:npm-*:read', 'repo:nuget-internal:read', 'repo:nuget-internal:write']);
    expect(s.can).toEqual(['Can publish to nuget-internal', 'Can download from npm-* and nuget-internal']);
    expect(s.cannot).toEqual(['Cannot manage users & roles, tokens, settings and repositories']);
  });

  it('collapses admin and every repository', () => {
    expect(summarizeScopes(['repo:*:read', 'repo:npm-x:read', 'repo:npm-x:admin']).can).toEqual(['Can manage npm-x', 'Can download from every repository']);
    const full = summarizeScopes(['system:*', 'repo:*:admin']);
    expect(full.can).toEqual(['Can manage every repository', 'Can manage every administration area: users, tokens, settings and more']);
    expect(full.cannot).toEqual([]);
  });

  it('describes system areas by level', () => {
    const s = summarizeScopes(['system:audit:read', 'system:users:read', 'system:users:write', 'system:tokens:admin']);
    expect(s.can).toEqual(['Can manage tokens', 'Can view and change users & roles', 'Can view audit log']);
    expect(s.cannot).toEqual(['Cannot publish packages', 'Cannot manage users & roles, settings and repositories']);
  });

  it('narrows read-only tokens', () => {
    expect(effectiveScopes(['repo:npm-*:write', 'system:*'], true)).toEqual(['repo:npm-*:read', 'system:*:read']);
    const s = summarizeScopes(['repo:npm-*:write'], true);
    expect(s.can).toEqual(['Can download from npm-*']);
    expect(s.cannot[0]).toMatch(/read-only/);
  });

  it('names the preset in the token list', () => {
    expect(shortSummary(['repo:*:read'])).toBe('Read only');
    expect(shortSummary(['repo:*:admin', 'system:*'])).toBe('Full admin');
    expect(shortSummary(['repo:*:read', 'system:security:read', 'system:security:write'])).toBe('Security reviewer');
    expect(shortSummary(['repo:docker-prod:write'])).toBe('Can publish to docker-prod');
    expect(shortSummary(['repo:a:write', 'repo:b:read'])).toBe('Can publish to a (+1 more)');
  });
});

describe('restrictions and rotation', () => {
  it('validates allowed networks', () => {
    expect(parseNetworks('10.0.0.0/8,\n 203.0.113.7  2001:db8::/48')).toEqual(['10.0.0.0/8', '203.0.113.7', '2001:db8::/48']);
    expect(networkError('10.0.0.0/8')).toBeNull();
    expect(networkError('2001:db8::/48')).toBeNull();
    expect(networkError('10.0.0.0/33')).toMatch(/not a valid IPv4/);
    expect(networkError('300.1.1.1')).toMatch(/not a valid IPv4/);
    expect(networkError('office')).toMatch(/not an IP address/);
  });

  it('keeps the lifetime of the rotated token within the server maximum', () => {
    expect(rotatedLifetimeDays({ created_at: '2026-01-01T00:00:00Z', expires_at: '2026-01-31T00:00:00Z' }, 366)).toBe(30);
    expect(rotatedLifetimeDays({ created_at: '2025-01-01T00:00:00Z', expires_at: '2027-01-01T00:00:00Z' }, 366)).toBe(366);
    expect(rotatedLifetimeDays({ created_at: '2026-01-01T00:00:00Z', expires_at: '2026-01-01T02:00:00Z' }, 366)).toBe(1);
  });
});
