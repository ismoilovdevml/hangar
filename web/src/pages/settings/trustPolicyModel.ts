/**
 * Pure helpers of the CI trust policies page: presets for the common CI
 * platforms, the edit form and its checks, the server's refusals mapped onto
 * fields, and the usage snippets a pipeline copies.
 */
import { ApiRequestError } from '../../api/client';
import { cleanServerMessage } from '../../lib/apiError';
import type { CodeLine } from '../../lib/clientSnippets';
import { parsePrivilege } from '../../lib/privileges';
import type { TrustPolicy, TrustPolicyInput } from './trustPoliciesApi';

export const GITHUB_ISSUER = 'https://token.actions.githubusercontent.com';
export const GITLAB_ISSUER = 'https://gitlab.com';

export type MatchKind = 'exact' | 'glob';

export interface ConditionRow {
  claim: string;
  match: MatchKind;
  values: string[];
}

export type PresetId = 'github' | 'gitlab' | 'other';

export interface Preset {
  id: PresetId;
  label: string;
  issuer: string;
  /** Claims the platform puts in its ID tokens, offered as suggestions. */
  claims: string[];
  /** Claims of which one must pin the owner with an exact value or an anchored glob (server: ownerClaims). */
  ownerClaims: string[];
  conditions: ConditionRow[];
  /** Placeholder per claim for the values field. */
  examples: Record<string, string>;
}

export const PRESETS: Preset[] = [
  {
    id: 'github',
    label: 'GitHub Actions',
    issuer: GITHUB_ISSUER,
    claims: ['repository', 'repository_owner', 'repository_id', 'repository_owner_id', 'ref', 'ref_type', 'environment', 'workflow_ref', 'event_name', 'sub'],
    ownerClaims: ['repository', 'repository_owner', 'repository_id', 'repository_owner_id', 'sub'],
    conditions: [
      { claim: 'repository', match: 'exact', values: [] },
      { claim: 'ref', match: 'glob', values: ['refs/heads/main', 'refs/tags/v*'] },
      { claim: 'environment', match: 'exact', values: ['production'] },
    ],
    examples: { repository: 'acme/app', repository_owner: 'acme', ref: 'refs/heads/main', environment: 'production', workflow_ref: 'acme/app/.github/workflows/release.yml@refs/heads/main' },
  },
  {
    id: 'gitlab',
    label: 'GitLab',
    issuer: GITLAB_ISSUER,
    claims: ['project_path', 'namespace_path', 'project_id', 'namespace_id', 'ref', 'ref_type', 'ref_protected', 'environment', 'sub'],
    ownerClaims: ['project_path', 'namespace_path', 'project_id', 'namespace_id', 'sub'],
    conditions: [
      { claim: 'project_path', match: 'exact', values: [] },
      { claim: 'ref_protected', match: 'exact', values: ['true'] },
    ],
    examples: { project_path: 'acme/app', namespace_path: 'acme', ref: 'main', ref_protected: 'true', environment: 'production' },
  },
  {
    id: 'other',
    label: 'Other issuer',
    issuer: '',
    claims: ['sub', 'ref', 'environment'],
    ownerClaims: [],
    conditions: [{ claim: '', match: 'exact', values: [] }],
    examples: {},
  },
];

/** The preset of an issuer: GitHub, gitlab.com, or any other (a self-managed GitLab counts as other). */
export function presetFor(issuer: string): Preset {
  const i = issuer.trim().replace(/\/+$/, '');
  return PRESETS.find((p) => p.id !== 'other' && p.issuer === i) ?? PRESETS[2]!;
}

export function issuerLabel(issuer: string): string {
  const p = presetFor(issuer);
  return p.id === 'other' ? issuer : p.label;
}

export const DEFAULT_TTL_SECONDS = 900;
export const MIN_TTL_SECONDS = 60;
export const MAX_TTL_SECONDS = 3600;

export interface PolicyForm {
  name: string;
  description: string;
  enabled: boolean;
  issuer: string;
  audience: string;
  conditions: ConditionRow[];
  serviceAccount: string;
  scopes: string[];
  /** Lifetime of minted tokens in minutes, as typed. */
  ttlMinutes: string;
}

export function newPolicyForm(preset: Preset = PRESETS[0]!): PolicyForm {
  return {
    name: '',
    description: '',
    enabled: true,
    issuer: preset.issuer,
    audience: '',
    conditions: preset.conditions.map((c) => ({ ...c, values: [...c.values] })),
    serviceAccount: '',
    scopes: [],
    ttlMinutes: String(DEFAULT_TTL_SECONDS / 60),
  };
}

const minutes = (seconds: number) => String(Math.round((seconds / 60) * 100) / 100);

