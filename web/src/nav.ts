import type { ComponentType, SVGProps } from 'react';
import {
  IconCluster,
  IconClock,
  IconKey,
  IconDatabase,
  IconOverview,
  IconPackage,
  IconSearch,
  IconServer,
  IconShield,
  IconTerminal,
  IconTrash,
  IconUser,
  IconUsers,
} from './components/icons';
import { IconAudit, IconSettings } from './components/opsIcons';
import { allows } from './lib/can';
import { GATES, passes, type Gate } from './lib/gates';
import { IconLogs, IconPulse } from './pages/system/icons';
import { IconLock, IconRoute } from './pages/settings/icons';

export type AppPath =
  | '/'
  | '/browse'
  | '/search'
  | '/repositories'
  | '/repositories/new'
  | '/blob-stores'
  | '/cleanup'
  | '/vulnerabilities'
  | '/access'
  | '/audit'
  | '/account'
  | '/tasks'
  | '/cluster'
  | '/system/status'
  | '/system/logs'
  | '/settings'
  | '/settings/system'
  | '/settings/authentication'
  | '/settings/proxies'
  | '/api-docs';

/** `system:<area>:<action>` the signed-in user must hold to see an entry (the server still authorizes every call). */
export interface Requirement {
  area: string;
  action: 'read' | 'admin';
}

export interface NavItem {
  to: AppPath;
  /** Element id on the target page to scroll to. */
  hash?: string;
  label: string;
  icon: ComponentType<SVGProps<SVGSVGElement> & { size?: number }>;
  keywords?: string;
  requires?: Requirement | Gate;
}

export interface NavSection {
  title: string;
  items: NavItem[];
  requires?: Requirement;
}

/** Whether the holder of `privileges` may see an entry with this requirement. */
export function canSee(privileges: readonly string[] | undefined, requires: Requirement | Gate | undefined): boolean {
  if (!requires) return true;
  if ('allowed' in requires) return passes(requires, privileges);
  return allows(privileges, 'system', requires.area, requires.action);
}

/** The sections and entries `privileges` may open; sections left empty are dropped. */
export function visibleNav(privileges: readonly string[] | undefined): NavSection[] {
  return NAV.filter((s) => canSee(privileges, s.requires))
    .map((s) => ({ ...s, items: s.items.filter((i) => canSee(privileges, i.requires)) }))
    .filter((s) => s.items.length > 0);
}

/** The sidebar. Personal pages (My account, tokens) live in Settings. */
export const NAV: NavSection[] = [
  {
    title: 'Registry',
    items: [
      { to: '/', label: 'Overview', icon: IconOverview, keywords: 'home dashboard' },
      { to: '/browse', label: 'Packages', icon: IconPackage, keywords: 'browse components' },
      { to: '/search', label: 'Search', icon: IconSearch, keywords: 'find query' },
      { to: '/repositories', label: 'Repositories', icon: IconDatabase, keywords: 'repos hosted proxy group' },
    ],
  },
  {
    title: 'Storage',
    items: [
      { to: '/blob-stores', label: 'Blob stores', icon: IconServer, keywords: 's3 minio disk bucket', requires: GATES.blobStores },
      { to: '/cleanup', label: 'Cleanup policies', icon: IconTrash, keywords: 'retention delete', requires: GATES.cleanup },
    ],
  },
  {
    title: 'Security',
    items: [
      { to: '/vulnerabilities', label: 'Vulnerabilities', icon: IconShield, keywords: 'cve scanning findings quarantine' },
      {
        to: '/access',
        label: 'Access & audit',
        icon: IconUsers,
        keywords: 'users members service accounts roles permissions tokens',
        requires: GATES.access,
      },
      {
        to: '/audit',
        label: 'Audit log',
        icon: IconAudit,
        keywords: 'events history who did export compliance',
        requires: { area: 'audit', action: 'read' },
      },
    ],
  },
  {
    title: 'Operations',
    items: [
      { to: '/tasks', label: 'Scheduled tasks', icon: IconClock, keywords: 'jobs cron', requires: GATES.tasks },
      { to: '/cluster', label: 'Cluster & replication', icon: IconCluster, keywords: 'nodes edge ha', requires: GATES.cluster },
      {
        to: '/system/status',
        label: 'System status',
        icon: IconPulse,
        keywords: 'health nodes heartbeat database nats subsystems uptime checks',
        requires: { area: 'cluster', action: 'read' },
      },
      {
        to: '/system/logs',
        label: 'System logs',
        icon: IconLogs,
        keywords: 'log records errors warnings debug server output',
        requires: { area: 'cluster', action: 'admin' },
      },
    ],
  },
  {
    title: 'Settings',
    items: [
      { to: '/settings', label: 'Settings', icon: IconSettings, keywords: 'preferences configuration administration my account profile' },
      {
        to: '/settings/authentication',
        label: 'Authentication',
        icon: IconLock,
        keywords: 'sign-in sso oidc ldap active directory keycloak login groups roles',
        requires: { area: 'settings', action: 'read' },
      },
      {
        to: '/settings/proxies',
        label: 'Proxies',
        icon: IconRoute,
        keywords: 'internet access http proxy outbound corporate no_proxy ca certificate',
        requires: { area: 'settings', action: 'read' },
      },
    ],
  },
];

