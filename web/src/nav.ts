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
import { IconAudit } from './components/opsIcons';
import { allows } from './lib/privileges';
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
  | '/system'
  | '/system/status'
  | '/system/nodes'
  | '/system/logs'
  | '/system/about'
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
  /** The name outside its tab bar (command palette), when `label` is a tab's short name. */
  title?: string;
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

/** The sidebar. Personal pages (My account, tokens) are in the account menu. */
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
        label: 'Access',
        icon: IconUsers,
        keywords: 'users members service accounts roles permissions tokens',
        requires: GATES.access,
      },
      {
        to: '/audit',
        label: 'Audit log',
        icon: IconAudit,
        keywords: 'events history who did export compliance',
        requires: GATES.audit,
      },
    ],
  },
  {
    title: 'Operations',
    items: [
      { to: '/tasks', label: 'Scheduled tasks', icon: IconClock, keywords: 'jobs cron', requires: GATES.tasks },
      { to: '/system', label: 'System', icon: IconPulse, keywords: 'status health cluster nodes logs version about installation' },
    ],
  },
  {
    title: 'Settings',
    items: [
      {
        to: '/settings/authentication',
        label: 'Authentication',
        icon: IconLock,
        keywords: 'sign-in sso oidc ldap active directory keycloak login groups roles',
        requires: GATES.settings,
      },
      {
        to: '/settings/proxies',
        label: 'Proxies',
        icon: IconRoute,
        keywords: 'internet access http proxy outbound corporate no_proxy ca certificate',
        requires: GATES.settings,
      },
    ],
  },
];

/**
 * The tabs of the System area (/system), in order. /system opens the first
 * one the user may see; About needs no privilege, so everyone has a tab.
 */
export const SYSTEM_TABS: NavItem[] = [
  {
    to: '/system/status',
    label: 'Status',
    title: 'System status',
    icon: IconPulse,
    keywords: 'health heartbeat database nats subsystems uptime checks',
    requires: GATES.cluster,
  },
  { to: '/system/nodes', label: 'Nodes', title: 'Cluster nodes', icon: IconCluster, keywords: 'cluster edge ha leader quorum', requires: GATES.cluster },
  {
    to: '/system/logs',
    label: 'Logs',
    title: 'System logs',
    icon: IconLogs,
    keywords: 'log records errors warnings debug server output',
    requires: GATES.logs,
  },
  {
    to: '/system/about',
    label: 'About',
    title: 'About this installation',
    icon: IconTerminal,
    keywords: 'version build addresses base url registry host endpoints configuration hangar.yaml instance name',
  },
];

/**
 * Personal pages, reached from the account menu rather than the sidebar. The
 * command palette lists them next to the sidebar pages so every page stays one search away.
 */
export const ACCOUNT_PAGES: NavSection = {
  title: 'Account',
  items: [
    { to: '/account', label: 'My account', icon: IconUser, keywords: 'profile password sessions devices personal' },
    { to: '/account', hash: 'my-tokens-title', label: 'Access tokens', icon: IconKey, keywords: 'personal access tokens api ci cli' },
    { to: '/api-docs', label: 'API reference', icon: IconTerminal, keywords: 'rest api openapi endpoints curl automation integration scripts token' },
  ],
};

const under = (pathname: string, prefix: string) => pathname === prefix || pathname.startsWith(`${prefix}/`);

/** The sidebar entry of `pathname`: its own entry, or that of the page it sits inside. */
export function activeNav(pathname: string): AppPath | undefined {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, '') : pathname;
  let best: AppPath | undefined;
  for (const item of NAV.flatMap((s) => s.items)) {
    const hit = item.to === '/' ? path === '/' : under(path, item.to);
    if (hit && (!best || item.to.length > best.length)) best = item.to;
  }
  return best;
}
