/**
 * Provider presets of Settings → Authentication. A preset fills the fields
 * a provider always wants (scopes, claims, filters, attributes) so the
 * administrator only types the few values that are theirs: the address,
 * the client or service account and its secret.
 *
 * Presets are client-side data. The chosen preset's id is stored with the
 * settings (`preset`) only so the page reopens on it; the server never acts
 * on it.
 */
import type { LDAPSettings, OIDCSettings } from './settingsApi';
import type { LdapForm, OidcForm } from './model';

export type Protocol = 'oidc' | 'ldap';

/** Ids of the preset cards, in the order the page shows them. */
export const PRESET_IDS = ['active-directory', 'openldap', 'freeipa', 'keycloak', 'entra-id', 'google', 'okta', 'other'] as const;
export type PresetId = (typeof PRESET_IDS)[number];

type OidcOwned = Pick<OidcForm, 'scopes' | 'client_auth' | 'username_claim' | 'email_claim' | 'name_claim' | 'groups_claim'>;
type LdapOwned = Pick<
  LdapForm,
  'user_filter' | 'username_attribute' | 'email_attribute' | 'name_attribute' | 'group_attribute' | 'group_filter' | 'group_name_attribute' | 'nested_groups'
>;

export interface OidcPreset {
  id: PresetId;
  protocol: 'oidc';
  name: string;
  /** Placeholder of the issuer field; `<…>` parts are the administrator's. */
  issuerPlaceholder: string;
  /** A fixed issuer every tenant shares (Google). */
  issuer?: string;
  /** Sign-in button label the preset suggests. */
  displayName: string;
  values: OidcOwned;
  /** Where to register the client, in order. */
  steps: string[];
  groupPlaceholder: string;
}

export interface LdapPreset {
  id: PresetId;
  protocol: 'ldap';
  name: string;
  urlPlaceholder: string;
  bindPlaceholder: string;
  values: LdapOwned;
  /** User base DN for a directory root ("dc=example,dc=com"). */
  userBase: (root: string) => string;
  /** Group base DN for a directory root, when groups are searched (nested groups). */
  groupBase?: (root: string) => string;
  /** Upper-case DC= as Active Directory writes it. */
  upperDc?: boolean;
  steps: string[];
  groupPlaceholder: string;
}

export type Preset = OidcPreset | LdapPreset;

const OIDC_STD: OidcOwned = {
  scopes: ['openid', 'profile', 'email'],
  client_auth: 'basic',
  username_claim: 'preferred_username',
  email_claim: 'email',
  name_claim: 'name',
  groups_claim: 'groups',
};

/** Active Directory's in-chain matching rule: groups the user is in, directly or through other groups. */
export const AD_NESTED_GROUP_FILTER = '(member:1.2.840.113556.1.4.1941:={dn})';

const OIDC_PRESETS: Record<string, OidcPreset> = {
  keycloak: {
    id: 'keycloak',
    protocol: 'oidc',
    name: 'Keycloak',
    issuerPlaceholder: 'https://<host>/realms/<realm>',
    displayName: 'SSO',
    values: OIDC_STD,
    steps: [
      'In your realm, open Clients → Create client: OpenID Connect, client ID hangar, Client authentication on, Standard flow only.',
      'Add the redirect URI below to Valid redirect URIs and the same address ending in /login to Valid post logout redirect URIs, then copy the secret from the Credentials tab.',
      'Under Client scopes → hangar-dedicated, add a Group Membership mapper with token claim name groups (Full group path off).',
    ],
    groupPlaceholder: 'hangar-admins',
  },
  'entra-id': {
    id: 'entra-id',
    protocol: 'oidc',
    name: 'Microsoft Entra ID',
    issuerPlaceholder: 'https://login.microsoftonline.com/<tenant-id>/v2.0',
    displayName: 'Microsoft',
    values: OIDC_STD,
    steps: [
      'In the Entra admin center, App registrations → New registration; add the redirect URI below as a Web redirect URI.',
      'Certificates & secrets → New client secret. The issuer uses the Directory (tenant) ID from the Overview page.',
      'Token configuration → Add groups claim → Security groups. Entra sends group object IDs: map those IDs to roles.',
    ],
    groupPlaceholder: '00000000-0000-0000-0000-000000000000',
  },
  google: {
    id: 'google',
    protocol: 'oidc',
    name: 'Google Workspace',
    issuerPlaceholder: 'https://accounts.google.com',
    issuer: 'https://accounts.google.com',
    displayName: 'Google',
    values: { ...OIDC_STD, username_claim: 'email', groups_claim: 'groups' },
    steps: [
      'In the Google Cloud console, set the OAuth consent screen user type to Internal, so only accounts of your Workspace can sign in.',
      'Credentials → Create credentials → OAuth client ID → Web application; add the redirect URI below.',
      'Google sends no groups: everyone gets the default roles. Grant more in Access & audit after the first sign-in.',
    ],
    groupPlaceholder: 'hangar-admins',
  },
  okta: {
    id: 'okta',
    protocol: 'oidc',
    name: 'Okta',
    issuerPlaceholder: 'https://<org>.okta.com',
    displayName: 'Okta',
    values: { ...OIDC_STD, scopes: ['openid', 'profile', 'email', 'groups'] },
    steps: [
      'Applications → Create App Integration → OIDC, Web Application; set the sign-in redirect URI to the URI below.',
      'Sign On → OpenID Connect ID Token: set the groups claim filter to groups, Matches regex .*',
      'Assign the application to the groups that may use Hangar.',
    ],
    groupPlaceholder: 'hangar-admins',
  },
  other: {
    id: 'other',
    protocol: 'oidc',
    name: 'Other',
    issuerPlaceholder: 'https://sso.example.com',
    displayName: 'SSO',
    values: OIDC_STD,
    steps: [
      'Register a confidential web client with the authorization code flow and the redirect URI below (GitLab, Authentik, Dex and others work the same way).',
      'The issuer URL is the address under which the provider publishes /.well-known/openid-configuration.',
      'Make the user’s groups reach a claim in the ID token or userinfo, and name that claim under Customize.',
    ],
    groupPlaceholder: 'hangar-admins',
  },
};

