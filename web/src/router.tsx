import type { QueryClient } from '@tanstack/react-query';
import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  lazyRouteComponent,
  Outlet,
  redirect,
  type RouteComponent,
  type RouterHistory,
} from '@tanstack/react-router';
import './pageStyles';
import { ApiRequestError } from './api/client';
import { sessionQuery } from './api/hooks';
import { AppShell } from './components/AppShell';
import { isRange } from './lib/format';
import { LoginPage } from './pages/LoginPage';
import { ChangePasswordPage } from './pages/ChangePasswordPage';
import { safeRedirect } from './lib/redirect';
import { GATES, type Gate } from './lib/gates';
import { DocsShell } from './pages/apidocs/DocsShell';
import { isBrowseTab, type BrowseTab } from './lib/packages';
import { isSearchSort } from './lib/searchQuery';
import type { SearchSort } from './api/types';
import { isFormat, isRepoSort, isRepoType, type RepoSort } from './lib/repositories';
import type { Format, RepositoryType } from './api/types';
import { isAccessTab, type AccessSearch } from './pages/access/model';
import { NotFoundPage } from './pages/screens';
import { validateVulnSearch, type VulnSearch } from './lib/vulnerabilities';
import type { CleanupSearch } from './pages/cleanup/CleanupPage';
import type { TaskDetailSearch } from './pages/tasks/TaskDetailPage';
import type { TasksSearch } from './pages/tasks/TasksPage';
import type { NewTaskSearch } from './pages/tasks/NewTaskPage';
import { isImportKind } from './lib/imports';
import { validateAuditSearch, type AuditSearch } from './lib/audit';
import { isTaskStatusFilter, isTaskType } from './lib/tasks';
import { validateLogsSearch, type SystemLogsSearch } from './pages/system/model';

// Pages load on first visit (or on hover of a link to them), so the first
// load fetches only the shell and the page it opens.
const ApiDocsPage = lazyRouteComponent(() => import('./pages/apidocs/ApiDocsPage'), 'ApiDocsPage');
const OverviewPage = lazyRouteComponent(() => import('./pages/overview/OverviewPage'), 'OverviewPage');
const RepoCreatePage = lazyRouteComponent(() => import('./pages/repositories/RepoCreatePage'), 'RepoCreatePage');
const RepositoriesPage = lazyRouteComponent(() => import('./pages/repositories/RepositoriesPage'), 'RepositoriesPage');
const RepoSettingsPage = lazyRouteComponent(() => import('./pages/repositories/settings/RepoSettingsPage'), 'RepoSettingsPage');
const BrowsePage = lazyRouteComponent(() => import('./pages/browse/BrowsePage'), 'BrowsePage');
const SearchPage = lazyRouteComponent(() => import('./pages/search/SearchPage'), 'SearchPage');
const BlobStoresPage = lazyRouteComponent(() => import('./pages/blobstores/BlobStoresPage'), 'BlobStoresPage');
const BlobStoreDetailPage = lazyRouteComponent(() => import('./pages/blobstores/BlobStoreDetailPage'), 'BlobStoreDetailPage');
const AccessPage = lazyRouteComponent(() => import('./pages/access/AccessPage'), 'AccessPage');
const AccountPage = lazyRouteComponent(() => import('./pages/access/AccountPage'), 'AccountPage');
const ClusterPage = lazyRouteComponent(() => import('./pages/cluster/ClusterPage'), 'ClusterPage');
const VulnerabilitiesPage = lazyRouteComponent(() => import('./pages/vulnerabilities/VulnerabilitiesPage'), 'VulnerabilitiesPage');
const AuditPage = lazyRouteComponent(() => import('./pages/audit/AuditPage'), 'AuditPage');
const CleanupPage = lazyRouteComponent(() => import('./pages/cleanup/CleanupPage'), 'CleanupPage');
const SettingsHubPage = lazyRouteComponent(() => import('./pages/settings/SettingsHubPage'), 'SettingsHubPage');
const SystemPage = lazyRouteComponent(() => import('./pages/settings/SystemPage'), 'SystemPage');
const AuthenticationPage = lazyRouteComponent(() => import('./pages/settings/AuthenticationPage'), 'AuthenticationPage');
const ProxiesPage = lazyRouteComponent(() => import('./pages/settings/ProxiesPage'), 'ProxiesPage');
const TaskDetailPage = lazyRouteComponent(() => import('./pages/tasks/TaskDetailPage'), 'TaskDetailPage');
const TasksPage = lazyRouteComponent(() => import('./pages/tasks/TasksPage'), 'TasksPage');
const NewTaskPage = lazyRouteComponent(() => import('./pages/tasks/NewTaskPage'), 'NewTaskPage');
const SystemStatusPage = lazyRouteComponent(() => import('./pages/system/SystemStatusPage'), 'SystemStatusPage');
const SystemLogsPage = lazyRouteComponent(() => import('./pages/system/SystemLogsPage'), 'SystemLogsPage');

