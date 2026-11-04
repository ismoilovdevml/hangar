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
import { isFormat, isRepoType } from './lib/repositories';
import type { Format, RepositoryType } from './api/types';
import {
  AccessPage,
  BlobStoresPage,
  BrowsePage,
  CleanupPage,
  ClusterPage,
  NotFoundPage,
  SearchPage,
  TasksPage,
  VulnerabilitiesPage,
} from './pages/screens';

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
  validateSearch: (s: Record<string, unknown>): { redirect?: string } => ({ redirect: str(s.redirect) }),
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

const browseRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/browse',
  staticData: { title: 'Packages' },
  validateSearch: (s: Record<string, unknown>): { repo?: string; name?: string; version?: string } => ({
    repo: str(s.repo),
    name: str(s.name),
    version: str(s.version),
  }),
  component: BrowsePage,
});

export interface RepositoriesSearch {
  type?: RepositoryType;
  format?: Format;
  q?: string;
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
  validateSearch: (s: Record<string, unknown>): { q?: string } => ({ q: str(s.q) }),
  component: SearchPage,
});

const routeTree = rootRoute.addChildren([
  loginRoute,
  appRoute.addChildren([
    overviewRoute,
    browseRoute,
    searchRoute,
    repositoriesRoute,
    repoCreateRoute,
    page('/blob-stores', 'Blob stores', BlobStoresPage),
    page('/cleanup', 'Cleanup policies', CleanupPage),
    page('/vulnerabilities', 'Vulnerabilities', VulnerabilitiesPage),
    page('/access', 'Access & audit', AccessPage),
    page('/tasks', 'Scheduled tasks', TasksPage),
    page('/cluster', 'Cluster & replication', ClusterPage),
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
