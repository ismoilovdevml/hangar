/**
 * Thin aliases over the types generated from api/openapi.yaml.
 *
 * Regenerate src/api/schema.d.ts with `npm run gen:api` whenever the contract
 * changes; call sites import from here so they keep working across renames.
 */
import type { components, operations } from './schema';

type Schemas = components['schemas'];

export type Format = Schemas['Format'];
export type FormatInfo = Schemas['FormatInfo'];
export type RepositoryType = Schemas['RepositoryType'];
export type OverviewRange = Schemas['OverviewRange'];

/** Standard error body for every non-2xx JSON response. */
export type ApiError = Schemas['Error'];

/* ---------- system / session ---------- */

export type SystemStatus = Schemas['SystemStatus'];
export type AuthProviders = Schemas['AuthProviders'];
export type LoginRequest = Schemas['LoginRequest'];
export type Organization = Schemas['Organization'];
export type User = Schemas['User'];
export type Session = Schemas['Session'];
export type ClientEndpoint = Schemas['ClientEndpoint'];

/* ---------- overview ---------- */

export type OverviewMetrics = Schemas['OverviewMetrics'];
export type TrafficBucket = Schemas['TrafficBucket'];
export type ScanState = Schemas['ScanState'];
export type ScanSummary = Schemas['ScanSummary'];
export type Publisher = Schemas['Publisher'];
export type PublishedComponent = Schemas['PublishedComponent'];
export type UpstreamState = Schemas['UpstreamState'];
export type UpstreamHealth = Schemas['UpstreamHealth'];
export type SearchHit = Schemas['SearchHit'];

/** Every cursor-paginated list has this shape. */
export interface Page<T> {
  items: T[];
  next_cursor?: string | null;
}

/* ---------- repositories ---------- */

export type WritePolicy = Schemas['WritePolicy'];
export type UpstreamAuth = Schemas['UpstreamAuth'];
export type UpstreamHTTP = Schemas['UpstreamHTTP'];
export type RepositoryConfig = Schemas['RepositoryConfig'];
export type RepositoryInput = Schemas['RepositoryInput'];
export type RepositoryStatus = Schemas['RepositoryStatus'];
export type Repository = Schemas['Repository'];
export type RepositoryList = Schemas['RepositoryList'];
export type RepositoryCounts = RepositoryList['counts'];
export type ListRepositoriesQuery = NonNullable<operations['listRepositories']['parameters']['query']>;

/* ---------- blob stores / cleanup ---------- */

export type BlobStoreInput = Schemas['BlobStoreInput'];
export type BlobStore = Schemas['BlobStore'];
export type BlobStoreState = BlobStore['state'];
export type BlobStoreList = Schemas['BlobStoreList'];
export type CleanupPolicy = Schemas['CleanupPolicy'];
export type CleanupPolicyPage = Schemas['CleanupPolicyPage'];

/* ---------- components, assets, search ---------- */

export type ComponentDetail = Schemas['ComponentDetail'];
export type PublishedComponentPage = Schemas['PublishedComponentPage'];
export type Asset = Schemas['Asset'];
export type AssetPage = Schemas['AssetPage'];
export type Finding = Schemas['Finding'];
export type FindingPage = Schemas['FindingPage'];
export type Severity = Schemas['Severity'];
export type FacetValue = Schemas['FacetValue'];
export type SearchResult = Schemas['SearchResult'];
export type SearchSort = NonNullable<NonNullable<operations['search']['parameters']['query']>['sort']>;
export type ComponentSort = NonNullable<NonNullable<operations['listComponents']['parameters']['query']>['sort']>;

/* ---------- live events (SSE /api/v1/events) ---------- */

export type LiveEvent = Schemas['LiveEvent'];
export type LiveEventType = LiveEvent['type'];

/* Repository browse tree, asset detail and package facts. */
export type BrowseEntry = Schemas['BrowseEntry'];
export type BrowseEntryKind = BrowseEntry['kind'];
export type BrowsePage = Schemas['BrowsePage'];
export type BrowseComponent = Schemas['BrowseComponent'];
export type AssetDetail = Schemas['AssetDetail'];
export type ComponentInfo = Schemas['ComponentInfo'];
export type Readme = Schemas['Readme'];