/**
 * Pages reached from the Settings hub but not the sidebar. The command
 * palette lists them next to the sidebar pages so every page stays one search away.
 */
export const SETTINGS_PAGES: NavSection = {
  title: 'Settings',
  items: [
    { to: '/account', label: 'My account', icon: IconUser, keywords: 'profile password sessions devices personal' },
    { to: '/account', hash: 'my-tokens-title', label: 'Access tokens', icon: IconKey, keywords: 'personal access tokens api ci cli' },
    { to: '/settings/system', label: 'System', icon: IconTerminal, keywords: 'version build addresses base url endpoints configuration hangar.yaml' },
    { to: '/api-docs', label: 'API reference', icon: IconTerminal, keywords: 'rest api openapi endpoints curl automation integration scripts token' },
  ],
};

/** Where an entry in the Settings hub leads, and what its status line shows. */
export type HubEntryId =
  | 'addresses'
  | 'instance'
  | 'sign-in'
  | 'internet'
  | 'storage'
  | 'nodes'
  | 'configuration'
  | 'status'
  | 'logs'
  | 'audit'
  | 'account'
  | 'tokens'
  | 'api-docs';

export interface HubEntry {
  id: HubEntryId;
  label: string;
  /** `/api-docs` is served by a route that older builds do not have; the hub hides it there. */
  to: AppPath | '/api-docs';
  hash?: string;
  requires?: Requirement;
}

export interface HubGroup {
  /** Read by screen readers; the groups carry no visible heading. */
  label: string;
  entries: HubEntry[];
}

const SETTINGS_READ: Requirement = { area: 'settings', action: 'read' };

/** The Settings page: one row per setting, grouped into cards. */
export const SETTINGS_HUB: HubGroup[] = [
  {
    label: 'This installation',
    entries: [
      { id: 'addresses', label: 'Addresses', to: '/settings/system', hash: 'endpoints-title', requires: SETTINGS_READ },
      { id: 'instance', label: 'Instance name', to: '/settings/system', requires: SETTINGS_READ },
    ],
  },
  {
    label: 'Sign-in, network and storage',
    entries: [
      { id: 'sign-in', label: 'Sign-in', to: '/settings/authentication', requires: SETTINGS_READ },
      { id: 'internet', label: 'Internet access', to: '/settings/proxies', requires: SETTINGS_READ },
      { id: 'storage', label: 'Storage', to: '/blob-stores', requires: { area: 'blobstores', action: 'read' } },
    ],
  },
  {
    label: 'Operations',
    entries: [
      { id: 'nodes', label: 'Nodes', to: '/cluster', requires: { area: 'cluster', action: 'read' } },
      { id: 'configuration', label: 'Configuration', to: '/settings/system', hash: 'config-title', requires: SETTINGS_READ },
      { id: 'status', label: 'System status', to: '/system/status', requires: { area: 'cluster', action: 'read' } },
      { id: 'logs', label: 'System logs', to: '/system/logs', requires: { area: 'cluster', action: 'admin' } },
      { id: 'audit', label: 'Audit log', to: '/audit', requires: { area: 'audit', action: 'read' } },
    ],
  },
  {
    label: 'Your account',
    entries: [
      { id: 'account', label: 'My account', to: '/account' },
      { id: 'tokens', label: 'Access tokens', to: '/account', hash: 'my-tokens-title' },
      { id: 'api-docs', label: 'API reference', to: '/api-docs' },
    ],
  },
];

/** The hub groups and entries `privileges` may see; groups left empty are dropped. */
export function visibleHub(privileges: readonly string[] | undefined): HubGroup[] {
  return SETTINGS_HUB.map((g) => ({ ...g, entries: g.entries.filter((e) => canSee(privileges, e.requires)) })).filter((g) => g.entries.length > 0);
}

/**
 * Pages without a sidebar entry light up the entry of the area they belong to:
 * My account, the API reference and System sit inside Settings.
 */
const AREA_OF: [prefix: string, item: AppPath][] = [
  ['/account', '/settings'],
  ['/api-docs', '/settings'],
  ['/settings/system', '/settings'],
]

const under = (pathname: string, prefix: string) => pathname === prefix || pathname.startsWith(`${prefix}/`);

/**
 * The sidebar entry for `pathname`, and whether it is that page (or a page
 * inside it) or only the area the page belongs to.
 */
export function activeNav(pathname: string): { to: AppPath; own: boolean } | undefined {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, '') : pathname;
  let best: AppPath | undefined;
  for (const item of NAV.flatMap((s) => s.items)) {
    const hit = item.to === '/' ? path === '/' : under(path, item.to);
    if (hit && (!best || item.to.length > best.length)) best = item.to;
  }
  if (best) return { to: best, own: true };
  let area: [string, AppPath] | undefined;
  for (const entry of AREA_OF) {
    if (under(path, entry[0]) && (!area || entry[0].length > area[0].length)) area = entry;
  }
  return area && { to: area[1], own: false };
}