declare module '@tanstack/react-router' {
  interface StaticDataRouteOption {
    /** Shown as the last breadcrumb segment and in the document title. */
    title?: string;
    /** The page sits under Settings: the breadcrumb reads "Settings / <title>". */
    inSettings?: boolean;
    /** Users the gate turns away see who to ask for access instead of the page. */
    gate?: Gate;
  }
  interface HistoryState {
    /** Cursors of the pages before the current one, so "Previous" works with opaque cursors. */
    pageCursors?: string[];
    /** Set on the repository page right after "New repository" created it. */
    created?: string;
  }
}

export interface RouterContext {
  queryClient: QueryClient;
}

const str = (v: unknown) => (typeof v === 'string' && v.length > 0 ? v : undefined);

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: Outlet,
  notFoundComponent: NotFoundPage,
});

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  staticData: { title: 'Sign in' },
  validateSearch: (s: Record<string, unknown>): { redirect?: string; error?: string } => ({
    redirect: str(s.redirect),
    error: str(s.error),
  }),
  component: LoginPage,
});

/** A user who must replace their password sees only this page until they have. */
const changePasswordRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/change-password',
  staticData: { title: 'Set a new password' },
  validateSearch: (s: Record<string, unknown>): { redirect?: string } => ({ redirect: str(s.redirect) }),
  beforeLoad: async ({ context, location, search }) => {
    let session;
    try {
      // Fetched again when stale: the flag may have been set or cleared meanwhile.
      session = await context.queryClient.fetchQuery(sessionQuery);
    } catch (err) {
      if (err instanceof ApiRequestError && err.status === 401) {
        throw redirect({ to: '/login', search: { redirect: search.redirect ?? location.href } });
      }
      throw err;
    }
    if (!session.password_change_required) throw redirect({ href: safeRedirect(search.redirect), replace: true });
  },
  component: ChangePasswordPage,
});

/** Public pages that use the app shell when signed in and stand alone otherwise. */
const docsRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'docs',
  component: DocsShell,
});

/** The API reference renders the server's public OpenAPI document. */
const apiDocsRoute = createRoute({
  getParentRoute: () => docsRoute,
  path: '/api-docs',
  staticData: { title: 'API reference', inSettings: true },
  component: ApiDocsPage,
});

/** Everything behind sign-in shares the shell and the session check. */
const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'app',
  beforeLoad: async ({ context, location }) => {
    let session;
    try {
      session = await context.queryClient.ensureQueryData(sessionQuery);
    } catch (err) {
      if (err instanceof ApiRequestError && err.status === 401) {
        throw redirect({ to: '/login', search: { redirect: location.href } });
      }
      throw err;
    }
    if (session.password_change_required) {
      throw redirect({ to: '/change-password', search: { redirect: location.href } });
    }
  },
  component: AppShell,
});

