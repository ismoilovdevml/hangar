import type { QueryClient } from '@tanstack/react-query';
import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  Outlet,
  redirect,
  type RouterHistory,
} from '@tanstack/react-router';
import { ApiRequestError } from './api/client';
import { sessionQuery } from './api/hooks';
import { AppShell } from './components/AppShell';
import { isRange } from './lib/format';
import { LoginPage } from './pages/LoginPage';
import { OverviewPage } from './pages/overview/OverviewPage';
import { RepoCreatePage } from './pages/repositories/RepoCreatePage';
import { RepositoriesPage } from './pages/repositories/RepositoriesPage';
import { RepoSettingsPage } from './pages/repositories/settings/RepoSettingsPage';
import { BrowsePage } from './pages/browse/BrowsePage';
import { SearchPage } from './pages/search/SearchPage';
import { isBrowseTab, type BrowseTab } from './lib/packages';
import { isSearchSort } from './lib/searchQuery';
import { BlobStoresPage } from './pages/blobstores/BlobStoresPage';
import { BlobStoreDetailPage } from './pages/blobstores/BlobStoreDetailPage';
import type { SearchSort } from './api/types';
import { isFormat, isRepoSort, isRepoType, type RepoSort } from './lib/repositories';
import type { Format, RepositoryType } from './api/types';
import { AccessPage } from './pages/access/AccessPage';
import { isAccessTab, type AccessSearch } from './pages/access/model';
import { AccountPage } from './pages/access/AccountPage';
import { ClusterPage, NotFoundPage, VulnerabilitiesPage } from './pages/screens';
import { AuditPage } from './pages/audit/AuditPage';
import { CleanupPage, type CleanupSearch } from './pages/cleanup/CleanupPage';
import { SystemPage } from './pages/settings/SystemPage';
import { TaskDetailPage, type TaskDetailSearch } from './pages/tasks/TaskDetailPage';
import { TasksPage, type TasksSearch } from './pages/tasks/TasksPage';
import { validateAuditSearch, type AuditSearch } from './lib/audit';
import { isTaskStatusFilter, isTaskType } from './lib/tasks';

declare module '@tanstack/react-router' {
  interface StaticDataRouteOption {
    /** Shown as the last breadcrumb segment and in the document title. */
    title?: string;
  }
  interface HistoryState {
    /** Cursors of the pages before the current one, so "Previous" works with opaque cursors. */
    pageCursors?: string[];
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

/** Everything behind sign-in shares the shell and the session check. */
const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'app',
  beforeLoad: async ({ context, location }) => {
    try {
      await context.queryClient.ensureQueryData(sessionQuery);
    } catch (err) {
      if (err instanceof ApiRequestError && err.status === 401) {
        throw redirect({ to: '/login', search: { redirect: location.href } });
      }
      throw err;
    }
  },
  component: AppShell,
});

const page = <TPath extends string>(path: TPath, title: string, component: () => JSX.Element) =>
  createRoute({ getParentRoute: () => appRoute, path, staticData: { title }, component });

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
  staticData: { title: 'New repository' },
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
  staticData: { title: 'Cleanup policies' },
  validateSearch: (s: Record<string, unknown>): CleanupSearch => ({
    policy: str(s.policy),
    create: s.create === true || s.create === 'true' ? true : undefined,
  }),
  component: CleanupPage,
});

const tasksRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tasks',
  staticData: { title: 'Scheduled tasks' },
  validateSearch: (s: Record<string, unknown>): TasksSearch => ({
    type: isTaskType(s.type) ? s.type : undefined,
    status: isTaskStatusFilter(s.status) ? s.status : undefined,
    q: str(s.q),
  }),
  component: TasksPage,
});

const taskDetailRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tasks/$taskId',
  staticData: { title: 'Task' },
  validateSearch: (s: Record<string, unknown>): TaskDetailSearch => ({ run: str(s.run), cursor: str(s.cursor) }),
  component: TaskDetailPage,
});

const auditRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/audit',
  staticData: { title: 'Audit log' },
  validateSearch: (s: Record<string, unknown>): AuditSearch => validateAuditSearch(s),
  component: AuditPage,
});

const userStatuses = ['active', 'disabled', 'locked'] as const;
const pick = <T extends string>(v: unknown, allowed: readonly T[]): T | undefined =>
  allowed.includes(v as T) ? (v as T) : undefined;

const accessRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/access',
  staticData: { title: 'Access & audit' },
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
  staticData: { title: 'My account' },
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
  staticData: { title: 'Blob stores' },
  validateSearch: (s: Record<string, unknown>): { add?: boolean } => ({ add: s.add === true || s.add === 'true' ? true : undefined }),
  component: BlobStoresPage,
});

const blobStoreRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/blob-stores/$name',
  staticData: { title: 'Blob store' },
  component: BlobStoreDetailPage,
});

const routeTree = rootRoute.addChildren([
  loginRoute,
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
    page('/vulnerabilities', 'Vulnerabilities', VulnerabilitiesPage),
    accessRoute,
    accountRoute,
    tasksRoute,
    taskDetailRoute,
    page('/cluster', 'Cluster & replication', ClusterPage),
    auditRoute,
    page('/settings', 'System', SystemPage),
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