const LDAP_PRESETS: Record<string, LdapPreset> = {
  'active-directory': {
    id: 'active-directory',
    protocol: 'ldap',
    name: 'Active Directory',
    urlPlaceholder: 'ldaps://dc1.example.com',
    bindPlaceholder: 'hangar-svc@example.com',
    values: {
      user_filter: '(&(objectClass=user)(sAMAccountName={username}))',
      username_attribute: 'sAMAccountName',
      email_attribute: 'mail',
      name_attribute: 'displayName',
      group_attribute: 'memberOf',
      group_filter: AD_NESTED_GROUP_FILTER,
      group_name_attribute: 'cn',
      nested_groups: true,
    },
    userBase: (root) => root,
    groupBase: (root) => root,
    upperDc: true,
    steps: [
      'Create a service account that may read users and groups; enter it as user@domain or as its DN.',
      'Use ldaps:// (port 636). If your domain controllers use a private CA, paste it under Customize.',
      'Users and groups are searched from the domain root, with nested groups; narrow the search bases under Customize.',
    ],
    groupPlaceholder: 'Hangar Admins',
  },
  openldap: {
    id: 'openldap',
    protocol: 'ldap',
    name: 'OpenLDAP',
    urlPlaceholder: 'ldaps://ldap.example.com',
    bindPlaceholder: 'cn=hangar,ou=services,dc=example,dc=com',
    values: {
      user_filter: '(&(objectClass=inetOrgPerson)(uid={username}))',
      username_attribute: 'uid',
      email_attribute: 'mail',
      name_attribute: 'cn',
      group_attribute: 'memberOf',
      group_filter: '(member={dn})',
      group_name_attribute: 'cn',
      nested_groups: false,
    },
    userBase: (root) => root,
    steps: [
      'Create a read-only bind entry for Hangar, e.g. cn=hangar,ou=services,dc=example,dc=com.',
      'Groups are read from memberOf, which needs the memberof overlay. Without it, set a group base DN under Customize.',
      'Use ldaps://, or ldap:// with StartTLS under Customize.',
    ],
    groupPlaceholder: 'hangar-admins',
  },
  freeipa: {
    id: 'freeipa',
    protocol: 'ldap',
    name: 'FreeIPA',
    urlPlaceholder: 'ldaps://ipa.example.com',
    bindPlaceholder: 'uid=hangar,cn=sysaccounts,cn=etc,dc=example,dc=com',
    values: {
      user_filter: '(&(objectClass=person)(uid={username}))',
      username_attribute: 'uid',
      email_attribute: 'mail',
      name_attribute: 'displayName',
      group_attribute: 'memberOf',
      group_filter: '(member={dn})',
      group_name_attribute: 'cn',
      nested_groups: false,
    },
    userBase: (root) => (root ? `cn=users,cn=accounts,${root}` : ''),
    steps: [
      'Create a system account under cn=sysaccounts,cn=etc for Hangar to search with.',
      'Users are searched in cn=users,cn=accounts; groups come from memberOf, which FreeIPA keeps up to date.',
      'Trust the IPA certificate authority (/etc/ipa/ca.crt) under Customize.',
    ],
    groupPlaceholder: 'admins',
  },
  other: {
    id: 'other',
    protocol: 'ldap',
    name: 'Other',
    urlPlaceholder: 'ldaps://ldap.example.com',
    bindPlaceholder: 'cn=hangar,dc=example,dc=com',
    values: {
      user_filter: '(&(objectClass=person)(uid={username}))',
      username_attribute: 'uid',
      email_attribute: 'mail',
      name_attribute: 'cn',
      group_attribute: 'memberOf',
      group_filter: '(member={dn})',
      group_name_attribute: 'cn',
      nested_groups: false,
    },
    userBase: (root) => root,
    steps: [
      'Enter the directory and a service account that may search users.',
      'Set the user filter, the attributes and how groups are found under Customize.',
    ],
    groupPlaceholder: 'hangar-admins',
  },
};

