import { describe, expect, it } from 'vitest';
import { directoryRoot, inferLdapPreset, inferOidcPreset, ldapPreset, withBindDn } from '../pages/settings/presets';
import type { LdapForm } from '../pages/settings/model';

describe('provider presets', () => {
  it('recognises providers saved without a preset by their issuer', () => {
    expect(inferOidcPreset({ issuer_url: 'https://sso.example.com/realms/acme' })).toBe('keycloak');
    expect(inferOidcPreset({ issuer_url: 'https://login.microsoftonline.com/0000/v2.0' })).toBe('entra-id');
    expect(inferOidcPreset({ issuer_url: 'https://accounts.google.com' })).toBe('google');
    expect(inferOidcPreset({ issuer_url: 'https://acme.okta.com' })).toBe('okta');
    expect(inferOidcPreset({ issuer_url: 'https://gitlab.example.com' })).toBe('other');
    // A stored preset wins, an LDAP one does not count for OIDC.
    expect(inferOidcPreset({ issuer_url: 'https://sso.example.com/realms/acme', preset: 'other' })).toBe('other');
    expect(inferOidcPreset({ issuer_url: 'https://acme.okta.com', preset: 'openldap' })).toBe('okta');
  });

  it('recognises directories by their filter and base', () => {
    const base = { preset: '', user_filter: '(uid={username})', username_attribute: 'uid', user_base_dn: 'dc=example,dc=com' };
    expect(inferLdapPreset({ ...base, user_filter: '(&(objectClass=user)(sAMAccountName={username}))' })).toBe('active-directory');
    expect(inferLdapPreset({ ...base, user_base_dn: 'cn=users,cn=accounts,dc=example,dc=com' })).toBe('freeipa');
    expect(inferLdapPreset({ ...base, user_filter: '(&(objectClass=inetOrgPerson)(uid={username}))' })).toBe('openldap');
    expect(inferLdapPreset(base)).toBe('other');
  });

  it('finds the directory root of a service account', () => {
    expect(directoryRoot('hangar-svc@corp.example.com', true)).toBe('DC=corp,DC=example,DC=com');
    expect(directoryRoot('cn=hangar,ou=services,dc=example,dc=com')).toBe('dc=example,dc=com');
    expect(directoryRoot('CN=svc,OU=Service,DC=acme,DC=example', true)).toBe('DC=acme,DC=example');
    expect(directoryRoot('svc-hangar')).toBe('');
  });

  it('moves derived search bases with the service account, never typed ones', () => {
    const ad = ldapPreset('active-directory');
    const f = { bind_dn: '', user_base_dn: '', group_base_dn: '' } as LdapForm;
    const a = withBindDn(f, ad, 'svc@example.com');
    expect(a).toMatchObject({ user_base_dn: 'DC=example,DC=com', group_base_dn: 'DC=example,DC=com' });
    const b = withBindDn(a, ad, 'svc@corp.example');
    expect(b).toMatchObject({ user_base_dn: 'DC=corp,DC=example' });
    const typed = withBindDn({ ...b, user_base_dn: 'OU=People,DC=corp,DC=example' }, ad, 'svc@other.example');
    expect(typed.user_base_dn).toBe('OU=People,DC=corp,DC=example');
    expect(typed.group_base_dn).toBe('DC=other,DC=example');
  });
});
