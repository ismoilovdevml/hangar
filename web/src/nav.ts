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
  IconTrash,
  IconUsers,
} from './components/icons';
import { IconAudit, IconSettings } from './components/opsIcons';
import { IconLogs, IconPulse } from './pages/system/icons';

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
  | '/settings';

export interface NavItem {
  to: AppPath;
  label: string;
  icon: ComponentType<SVGProps<SVGSVGElement> & { size?: number }>;
  keywords?: string;
}

export interface NavSection {
  title: string;
  items: NavItem[];
}

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
      { to: '/blob-stores', label: 'Blob stores', icon: IconServer, keywords: 's3 minio disk bucket' },
      { to: '/cleanup', label: 'Cleanup policies', icon: IconTrash, keywords: 'retention delete' },
    ],
  },
  {
    title: 'Security',
    items: [
      { to: '/vulnerabilities', label: 'Vulnerabilities', icon: IconShield, keywords: 'cve scanning findings quarantine' },
      { to: '/access', label: 'Access & audit', icon: IconUsers, keywords: 'users members service accounts roles permissions tokens' },
      { to: '/audit', label: 'Audit log', icon: IconAudit, keywords: 'events history who did export compliance' },
      { to: '/account', label: 'My account', icon: IconKey, keywords: 'profile password personal access tokens sessions devices' },
    ],
  },
  {
    title: 'Operations',
    items: [
      { to: '/tasks', label: 'Scheduled tasks', icon: IconClock, keywords: 'jobs cron' },
      { to: '/cluster', label: 'Cluster & replication', icon: IconCluster, keywords: 'nodes edge ha' },
      { to: '/system/status', label: 'System status', icon: IconPulse, keywords: 'health nodes heartbeat database nats subsystems uptime' },
      { to: '/system/logs', label: 'System logs', icon: IconLogs, keywords: 'log records errors warnings debug server output' },
      { to: '/settings', label: 'System', icon: IconSettings, keywords: 'settings status version health configuration' },
    ],
  },
];
