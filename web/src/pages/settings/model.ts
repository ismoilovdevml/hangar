/**
 * Form state of the Settings pages: what the user edits, how it becomes a
 * request body, and the client-side checks that mirror the server's (the
 * server stays the authority and answers with field errors too).
 */
import type { GroupRole, LDAPInput, LDAPSettings, OIDCInput, OIDCSettings, OutboundProxy, OutboundProxyInput } from './settingsApi';

export type Errors = Record<string, string>;

/* ---------------------------------------------------------------- OIDC */

export interface OidcForm {
  enabled: boolean;
  issuer_url: string;
  client_id: string;
  client_secret: string;
  clear_client_secret: boolean;
  client_auth: 'basic' | 'post';
  display_name: string;
  scopes: string[];
  username_claim: string;
  email_claim: string;
  name_claim: string;
  groups_claim: string;
  ca_pem: string;
  group_roles: GroupRole[];
  default_roles: string[];
  allowed_groups: string[];
  auto_create: boolean;
}

export function oidcForm(s: OIDCSettings): OidcForm {
  return {
    enabled: s.enabled,
    issuer_url: s.issuer_url,
    client_id: s.client_id,
    client_secret: '',
    clear_client_secret: false,
    client_auth: s.client_auth,
    display_name: s.display_name,
    scopes: [...s.scopes],
    username_claim: s.username_claim,
    email_claim: s.email_claim,
    name_claim: s.name_claim,
    groups_claim: s.groups_claim,
    ca_pem: s.ca_pem,
    group_roles: s.group_roles.map((r) => ({ group: r.group, roles: [...r.roles] })),
    default_roles: [...s.default_roles],
    allowed_groups: [...s.allowed_groups],
    auto_create: s.auto_create,
  };
}

export function oidcInput(f: OidcForm): OIDCInput {
  return {
    ...f,
    issuer_url: f.issuer_url.trim(),
    client_id: f.client_id.trim(),
    display_name: f.display_name.trim(),
    group_roles: f.group_roles.map((r) => ({ group: r.group.trim(), roles: r.roles })),
  };
}

const isLoopback = (h: string) => h === 'localhost' || h === '127.0.0.1' || h === '[::1]';

function parseUrl(s: string): URL | null {
  try {
    return new URL(s);
  } catch {
    return null;
  }
}