export function policyForm(p: TrustPolicy): PolicyForm {
  return {
    name: p.name,
    description: p.description,
    enabled: p.enabled,
    issuer: p.issuer,
    audience: p.audience,
    conditions: p.conditions.map((c) => ({ claim: c.claim, match: c.match === 'glob' ? 'glob' : 'exact', values: [...c.values] })),
    serviceAccount: p.service_account,
    scopes: [...p.scopes],
    ttlMinutes: minutes(p.max_ttl_seconds),
  };
}

/** Errors keyed by field: name, description, issuer, audience, conditions, conditions[i], service_account, scopes, ttl. */
export type PolicyErrors = Record<string, string>;

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const CLAIM_RE = /^[A-Za-z0-9_:-]+(\.[A-Za-z0-9_:-]+)*$/;

/** A value that fixes an owner: exact, or a glob whose first wildcard follows a "/" (acme/*, not acme*). */
export function anchored(match: MatchKind, value: string): boolean {
  if (match === 'exact') return value !== '';
  const i = value.search(/[*?]/);
  return i < 0 || (i > 0 && value.slice(0, i).includes('/'));
}

/** Seconds from the minutes field; NaN when it is not a number. */
export const ttlSeconds = (f: PolicyForm) => Math.round(Number(f.ttlMinutes.trim()) * 60);

/** Problems the server would refuse, found before sending. */
export function validatePolicy(f: PolicyForm): PolicyErrors {
  const e: PolicyErrors = {};
  if (!NAME_RE.test(f.name.trim())) e.name = 'Use 1 to 64 letters, digits, ".", "_" or "-", starting with a letter or digit.';
  if (f.description.trim().length > 500) e.description = 'Keep it to 500 characters.';
  const issuer = f.issuer.trim();
  let issuerUrl: URL | null = null;
  try {
    issuerUrl = new URL(issuer);
  } catch {
    issuerUrl = null;
  }
  if (!issuerUrl || issuerUrl.protocol !== 'https:') e.issuer = 'Enter the exact https:// issuer of the ID tokens (their iss claim).';
  if (f.conditions.length === 0) e.conditions = 'Add at least one condition: without one, every job the issuer signs would be trusted.';
  f.conditions.forEach((c, i) => {
    if (!CLAIM_RE.test(c.claim.trim())) e[`conditions[${i}]`] = 'Name a claim, such as repository or a dotted path.';
    else if (c.values.length === 0) e[`conditions[${i}]`] = 'Add at least one value.';
    else if (c.match === 'glob' && c.values.some((v) => v.replace(/[*?]/g, '') === '')) e[`conditions[${i}]`] = 'A pattern of only * and ? matches anything; leave the condition out instead.';
  });
  const preset = presetFor(issuer);
  if (!e.conditions && preset.ownerClaims.length > 0 && !f.conditions.some((c) => preset.ownerClaims.includes(c.claim.trim()) && c.values.length > 0 && c.values.every((v) => anchored(c.match, v)))) {
    e.conditions = `${preset.label} signs jobs of every account on the platform: one condition on ${preset.ownerClaims.join(', ')} must fix the owner, with exact values or globs such as acme/* (not acme*).`;
  }
  if (!f.serviceAccount.trim()) e.service_account = 'Name the service account the minted tokens act as.';
  if (f.scopes.length === 0) e.scopes = 'Add at least one scope, such as repo:npm-hosted:write.';
  else {
    const bad = f.scopes.find((s) => parsePrivilege(s) === null);
    if (bad) e.scopes = `"${bad}" is not a privilege. Write repo:<repository or glob>:<read|write|delete|admin> or system:<area>:<read|admin>.`;
  }
  const ttl = ttlSeconds(f);
  if (!Number.isFinite(ttl) || ttl < MIN_TTL_SECONDS || ttl > MAX_TTL_SECONDS) e.ttl = 'Between 1 and 60 minutes.';
  return e;
}

export function policyInput(f: PolicyForm): TrustPolicyInput {
  const input: TrustPolicyInput = {
    name: f.name.trim(),
    description: f.description.trim(),
    enabled: f.enabled,
    issuer: f.issuer.trim().replace(/\/+$/, ''),
    conditions: f.conditions.map((c) => ({ claim: c.claim.trim(), match: c.match, values: [...c.values] })),
    service_account: f.serviceAccount.trim(),
    scopes: [...f.scopes],
    max_ttl_seconds: ttlSeconds(f),
  };
  if (f.audience.trim()) input.audience = f.audience.trim();
  return input;
}

/**
 * The server's refusal of a policy mapped onto its field. Its message starts
 * with the field ("conditions: this issuer signs jobs of every account …",
 * "service_account \"x\": no such service account", "privilege …"); the rest
 * becomes the sentence under the field.
 */