const page = <TPath extends string>(path: TPath, title: string, component: RouteComponent, inSettings?: boolean, gate?: Gate) =>
  createRoute({ getParentRoute: () => appRoute, path, staticData: { title, inSettings, gate }, component });

const overviewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/',
  staticData: { title: 'Overview' },
  validateSearch: (s: Record<string, unknown>): { range?: '1h' | '7d' | '30d' | 'today' } => ({
    range: isRange(s.range) ? s.range : undefined,
  }),
  component: OverviewPage,
});

export interface BrowseSearch {
  repo?: string;
  /** Namespace (scope, groupId, path); absent = any namespace with this name. */
  ns?: string;
  name?: string;
  version?: string;
  tab?: BrowseTab;
  /** Selected node of the browse tree: a folder ("dir/") or a file path. */
  path?: string;
  /** Asset id of the selected file. */
  asset?: string;
  /** Group member that holds the selected package (browsing a group). */
  member?: string;
}

const browseRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/browse',
  staticData: { title: 'Packages' },
  validateSearch: (s: Record<string, unknown>): BrowseSearch => ({
    repo: str(s.repo),
    ns: str(s.ns),
    name: str(s.name),
    version: str(s.version),
    tab: isBrowseTab(s.tab) ? s.tab : undefined,
    path: str(s.path),
    asset: str(s.asset),
    member: str(s.member),
  }),
  component: BrowsePage,
});

export interface RepositoriesSearch {
  type?: RepositoryType;
  format?: Format;
  q?: string;
  sort?: RepoSort;
  cursor?: string;
}

const repositoriesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/repositories',
  staticData: { title: 'Repositories' },
  validateSearch: (s: Record<string, unknown>): RepositoriesSearch => ({
    type: isRepoType(s.type) ? s.type : undefined,
    format: isFormat(s.format) ? s.format : undefined,
    q: str(s.q),
    sort: isRepoSort(s.sort) && s.sort !== 'name' ? s.sort : undefined,
    cursor: str(s.cursor),
  }),
  component: RepositoriesPage,
});

const repoCreateRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/repositories/new',
  staticData: { title: 'New repository', gate: GATES.createRepository },
  validateSearch: (s: Record<string, unknown>): { format?: Format; type?: RepositoryType } => ({
    format: isFormat(s.format) ? s.format : undefined,
    type: isRepoType(s.type) ? s.type : undefined,
  }),
  component: RepoCreatePage,
});

const searchRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/search',
  staticData: { title: 'Search' },
  validateSearch: (s: Record<string, unknown>): { q?: string; sort?: SearchSort } => ({
    q: str(s.q),
    sort: isSearchSort(s.sort) ? s.sort : undefined,
  }),
  component: SearchPage,
});

const cleanupRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/cleanup',
  staticData: { title: 'Cleanup policies', gate: GATES.cleanup },
  validateSearch: (s: Record<string, unknown>): CleanupSearch => ({
    policy: str(s.policy),
    create: s.create === true || s.create === 'true' ? true : undefined,
  }),
  component: CleanupPage,
});

const tasksRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tasks',
  staticData: { title: 'Scheduled tasks', gate: GATES.tasks },
  validateSearch: (s: Record<string, unknown>): TasksSearch => ({
    type: isTaskType(s.type) ? s.type : undefined,
    status: isTaskStatusFilter(s.status) ? s.status : undefined,
    q: str(s.q),
  }),
  component: TasksPage,
});

const newTaskRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tasks/new',
  staticData: { title: 'New task', gate: GATES.newTask },
  validateSearch: (s: Record<string, unknown>): NewTaskSearch => ({
    kind: isImportKind(s.kind) ? s.kind : undefined,
    repository: str(s.repository),
  }),
  component: NewTaskPage,
});

const taskDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tasks/$taskId',
  staticData: { title: 'Task', gate: GATES.tasks },
  validateSearch: (s: Record<string, unknown>): TaskDetailSearch => ({ run: str(s.run), cursor: str(s.cursor) }),
  component: TaskDetailPage,
});

const vulnerabilitiesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/vulnerabilities',
  staticData: { title: 'Vulnerabilities' },
  validateSearch: (s: Record<string, unknown>): VulnSearch => validateVulnSearch(s),
  component: VulnerabilitiesPage,
});

const auditRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/audit',
  staticData: { title: 'Audit log', inSettings: true, gate: GATES.audit },
  validateSearch: (s: Record<string, unknown>): AuditSearch => validateAuditSearch(s),
  component: AuditPage,
});

const userStatuses = ['active', 'disabled', 'locked'] as const;
const pick = <T extends string>(v: unknown, allowed: readonly T[]): T | undefined =>
  allowed.includes(v as T) ? (v as T) : undefined;

const accessRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/access',
  staticData: { title: 'Access & audit', gate: GATES.access },
  validateSearch: (s: Record<string, unknown>): AccessSearch => ({
    tab: isAccessTab(s.tab) ? s.tab : undefined,
    q: str(s.q),
    role: str(s.role),
    status: pick(s.status, userStatuses),
    user: str(s.user),
    events: str(s.events),
    owner_kind: pick(s.owner_kind, ['user', 'service_account'] as const),
    token_status: pick(s.token_status, ['active', 'expired'] as const),
  }),
  component: AccessPage,
});

const accountRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/account',
  staticData: { title: 'My account', inSettings: true },
  component: AccountPage,
});

/** Settings live under the name (not at /repositories/$name) so a repository called "new" stays reachable. */
const repoSettingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/repositories/$name/settings',
  staticData: { title: 'Repository settings' },
  component: RepoSettingsPage,
});

const blobStoresRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/blob-stores',
  staticData: { title: 'Blob stores', gate: GATES.blobStores },
  validateSearch: (s: Record<string, unknown>): { add?: boolean } => ({ add: s.add === true || s.add === 'true' ? true : undefined }),
  component: BlobStoresPage,
});

const blobStoreRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/blob-stores/$name',
  staticData: { title: 'Blob store', gate: GATES.blobStores },
  component: BlobStoreDetailPage,
});

const systemStatusRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/system/status',
  staticData: { title: 'System status', inSettings: true, gate: GATES.cluster },
  component: SystemStatusPage,
});

const systemLogsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/system/logs',
  staticData: { title: 'System logs', inSettings: true, gate: GATES.logs },
  validateSearch: (s: Record<string, unknown>): SystemLogsSearch => validateLogsSearch(s),
  component: SystemLogsPage,
});

const routeTree = rootRoute.addChildren([
  loginRoute,
  changePasswordRoute,
  docsRoute.addChildren([apiDocsRoute]),
  appRoute.addChildren([
    overviewRoute,
    browseRoute,
    searchRoute,
    repositoriesRoute,
    repoCreateRoute,
    repoSettingsRoute,
    blobStoresRoute,
    blobStoreRoute,
    cleanupRoute,
    vulnerabilitiesRoute,
    accessRoute,
    accountRoute,
    tasksRoute,
    newTaskRoute,
    taskDetailRoute,
    page('/cluster', 'Cluster & replication', ClusterPage, false, GATES.cluster),
    systemStatusRoute,
    systemLogsRoute,
    auditRoute,
    // Settings gathers configuration and personal pages; their old addresses stay as they were.
    page('/settings', 'Settings', SettingsHubPage),
    page('/settings/system', 'System', SystemPage, true),
    page('/settings/authentication', 'Authentication', AuthenticationPage, true, GATES.settings),
    page('/settings/proxies', 'Proxies', ProxiesPage, true, GATES.settings),
  ]),
]);

export function createAppRouter(queryClient: QueryClient, history?: RouterHistory) {
  return createRouter({
    routeTree,
    context: { queryClient },
    history,
    defaultPreload: 'intent',
    scrollRestoration: true,
  });
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