function checkMapping(e: Errors, rows: GroupRole[]) {
  const seen = new Map<string, number>();
  rows.forEach((r, i) => {
    const key = r.group.trim().replace(/^\//, '').toLowerCase();
    if (!r.group.trim()) e[`group_roles[${i}].group`] = 'Enter a group name.';
    else if (seen.has(key)) e[`group_roles[${i}].group`] = `Already mapped in row ${seen.get(key)}.`;
    else seen.set(key, i + 1);
    if (r.roles.length === 0) e[`group_roles[${i}].roles`] = 'Pick at least one role.';
  });
}

export function validateOidc(f: OidcForm): Errors {
  const e: Errors = {};
  const issuer = f.issuer_url.trim();
  if (f.enabled && !issuer) e.issuer_url = 'Enter the issuer URL of your identity provider.';
  if (issuer) {
    const u = parseUrl(issuer);
    if (!u || !u.host || (u.protocol !== 'https:' && u.protocol !== 'http:')) e.issuer_url = 'Use an absolute https:// URL.';
    else if (u.protocol === 'http:' && !isLoopback(u.hostname)) e.issuer_url = 'Use https (http is accepted for localhost only).';
  }
  if (f.enabled && !f.client_id.trim()) e.client_id = 'Enter the client ID registered at the provider.';
  if (!f.scopes.includes('openid')) e.scopes = 'Scopes must include openid.';
  if (f.display_name.length > 64) e.display_name = 'Keep the button label under 64 characters.';
  if (f.ca_pem.trim() && !f.ca_pem.includes('-----BEGIN CERTIFICATE-----')) e.ca_pem = 'Paste PEM certificates (-----BEGIN CERTIFICATE----- …).';
  checkMapping(e, f.group_roles);
  return e;
}

/* ---------------------------------------------------------------- LDAP */

export interface LdapForm {
  enabled: boolean;
  url: string;
  start_tls: boolean;
  ca_pem: string;
  bind_dn: string;
  bind_password: string;
  clear_bind_password: boolean;
  user_base_dn: string;
  user_filter: string;
  username_attribute: string;
  email_attribute: string;
  name_attribute: string;
  group_attribute: string;
  group_base_dn: string;
  group_filter: string;
  group_name_attribute: string;
  nested_groups: boolean;
  group_roles: GroupRole[];
  default_roles: string[];
  allowed_groups: string[];
  auto_create: boolean;
}

export function ldapForm(s: LDAPSettings): LdapForm {
  return {
    enabled: s.enabled,
    url: s.url,
    start_tls: s.start_tls,
    ca_pem: s.ca_pem,
    bind_dn: s.bind_dn,
    bind_password: '',
    clear_bind_password: false,
    user_base_dn: s.user_base_dn,
    user_filter: s.user_filter,
    username_attribute: s.username_attribute,
    email_attribute: s.email_attribute,
    name_attribute: s.name_attribute,
    group_attribute: s.group_attribute,
    group_base_dn: s.group_base_dn,
    group_filter: s.group_filter,
    group_name_attribute: s.group_name_attribute,
    nested_groups: s.nested_groups,
    group_roles: s.group_roles.map((r) => ({ group: r.group, roles: [...r.roles] })),
    default_roles: [...s.default_roles],
    allowed_groups: [...s.allowed_groups],
    auto_create: s.auto_create,
  };
}

export function ldapInput(f: LdapForm): LDAPInput {
  return { ...f, url: f.url.trim(), bind_dn: f.bind_dn.trim(), user_base_dn: f.user_base_dn.trim(), group_roles: f.group_roles.map((r) => ({ group: r.group.trim(), roles: r.roles })) };
}

export function validateLdap(f: LdapForm, passwordStored: boolean): Errors {
  const e: Errors = {};
  const url = f.url.trim();
  if (f.enabled && !url) e.url = 'Enter the directory address, e.g. ldaps://dc1.example.com.';
  if (url) {
    const u = /^(ldaps?):\/\/[^/\s]+/.exec(url);
    if (!u) e.url = 'Use ldap://host[:port] or ldaps://host[:port].';
    else if (u[1] === 'ldaps' && f.start_tls) e.start_tls = 'StartTLS is for ldap:// addresses; ldaps:// is already encrypted.';
  }
  const hasPassword = (f.bind_password !== '' || passwordStored) && !f.clear_bind_password;
  if (f.bind_dn.trim() && !hasPassword) e.bind_password = 'Enter the password of the bind DN.';
  if (!f.bind_dn.trim() && f.bind_password) e.bind_dn = 'Enter the bind DN this password belongs to.';
  if (f.enabled && !f.user_base_dn.trim()) e.user_base_dn = 'Enter where users are searched, e.g. DC=example,DC=com.';
  if (!f.user_filter.includes('{username}')) e.user_filter = 'The filter must contain {username}.';
  else if (!f.user_filter.trim().startsWith('(')) e.user_filter = 'Write an LDAP filter in parentheses.';
  if (f.group_filter.trim() && !f.group_filter.trim().startsWith('(')) e.group_filter = 'Write an LDAP filter in parentheses.';
  if (f.nested_groups && !f.group_base_dn.trim()) e.group_base_dn = 'Nested groups need a group base DN.';
  if (f.ca_pem.trim() && !f.ca_pem.includes('-----BEGIN CERTIFICATE-----')) e.ca_pem = 'Paste PEM certificates (-----BEGIN CERTIFICATE----- …).';
  checkMapping(e, f.group_roles);
  return e;
}

/* ---------------------------------------------------------------- proxies */

export interface ProxyForm {
  name: string;
  scheme: 'http' | 'https';
  host: string;
  port: string;
  username: string;
  password: string;
  clear_password: boolean;
  no_proxy: string[];
  redirect_hosts: string[];
  ca_pem: string;
  default: boolean;
}

export const NEW_PROXY: ProxyForm = {
  name: '',
  scheme: 'http',
  host: '',
  port: '3128',
  username: '',
  password: '',
  clear_password: false,
  no_proxy: [],
  redirect_hosts: [],
  ca_pem: '',
  default: false,
};

export function proxyForm(p: OutboundProxy): ProxyForm {
  return {
    name: p.name,
    scheme: p.scheme,
    host: p.host,
    port: String(p.port),
    username: p.username,
    password: '',
    clear_password: false,
    no_proxy: [...p.no_proxy],
    redirect_hosts: [...p.redirect_hosts],
    ca_pem: p.ca_pem,
    default: p.default,
  };
}

export function proxyInput(f: ProxyForm): OutboundProxyInput {
  return {
    name: f.name.trim(),
    scheme: f.scheme,
    host: f.host.trim(),
    port: Number(f.port),
    username: f.username.trim(),
    password: f.password,
    clear_password: f.clear_password,
    no_proxy: f.no_proxy,
    redirect_hosts: f.redirect_hosts,
    ca_pem: f.ca_pem.trim(),
    default: f.default,
  };
}

export const PROXY_NAME = /^[a-z0-9][a-z0-9._-]{0,62}$/;
const HOST = /^(?:[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,61}[A-Za-z0-9])?)(?:\.[A-Za-z0-9_](?:[A-Za-z0-9_-]{0,61}[A-Za-z0-9])?)*\.?$/;
const IPV4 = /^\d{1,3}(?:\.\d{1,3}){3}$/;