/** The cards of the picker. "Other" covers both protocols. */
export const PRESET_CARDS: { id: PresetId; name: string; kind: string }[] = [
  { id: 'active-directory', name: 'Active Directory', kind: 'LDAP' },
  { id: 'openldap', name: 'OpenLDAP', kind: 'LDAP' },
  { id: 'freeipa', name: 'FreeIPA', kind: 'LDAP' },
  { id: 'keycloak', name: 'Keycloak', kind: 'OIDC' },
  { id: 'entra-id', name: 'Microsoft Entra ID', kind: 'OIDC' },
  { id: 'google', name: 'Google Workspace', kind: 'OIDC' },
  { id: 'okta', name: 'Okta', kind: 'OIDC' },
  { id: 'other', name: 'Other', kind: 'Any OIDC or LDAP' },
];

/** The protocol of a card; "other" asks. */
export function protocolOf(id: PresetId): Protocol | null {
  if (id === 'other') return null;
  return id in LDAP_PRESETS ? 'ldap' : 'oidc';
}

export function oidcPreset(id: PresetId): OidcPreset {
  return OIDC_PRESETS[id] ?? OIDC_PRESETS.other!;
}

export function ldapPreset(id: PresetId): LdapPreset {
  return LDAP_PRESETS[id] ?? LDAP_PRESETS.other!;
}

const isPresetId = (v: string | undefined): v is PresetId => !!v && (PRESET_IDS as readonly string[]).includes(v);

/**
 * The preset an OIDC provider was set up with: the stored one, else a guess
 * from the issuer (for providers set in the configuration file or saved
 * before presets existed).
 */
export function inferOidcPreset(s: Pick<OIDCSettings, 'issuer_url' | 'preset'>): PresetId {
  if (isPresetId(s.preset) && (s.preset === 'other' || protocolOf(s.preset) === 'oidc')) return s.preset;
  const issuer = s.issuer_url.toLowerCase();
  if (/\/realms\/[^/]+\/?$/.test(issuer)) return 'keycloak';
  if (issuer.includes('login.microsoftonline.com')) return 'entra-id';
  if (issuer.startsWith('https://accounts.google.com')) return 'google';
  if (/\.(okta|oktapreview|okta-emea)\.com(\/|$)/.test(issuer)) return 'okta';
  return 'other';
}

/** Like inferOidcPreset, from the filter and attributes. */
export function inferLdapPreset(s: Pick<LDAPSettings, 'preset' | 'user_filter' | 'username_attribute' | 'user_base_dn'>): PresetId {
  if (isPresetId(s.preset) && (s.preset === 'other' || protocolOf(s.preset) === 'ldap')) return s.preset;
  if (/samaccountname/i.test(s.user_filter) || s.username_attribute.toLowerCase() === 'samaccountname') return 'active-directory';
  if (/cn=accounts,/i.test(s.user_base_dn)) return 'freeipa';
  if (/inetorgperson/i.test(s.user_filter)) return 'openldap';
  return 'other';
}

/**
 * The directory root a service account lives in: the domain of a
 * user@domain name, or the dc= parts of a DN. Empty when unknown.
 */
export function directoryRoot(bindDn: string, upper = false): string {
  const dc = upper ? 'DC' : 'dc';
  const at = bindDn.trim().match(/^[^@\s,=]+@([a-z0-9.-]+\.[a-z0-9-]+)$/i);
  if (at) return at[1]!.split('.').map((p) => `${dc}=${p}`).join(',');
  const parts = [...bindDn.matchAll(/(?:^|,)\s*dc\s*=\s*([^,]+)/gi)].map((m) => m[1]!.trim());
  return parts.map((p) => `${dc}=${p}`).join(',');
}

