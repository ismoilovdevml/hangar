import { describe, expect, it } from 'vitest';
import { ApiRequestError } from '../../api/client';
import {
  anchored,
  conditionText,
  GITHUB_ISSUER,
  GITLAB_ISSUER,
  newPolicyForm,
  policyFieldErrors,
  policyForm,
  policyInput,
  presetFor,
  PRESETS,
  ttlText,
  usageSnippets,
  validatePolicy,
} from './trustPolicyModel';
import type { TrustPolicy } from './trustPoliciesApi';

const policy: TrustPolicy = {
  id: 'tp',
  name: 'app',
  description: '',
  enabled: true,
  issuer: GITHUB_ISSUER,
  audience: 'https://hangar.example.com',
  conditions: [{ claim: 'repository', match: 'exact', values: ['acme/app'] }],
  service_account: 'ci',
  scopes: ['repo:npm-hosted:write'],
  max_ttl_seconds: 900,
  created_at: '2026-11-01T00:00:00Z',
  updated_at: '2026-11-01T00:00:00Z',
};

const refused = (message: string) => policyFieldErrors(new ApiRequestError(400, { code: 'invalid_argument', message }));

describe('trust policy form', () => {
  it('round-trips a stored policy', () => {
    const f = policyForm(policy);
    expect(validatePolicy(f)).toEqual({});
    expect(policyInput(f)).toEqual({
      name: 'app',
      description: '',
      enabled: true,
      issuer: GITHUB_ISSUER,
      audience: 'https://hangar.example.com',
      conditions: [{ claim: 'repository', match: 'exact', values: ['acme/app'] }],
      service_account: 'ci',
      scopes: ['repo:npm-hosted:write'],
      max_ttl_seconds: 900,
    });
    // An empty audience is left to the server's default.
    expect(policyInput({ ...f, audience: ' ' })).not.toHaveProperty('audience');
  });

  it('picks the preset of an issuer', () => {
    expect(presetFor(`${GITHUB_ISSUER}/`).id).toBe('github');
    expect(presetFor(GITLAB_ISSUER).id).toBe('gitlab');
    expect(presetFor('https://gitlab.acme.example').id).toBe('other');
    expect(newPolicyForm(PRESETS[1]).conditions.map((c) => c.claim)).toEqual(['project_path', 'ref_protected']);
  });

  it('requires the owner to be pinned on public platforms, as the server does', () => {
    expect(anchored('exact', 'acme')).toBe(true);
    expect(anchored('glob', 'acme/*')).toBe(true);
    expect(anchored('glob', 'acme*')).toBe(false);
    expect(anchored('glob', '*/app')).toBe(false);
    const f = policyForm(policy);
    expect(validatePolicy({ ...f, conditions: [{ claim: 'ref', match: 'exact', values: ['refs/heads/main'] }] }).conditions).toMatch(/GitHub Actions signs jobs of every account/);
    expect(validatePolicy({ ...f, conditions: [{ claim: 'repository', match: 'glob', values: ['acme*'] }] }).conditions).toBeDefined();
    expect(validatePolicy({ ...f, conditions: [{ claim: 'repository', match: 'glob', values: ['acme/*'] }] })).toEqual({});
    // Another issuer only needs a condition.
    expect(validatePolicy({ ...f, issuer: 'https://ci.acme.example', conditions: [{ claim: 'ref', match: 'exact', values: ['main'] }] })).toEqual({});
    expect(validatePolicy({ ...f, issuer: 'https://ci.acme.example', conditions: [] }).conditions).toMatch(/at least one condition/);
  });

  it('checks the other fields before sending', () => {
    const f = policyForm(policy);
    expect(validatePolicy({ ...f, issuer: 'http://insecure.example' }).issuer).toBeDefined();
    expect(validatePolicy({ ...f, scopes: [] }).scopes).toBeDefined();
    expect(validatePolicy({ ...f, scopes: ['repo:x'] }).scopes).toMatch(/is not a privilege/);
    expect(validatePolicy({ ...f, ttlMinutes: '1.5' }).ttl).toBeUndefined();
    expect(policyInput({ ...f, ttlMinutes: '1.5' }).max_ttl_seconds).toBe(90);
    for (const t of ['0', '0.5', '61', 'x']) expect(validatePolicy({ ...f, ttlMinutes: t }).ttl).toBeDefined();
    expect(validatePolicy({ ...f, conditions: [{ claim: 'ref', match: 'glob', values: ['*'] }, ...f.conditions] })['conditions[0]']).toMatch(/matches anything/);
    expect(validatePolicy({ ...f, serviceAccount: '' }).service_account).toBeDefined();
  });

  it('puts the server’s refusals at their fields', () => {
    expect(refused('conditions: this issuer signs jobs of every account on the platform: add a condition')).toEqual({
      conditions: 'This issuer signs jobs of every account on the platform: add a condition.',
    });
    expect(refused('conditions: condition 3 (ref): at least one value is required')).toEqual({ 'conditions[2]': 'At least one value is required.' });
    expect(refused('name "a b": 1 to 64 letters')).toEqual({ name: '1 to 64 letters.' });
    expect(refused('name: a trust policy cannot be renamed')).toEqual({ name: 'A trust policy cannot be renamed.' });
    expect(Object.keys(refused('issuer "http://x" must be an https URL'))).toEqual(['issuer']);
    expect(refused('max_ttl_seconds 5: between 60 and 3600')).toEqual({ ttl: 'Between 60 and 3600.' });
    expect(Object.keys(refused('privilege repo:x: unknown domain'))).toEqual(['scopes']);
    expect(Object.keys(refused('service account "ci" is disabled'))).toEqual(['service_account']);
    expect(refused('something else')).toEqual({});
  });

  it('describes conditions and lifetimes', () => {
    expect(conditionText({ claim: 'ref', match: 'glob', values: ['refs/tags/v*', 'refs/heads/main'] })).toBe('ref ~ refs/tags/v* or refs/heads/main');
    expect(ttlText(900)).toBe('15 min');
    expect(ttlText(3600)).toBe('1 hour');
    expect(ttlText(90)).toBe('90 s');
  });

  it('writes snippets for the platform of the issuer, naming the policy and its audience', () => {
    const gh = usageSnippets(policy, 'https://hangar.example.com');
    expect(gh.map((s) => s.title)).toEqual(['GitHub Actions workflow', 'Exchange with curl']);
    const text = (i: number) => gh[i]!.lines.map((l) => l.text).join('\n');
    expect(text(0)).toContain('audience=https://hangar.example.com');
    expect(text(0)).toContain('POST https://hangar.example.com/api/v1/auth/oidc/exchange');
    expect(text(1)).toContain('\\"policy\\": \\"app\\"');
    expect(usageSnippets({ ...policy, issuer: GITLAB_ISSUER }, 'https://h').map((s) => s.title)).toEqual(['GitLab CI job', 'Exchange with curl']);
    expect(usageSnippets({ ...policy, issuer: 'https://ci.acme.example' }, 'https://h').map((s) => s.title)).toEqual(['Exchange with curl']);
  });
});