export function policyFieldErrors(err: unknown): PolicyErrors {
  if (!(err instanceof ApiRequestError) || err.status !== 400) return {};
  const msg = err.body?.message ?? '';
  const say = (s: string) => cleanServerMessage(s);
  const cond = /^conditions: condition (\d+)\b[^:]*:\s*(.+)$/.exec(msg);
  if (cond) return { [`conditions[${Number(cond[1]) - 1}]`]: say(cond[2]!) };
  const rules: [RegExp, string][] = [
    [/^conditions:\s*(.+)$/, 'conditions'],
    [/^name\b[^:]*:\s*(.+)$/, 'name'],
    [/^description:\s*(.+)$/, 'description'],
    [/^issuer\b/, 'issuer'],
    [/^audience:\s*(.+)$/, 'audience'],
    [/^max_ttl_seconds\b[^:]*:\s*(.+)$/, 'ttl'],
    [/^(?:scopes|privilege)\b/, 'scopes'],
    [/^service[_ ]account\b/, 'service_account'],
  ];
  for (const [re, field] of rules) {
    const m = re.exec(msg);
    if (m) return { [field]: say(m[1] ?? msg) };
  }
  return {};
}

/** One condition as text: "repository = acme/app", "ref ~ refs/tags/v* or refs/heads/main". */
export function conditionText(c: { claim: string; match?: string; values: string[] }): string {
  return `${c.claim} ${c.match === 'glob' ? '~' : '='} ${c.values.join(' or ')}`;
}

export function ttlText(seconds: number): string {
  if (seconds % 60 !== 0) return `${seconds} s`;
  const m = seconds / 60;
  return m === 60 ? '1 hour' : `${m} min`;
}

/* ---------------------------------------------------------------- usage */

export interface UsageSnippet {
  title: string;
  /** What the code is, for the copy button. */
  label: string;
  lines: CodeLine[];
}

const shellQuote = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;

/**
 * Paste-ready ways to use a policy: the platform's CI configuration (GitHub
 * Actions, GitLab CI) and the plain exchange with curl. The audience a job
 * asks for must equal the policy's; naming the policy keeps the exchange
 * unambiguous when several match.
 */
export function usageSnippets(p: Pick<TrustPolicy, 'name' | 'issuer' | 'audience'>, baseUrl: string): UsageSnippet[] {
  const exchange = `${baseUrl}/api/v1/auth/oidc/exchange`;
  const body = (token: string) => `{"token": "${token}", "policy": "${p.name}"}`;
  const out: UsageSnippet[] = [];
  const preset = presetFor(p.issuer);
  if (preset.id === 'github') {
    out.push({
      title: 'GitHub Actions workflow',
      label: 'GitHub Actions workflow',
      lines: [
        { text: 'permissions:' },
        { text: '  id-token: write   # lets the job request an ID token' },
        { text: '  contents: read' },
        { text: '' },
        { text: 'steps:' },
        { text: '  - name: Exchange the ID token for a Hangar token' },
        { text: '    run: |' },
        { text: '      ID_TOKEN=$(curl -sSf -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \\' },
        { text: `        "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=${p.audience}" | jq -r .value)` },
        { text: `      HANGAR_TOKEN=$(curl -sSf -X POST ${exchange} \\` },
        { text: `        -H 'Content-Type: application/json' -d "${body('$ID_TOKEN').replace(/"/g, '\\"')}" | jq -r .token)` },
        { text: '      echo "::add-mask::$HANGAR_TOKEN"' },
        { text: '      echo "HANGAR_TOKEN=$HANGAR_TOKEN" >> "$GITHUB_ENV"' },
      ],
    });
  } else if (preset.id === 'gitlab') {
    out.push({
      title: 'GitLab CI job',
      label: 'GitLab CI job',
      lines: [
        { text: 'publish:' },
        { text: '  id_tokens:' },
        { text: '    HANGAR_ID_TOKEN:' },
        { text: `      aud: ${p.audience}` },
        { text: '  script:' },
        { text: '    - |' },
        { text: `      HANGAR_TOKEN=$(curl -sSf -X POST ${exchange} \\` },
        { text: `        -H 'Content-Type: application/json' -d "${body('$HANGAR_ID_TOKEN').replace(/"/g, '\\"')}" | jq -r .token)` },
      ],
    });
  }
  out.push({
    title: 'Exchange with curl',
    label: 'token exchange with curl',
    lines: [
      { text: `# ID_TOKEN: the job's ID token, issued by ${p.issuer} for the audience ${p.audience}`, comment: true },
      { text: `curl -sSf -X POST ${exchange} \\` },
      { text: `  -H 'Content-Type: application/json' -d "${body('$ID_TOKEN').replace(/"/g, '\\"')}"` },
      { text: '# → {"token": "hgr_…", "token_type": "Bearer", "expires_in": 900, "scopes": [...], …}', comment: true },
      { text: '# Use the token as a Bearer token, or as the password of any package client:', comment: true },
      { text: `curl -H "Authorization: Bearer $HANGAR_TOKEN" ${shellQuote(`${baseUrl}/api/v1/auth/session`)}` },
    ],
  });
  return out;
}