export function validateProxy(f: ProxyForm, creating: boolean, passwordStored: boolean): Errors {
  const e: Errors = {};
  if (creating) {
    if (!f.name.trim()) e.name = 'Enter a name.';
    else if (!PROXY_NAME.test(f.name.trim())) e.name = "Use lower-case letters, digits, '.', '_' or '-', starting with a letter or digit.";
    else if (f.name.trim() === 'config') e.name = '"config" is reserved for the proxy of the configuration file.';
  }
  const host = f.host.trim();
  if (!host) e.host = 'Enter the proxy host name or IP address.';
  else if (/[/@\s]/.test(host) || (host.includes(':') && !host.includes('::'))) e.host = 'Enter the host only, without scheme, port or path.';
  else if (!HOST.test(host) && !IPV4.test(host) && !host.includes(':')) e.host = 'This is not a valid host name.';
  const port = Number(f.port);
  if (!/^\d+$/.test(f.port.trim()) || port < 1 || port > 65535) e.port = 'Enter a port between 1 and 65535.';
  const hasPassword = (f.password !== '' || passwordStored) && !f.clear_password;
  if (hasPassword && !f.username.trim()) e.username = 'Enter the username this password belongs to.';
  for (const h of f.redirect_hosts) {
    if (!/^\.?[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*(?::\d+)?$/.test(h)) {
      e.redirect_hosts = `"${h}": enter a host or a domain suffix like .cdn.example.`;
      break;
    }
  }
  for (const h of f.no_proxy) {
    if (/\s/.test(h)) {
      e.no_proxy = `"${h}": enter a host, a domain suffix like .corp.example, an IP or a CIDR.`;
      break;
    }
  }
  if (f.ca_pem.trim() && !f.ca_pem.includes('-----BEGIN CERTIFICATE-----')) e.ca_pem = 'Paste PEM certificates (-----BEGIN CERTIFICATE----- …).';
  return e;
}

/** host:port as people write it (IPv6 in brackets). */
export function hostPort(host: string, port: number | string): string {
  return host.includes(':') ? `[${host}]:${port}` : `${host}:${port}`;
}