/** Search bases a preset derives from a service account. */
export function ldapBases(p: LdapPreset, bindDn: string): { user_base_dn: string; group_base_dn: string } {
  const root = directoryRoot(bindDn, p.upperDc);
  return { user_base_dn: p.userBase(root), group_base_dn: p.groupBase ? p.groupBase(root) : '' };
}

/** The search bases every preset would derive from bindDn, to tell a derived base from a typed one. */
function derivedBases(bindDn: string): Set<string> {
  const out = new Set<string>(['']);
  for (const p of Object.values(LDAP_PRESETS)) {
    const b = ldapBases(p, bindDn);
    out.add(b.user_base_dn);
    out.add(b.group_base_dn);
  }
  return out;
}

const OIDC_OWNED = ['scopes', 'client_auth', 'username_claim', 'email_claim', 'name_claim', 'groups_claim'] as const;
const LDAP_OWNED = ['user_filter', 'username_attribute', 'email_attribute', 'name_attribute', 'group_attribute', 'group_filter', 'group_name_attribute', 'nested_groups'] as const;
const PRESET_LABELS = new Set(Object.values(OIDC_PRESETS).map((p) => p.displayName));

/**
 * Switches the OIDC form to a preset. Credentials the administrator typed
 * (client ID, secret, mapping) stay. The preset's own fields come from the
 * stored settings when they were saved with this preset, else from the
 * preset. An issuer that belongs to another provider is cleared so the
 * preset's template shows.
 */
export function applyOidcPreset(f: OidcForm, p: OidcPreset, stored: OIDCSettings | undefined): OidcForm {
  const same = !!stored && stored.source !== 'default' && inferOidcPreset(stored) === p.id;
  const next: OidcForm = { ...f };
  for (const k of OIDC_OWNED) (next as unknown as Record<string, unknown>)[k] = same ? copy(stored![k]) : copy(p.values[k]);
  if (same) {
    next.display_name = stored!.display_name;
    if (inferOidcPreset({ issuer_url: f.issuer_url }) !== p.id) next.issuer_url = stored!.issuer_url;
  } else {
    if (!f.display_name.trim() || PRESET_LABELS.has(f.display_name)) next.display_name = p.displayName;
    if (p.issuer) next.issuer_url = p.issuer;
    else if (f.issuer_url && p.id !== 'other' && inferOidcPreset({ issuer_url: f.issuer_url }) !== p.id) next.issuer_url = '';
  }
  return next;
}

/** Switches the LDAP form to a preset, like applyOidcPreset; derived search bases follow. */
export function applyLdapPreset(f: LdapForm, p: LdapPreset, stored: LDAPSettings | undefined): LdapForm {
  const same = !!stored && stored.source !== 'default' && inferLdapPreset(stored) === p.id;
  const next: LdapForm = { ...f };
  for (const k of LDAP_OWNED) (next as unknown as Record<string, unknown>)[k] = same ? copy(stored![k]) : copy(p.values[k]);
  if (same) {
    next.user_base_dn = stored!.user_base_dn;
    next.group_base_dn = stored!.group_base_dn;
    return next;
  }
  const derived = derivedBases(f.bind_dn);
  const bases = ldapBases(p, f.bind_dn);
  if (derived.has(f.user_base_dn)) next.user_base_dn = bases.user_base_dn;
  if (derived.has(f.group_base_dn)) next.group_base_dn = bases.group_base_dn;
  return next;
}

/** A new service account moves the search bases along, unless the administrator typed their own. */
export function withBindDn(f: LdapForm, p: LdapPreset, bindDn: string): LdapForm {
  const before = derivedBases(f.bind_dn);
  const bases = ldapBases(p, bindDn);
  return {
    ...f,
    bind_dn: bindDn,
    user_base_dn: before.has(f.user_base_dn) ? bases.user_base_dn : f.user_base_dn,
    group_base_dn: before.has(f.group_base_dn) ? bases.group_base_dn : f.group_base_dn,
  };
}

function copy<T>(v: T): T {
  return Array.isArray(v) ? ([...v] as T) : v;
}

/** The short "We'll use" summary of an OIDC form. */
export function oidcSummary(f: OidcForm): string[] {
  return [f.scopes.join(' '), f.groups_claim ? `${f.groups_claim} claim` : 'no groups claim', f.username_claim];
}

/** The short "We'll use" summary of an LDAP form. */
export function ldapSummary(f: LdapForm): string[] {
  const out = [f.username_attribute];
  if (f.group_base_dn.trim()) out.push(f.nested_groups ? 'nested groups on' : 'group search');
  else out.push(f.group_attribute);
  if (f.user_base_dn.trim()) out.push(f.user_base_dn);
  return out;
}
