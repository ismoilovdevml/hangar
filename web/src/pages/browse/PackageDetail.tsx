import { useRef, useState, type KeyboardEvent } from 'react';
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router';
import { useComponentInfo, useInvalidateBrowse, usePackageHolders } from '../../api/browse';
import { ApiRequestError } from '../../api/client';
import {
  flatItems,
  useAllRepositories,
  useComponent,
  useComponentAssets,
  useComponentFindings,
  useComponentVersions,
  useDeleteComponent,
  useRepository,
} from '../../api/catalog';
import { usePermissions } from '../../api/permissions';
import type { Asset, ComponentDetail, ComponentInfo, Finding, PublishedComponent } from '../../api/types';
import { ClientSetup } from '../../components/ClientSetup';
import { CodeBlock } from '../../components/CodeBlock';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { CopyButton } from '../../components/CopyButton';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { IconTrash } from '../../components/icons';
import { IconDownload } from '../../components/opsIcons';
import { NotAvailable } from '../../components/NotAvailable';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { formatBytes, formatCompact, relativeTime } from '../../lib/format';
import { componentName, unsupportedText } from '../../lib/overview';
import { downloadHref, hasVersion, securityPill, type BrowseTab } from '../../lib/packages';
import { repoClientTarget } from '../../lib/repoSettings';
import { TYPE_META } from '../../lib/repositories';
import { useNow } from '../../lib/useNow';
import { imageWhere, packageAt } from '../../lib/vulnerabilities';
import { Fact } from './AssetDetail';
import { dateTime, deleteMessage, shortDigest } from './detailUtil';
import { FormatIcon } from '../../components/FormatIcon';
import { Markdown, MarkdownInline } from '../../components/Markdown';
import { safeHref, safeImageSrc } from '../../components/urlPolicy';

const route = getRouteApi('/app/browse');

interface PackageDetailProps {
  id: string;
  /** Versions of this package found by the lookup, newest first. */
  versions: PublishedComponent[];
}

/** The largest downloadable content file: the package itself. */
const mainAsset = (assets: Asset[]) =>
  assets.filter((a) => a.kind === 'content' && a.download_url).sort((a, b) => b.size_bytes - a.size_bytes)[0];

export function PackageDetail({ id, versions }: PackageDetailProps) {
  const search = route.useSearch();
  const navigate = useNavigate();
  const detail = useComponent(id);
  const c = detail.data && detail.data.id === id ? detail.data : undefined;
  const repo = useRepository(c?.repository);
  const findings = useComponentFindings(c?.id);
  const info = useComponentInfo(c?.id);
  const assets = useComponentAssets(c?.id);
  const perms = usePermissions();
  const remove = useDeleteComponent();
  const invalidateBrowse = useInvalidateBrowse();
  const [toDelete, setToDelete] = useState<PublishedComponent | null>(null);
  const now = useNow();

  if (detail.isError) {
    return (
      <Panel>
        <ErrorNote what="the package" error={detail.error} onRetry={() => void detail.refetch()} />
      </Panel>
    );
  }
  if (!c) {
    return (
      <Panel aria-busy="true" className="browse-empty">
        <p>Loading…</p>
      </Panel>
    );
  }

  const label = componentName(c);
  const canDelete = perms.can('delete', 'repo', c.repository);
  const pill = securityPill(c.scan);
  const newest = versions[0]?.id === c.id;
  const cached = repo.data?.type === 'proxy';
  const base = { repo: search.repo ?? c.repository, path: search.path, member: search.member, ns: c.namespace || undefined, name: c.name };
  const hasFindings = !!findings.data;
  const available: BrowseTab[] = [
    'readme',
    'versions',
    'files',
    'repositories',
    ...(hasFindings ? (['security'] as const) : []),
  ];
  const tab: BrowseTab = search.tab && available.includes(search.tab) ? search.tab : 'readme';
  const setTab = (t: BrowseTab) => void navigate({ to: '/browse', search: (s) => ({ ...s, tab: t === 'readme' ? undefined : t }), replace: true });

  /** The search of another version; a version folder selected in the tree follows along. */
  const versionSearch = (v: string) => {
    const suffix = `/${c.version}/`;
    const path = search.path && c.version && search.path.toLowerCase().endsWith(suffix.toLowerCase()) ? `${search.path.slice(0, -suffix.length)}/${v}/` : search.path;
    return { ...base, path, version: v, tab: search.tab };
  };

  const askDelete = (v: PublishedComponent) => {
    remove.reset();
    setToDelete(v);
  };
  const confirmDelete = () => {
    if (!toDelete) return;
    const deleted = toDelete;
    remove.mutate(deleted.id, {
      onSuccess: () => {
        setToDelete(null);
        invalidateBrowse();
        if (deleted.id !== c.id) return;
        const remaining = versions.filter((v) => v.id !== deleted.id);
        void navigate({
          to: '/browse',
          // Stay on the package (its newest remaining version) unless the deleted one was the last.
          search: remaining.length ? { ...base, path: search.path?.endsWith(`/${c.version}/`) ? undefined : search.path, tab: search.tab } : { repo: base.repo },
          replace: true,
        });
      },
    });
  };

  const description = info.data?.description || (typeof c.attributes?.description === 'string' ? c.attributes.description : '');
  const download = mainAsset(flatItems(assets.data));
  const totalDownloads = versions.reduce((n, v) => n + (v.download_count ?? 0), 0);

  return (
    <div className="pkg-layout">
      <Panel className="pkg-head pkg-layout__main" aria-label={`${label} ${c.version}`}>
        <div className="pkg-head__row">
          <PackageMark format={c.format} iconUrl={info.data?.icon_url} />
          <div className="pkg-head__text">
            <div className="pkg-head__titles">
              <h1 className="pkg-head__title">{label}</h1>
              {hasVersion(c.version) && <Pill tone="info">{c.version}</Pill>}
              {newest && versions.length > 1 && <Pill tone="ok">Newest</Pill>}
              {c.signed && <Pill tone="ok">Signed</Pill>}
              {c.quarantined && <Pill tone="bad">Quarantined</Pill>}
              <Pill tone={pill.tone}>{pill.label}</Pill>
            </div>
            <p className="pkg-head__meta">
              {[description, c.platforms?.length ? c.platforms.join(', ') : ''].filter(Boolean).join(' · ')}
              {description || c.platforms?.length ? ' · ' : ''}
              {cached ? 'cached ' : `published by ${c.publisher.display_name} `}
              <time dateTime={c.published_at} title={dateTime(c.published_at)}>
                {relativeTime(c.published_at, now).toLowerCase()}
              </time>
            </p>
            {c.quarantined && c.quarantine_reason && <p className="form-error">Quarantined: {c.quarantine_reason}</p>}
          </div>
          <div className="pkg-head__actions">
            {download?.download_url && (
              <a href={downloadHref(download.download_url)} className="button button--primary" download title={download.path}>
                <IconDownload size={16} /> Download
              </a>
            )}
            {versions.length > 1 && (
              <button type="button" className="button button--secondary" onClick={() => setTab('versions')}>
                {versions.length} versions
              </button>
            )}
            <button
              type="button"
              className="button button--danger-outline"
              disabled={!canDelete}
              title={canDelete ? undefined : 'Needs delete rights on this repository'}
              onClick={() => askDelete(c)}
            >
              Delete version
            </button>
          </div>
        </div>

        <Tabs tabs={available} active={tab} onChange={setTab} counts={{ versions: versions.length }} format={c.format} />

        <div role="tabpanel" id={`pkg-panel-${tab}`} aria-labelledby={`pkg-tab-${tab}`} className="pkg-panel">
          {tab === 'readme' && <ReadmeTab c={c} q={info} />}
          {tab === 'versions' && <VersionsTable c={c} newestId={versions[0]?.id} cached={cached} canDelete={canDelete} onDelete={askDelete} versionSearch={versionSearch} now={now} />}
          {tab === 'files' && <FilesTable c={c} now={now} />}
          {tab === 'repositories' && <RepositoriesTab c={c} now={now} />}
          {tab === 'security' && findings.data && <FindingsTable items={findings.data.items} />}
        </div>
      </Panel>

      <div className="pkg-layout__side">
        <VersionCard c={c} newest={newest} cached={cached} totalDownloads={totalDownloads} info={info.data} infoPending={info.isPending && !info.isError} findings={findings.data?.items} now={now} />
        <Panel title="Set me up" titleId="setup-title" className="pkg-setup">
          {repo.data ? (
            <ClientSetup targets={[{ ...repoClientTarget(repo.data), publishes: false }]} install={{ namespace: c.namespace, name: c.name, version: c.version }} />
          ) : repo.isError ? (
            <CodeBlock label="install command" lines={[{ text: c.install_command }]} />
          ) : (
            <p className="meta">Loading…</p>
          )}
        </Panel>
      </div>

      {toDelete && (
        <ConfirmDialog
          title={`Delete ${label}${hasVersion(toDelete.version) ? ` ${toDelete.version}` : ''}?`}
          confirmLabel="Delete version"
          tone="danger"
          busy={remove.isPending}
          error={remove.isError ? deleteMessage(remove.error, 'versions') : null}
          onCancel={() => setToDelete(null)}
          onConfirm={confirmDelete}
        >
          <p>
            Clients will no longer be able to download it from <strong>{c.repository}</strong>. Its files are released for compaction. This cannot be
            undone.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}

const TAB_LABEL: Record<BrowseTab, string> = {
  readme: 'Readme',
  versions: 'Versions',
  files: 'Files',
  repositories: 'Repositories',
  security: 'Vulnerabilities',
};

function Tabs({
  tabs,
  active,
  onChange,
  counts,
  format,
}: {
  tabs: BrowseTab[];
  active: BrowseTab;
  onChange: (t: BrowseTab) => void;
  counts: Partial<Record<BrowseTab, number>>;
  format: string;
}) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const onKeyDown = (e: KeyboardEvent<HTMLButtonElement>, i: number) => {
    let next = -1;
    if (e.key === 'ArrowRight') next = (i + 1) % tabs.length;
    else if (e.key === 'ArrowLeft') next = (i - 1 + tabs.length) % tabs.length;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = tabs.length - 1;
    if (next < 0) return;
    e.preventDefault();
    onChange(tabs[next]!);
    refs.current[next]?.focus();
  };
  return (
    <div role="tablist" aria-label="Package details" className="pkg-tabs">
      {tabs.map((t, i) => (
        <button
          key={t}
          ref={(el) => {
            refs.current[i] = el;
          }}
          type="button"
          role="tab"
          id={`pkg-tab-${t}`}
          aria-selected={t === active}
          aria-controls={`pkg-panel-${t}`}
          tabIndex={t === active ? 0 : -1}
          className={t === active ? 'pkg-tabs__tab is-active' : 'pkg-tabs__tab'}
          onClick={() => onChange(t)}
          onKeyDown={(e) => onKeyDown(e, i)}
        >
          {t === 'files' && format === 'docker' ? 'Layers' : TAB_LABEL[t]}
          {counts[t] !== undefined && <span className="pkg-tabs__count"> {counts[t]}</span>}
        </button>
      ))}
    </div>
  );
}

/* ---------------------------------------------------------------- version card */

function vulnerabilities(c: ComponentDetail, findings: Finding[] | undefined): string {
  if (findings) {
    const open = findings.filter((f) => f.status === 'open' || f.status === 'assigned');
    if (!open.length) return 'No known vulnerabilities';
    const by = (s: string) => open.filter((f) => f.severity === s).length;
    return (['critical', 'high', 'medium', 'low'] as const)
      .map((s) => [s, by(s)] as const)
      .filter(([, n]) => n > 0)
      .map(([s, n]) => `${n} ${s}`)
      .join(' · ');
  }
  const s = c.scan;
  switch (s.state) {
    case 'pending':
      return 'Not scanned';
    case 'clean':
      return 'No known vulnerabilities';
    case 'quarantined':
      return 'Quarantined';
    case 'findings':
      return `${s.critical} critical · ${s.high} high · ${s.medium} medium · ${s.low} low`;
    case 'failed':
      return s.scan_error ? `Scan failed: ${s.scan_error}` : 'Scan failed';
    case 'unsupported':
      return unsupportedText(s.scan_error, c.format);
  }
}

function LicenseValue({ value }: { value: string }) {
  const href = /^https?:\/\//.test(value) ? safeHref(value) : null;
  return href ? (
    <a href={href} target="_blank" rel="noopener noreferrer nofollow">
      {value.replace(/^https?:\/\//, '')}
    </a>
  ) : (
    <>{value}</>
  );
}

interface VersionCardProps {
  c: ComponentDetail;
  newest: boolean;
  cached: boolean;
  totalDownloads: number;
  info: ComponentInfo | undefined;
  infoPending: boolean;
  findings: Finding[] | undefined;
  now: Date;
}

function VersionCard({ c, newest, cached, totalDownloads, info, infoPending, findings, now }: VersionCardProps) {
  const pending = <span className="meta">Reading the package…</span>;
  return (
    <Panel title="Version" titleId="version-card-title" className="version-card">
      <dl className="facts facts--stack">
        <Fact label="Version">
          {hasVersion(c.version) ? <span className="facts__mono">{c.version}</span> : <NotAvailable reason="no version" />}
          {newest && <span className="meta"> · newest</span>}
        </Fact>
        <Fact label={cached ? 'Cached' : 'Published'}>
          <time dateTime={c.published_at} title={dateTime(c.published_at)}>
            {relativeTime(c.published_at, now)}
          </time>
          {!cached && <span className="meta"> · {c.publisher.display_name}</span>}
        </Fact>
        <Fact label="Downloads">
          {formatCompact(c.download_count ?? 0)} this version
          <span className="meta"> · {formatCompact(Math.max(totalDownloads, c.download_count ?? 0))} in total</span>
        </Fact>
        <Fact label="Last downloaded">
          {c.last_downloaded_at ? (
            <time dateTime={c.last_downloaded_at} title={dateTime(c.last_downloaded_at)}>
              {relativeTime(c.last_downloaded_at, now)}
            </time>
          ) : (
            'Never'
          )}
        </Fact>
        <Fact label="Size">{formatBytes(c.size_bytes)}</Fact>
        <Fact label="Vulnerabilities">{vulnerabilities(c, findings)}</Fact>
        <Fact label="Licence">
          {infoPending
            ? pending
            : (info?.licenses.length ?? 0) > 0
              ? info!.licenses.map((l, i) => (
                  <span key={l}>
                    {i > 0 && ', '}
                    <LicenseValue value={l} />
                  </span>
                ))
              : c.license || 'Not declared'}
        </Fact>
        {info?.homepage && safeHref(info.homepage) && (
          <Fact label="Home page">
            <a href={safeHref(info.homepage)!} target="_blank" rel="noopener noreferrer nofollow" className="facts__break">
              {info.homepage.replace(/^https?:\/\//, '')}
            </a>
          </Fact>
        )}
        {c.digest && (
          <Fact label="Digest">
            <span className="facts__mono facts__copy">
              <span title={c.digest}>{shortDigest(c.digest)}</span>
              <CopyButton text={c.digest} label="Copy digest" />
            </span>
          </Fact>
        )}
        {c.base_image && <Fact label="Base image">{c.base_image}</Fact>}
        {c.source_url && (
          <Fact label="Source">
            {safeHref(c.source_url) ? (
              <a href={safeHref(c.source_url)!} target="_blank" rel="noreferrer noopener" className="facts__break">
                {c.source_url.replace(/^https?:\/\//, '')}
              </a>
            ) : (
              c.source_url
            )}
          </Fact>
        )}
      </dl>
      {info && info.keywords.length > 0 && (
        <div className="keywords" aria-label="Keywords">
          {info.keywords.map((k) => (
            <Pill key={k} tone="neutral">
              {k}
            </Pill>
          ))}
        </div>
      )}
    </Panel>
  );
}

/* ---------------------------------------------------------------- header */

/** The package's own icon when it has one (NuGet), else the format's logo. */
function PackageMark({ format, iconUrl }: { format: string; iconUrl?: string }) {
  const [failed, setFailed] = useState<string | null>(null);
  const src = iconUrl ? safeImageSrc(iconUrl) : null;
  return (
    <span className="pkg-head__mark">
      {src && failed !== src ? (
        <img className="pkg-head__icon" src={src} alt="" width={36} height={36} referrerPolicy="no-referrer" onError={() => setFailed(src)} />
      ) : (
        <FormatIcon format={format} size={32} />
      )}
    </span>
  );
}

/* ---------------------------------------------------------------- readme */

function ReadmeTab({ c, q }: { c: ComponentDetail; q: ReturnType<typeof useComponentInfo> }) {
  if (q.isError) {
    if (q.error instanceof ApiRequestError && q.error.status === 501) return <p className="meta">This server does not read package READMEs.</p>;
    return <ErrorNote what="the README" error={q.error} onRetry={() => void q.refetch()} />;
  }
  if (!q.data) return <p className="meta" aria-busy="true">Reading the README…</p>;
  const r = q.data.readme;
  if (!r) {
    return (
      <div className="readme-empty">
        <p>No README was published with this version.</p>
        {q.data.description && (
          <p className="meta">
            {c.format === 'maven' ? 'The POM describes it as: ' : 'Its description: '}
            {q.data.description}
          </p>
        )}
      </div>
    );
  }
  return (
    <article className="readme" aria-label="README">
      {r.format === 'markdown' ? <Markdown source={r.content} images={r.images} /> : <pre className="readme__text">{r.content}</pre>}
      <p className="meta readme__source">
        From {r.source}
        {r.truncated ? '; cut at 512 KiB' : ''}.
        {r.format === 'markdown' && q.data.readme_images === 'off' && ' Images hosted elsewhere are turned off on this server; their descriptions are shown instead.'}
      </p>
    </article>
  );
}

/* ---------------------------------------------------------------- tables */

function LoadMore({ q }: { q: { hasNextPage: boolean; isFetchingNextPage: boolean; fetchNextPage: () => unknown } }) {
  if (!q.hasNextPage) return null;
  return (
    <div className="pkg-more">
      <button type="button" className="button button--secondary button--sm" disabled={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()}>
        {q.isFetchingNextPage ? 'Loading…' : 'Load more'}
      </button>
    </div>
  );
}

interface VersionsTableProps {
  c: ComponentDetail;
  newestId: string | undefined;
  cached: boolean;
  canDelete: boolean;
  onDelete: (v: PublishedComponent) => void;
  versionSearch: (v: string) => Record<string, string | undefined>;
  now: Date;
}

function VersionsTable({ c, newestId, cached, canDelete, onDelete, versionSearch, now }: VersionsTableProps) {
  const q = useComponentVersions(c.id);
  const rows = flatItems(q.data);
  const columns: Column<PublishedComponent>[] = [
    {
      key: 'version',
      header: 'Version',
      className: 'col-first',
      cell: (v) => (
        <span className="version-cell">
          <Link to="/browse" search={versionSearch(v.version)} className="link-strong" aria-current={v.id === c.id ? 'page' : undefined}>
            {hasVersion(v.version) ? v.version : <NotAvailable reason="no version" />}
          </Link>
          {v.id === newestId && <Pill tone="ok">Newest</Pill>}
        </span>
      ),
    },
    {
      key: 'when',
      header: cached ? 'Cached' : 'Published',
      className: 'meta',
      cell: (v) => (
        <time dateTime={v.published_at} title={dateTime(v.published_at)}>
          {relativeTime(v.published_at, now)}
        </time>
      ),
    },
    { key: 'size', header: 'Size', align: 'right', className: 'muted nowrap', cell: (v) => (v.size_bytes !== undefined ? formatBytes(v.size_bytes) : '') },
    { key: 'dl', header: 'Downloads', align: 'right', className: 'muted', cell: (v) => formatCompact(v.download_count ?? 0) },
    {
      key: 'sec',
      header: 'Security',
      cell: (v) => {
        const p = securityPill(v.scan);
        return <Pill tone={p.tone}>{p.label}</Pill>;
      },
    },
    {
      key: 'del',
      header: <span className="visually-hidden">Delete</span>,
      align: 'right',
      className: 'col-last',
      cell: (v) => (
        <button
          type="button"
          className="icon-button icon-button--sm"
          aria-label={`Delete version ${v.version || v.name}`}
          disabled={!canDelete}
          title={canDelete ? undefined : 'Needs delete rights on this repository'}
          onClick={() => onDelete(v)}
        >
          <IconTrash size={16} />
        </button>
      ),
    },
  ];
  if (q.isError) return <ErrorNote what="versions" error={q.error} onRetry={() => void q.refetch()} />;
  return (
    <div className="pkg-table">
      <DataTable caption="Versions" columns={columns} rows={rows} rowKey={(v) => v.id} busy={q.isFetching} minWidth={640} empty={q.isPending ? 'Loading…' : 'No versions.'} />
      <LoadMore q={q} />
    </div>
  );
}

function FilesTable({ c, now }: { c: ComponentDetail; now: Date }) {
  const q = useComponentAssets(c.id);
  const rows = flatItems(q.data);
  const layers = c.format === 'docker';
  const biggest = Math.max(1, ...rows.map((a) => a.size_bytes));
  const columns: Column<Asset>[] = [
    {
      key: 'path',
      header: layers ? 'Layer or manifest' : 'File',
      className: 'col-first',
      cell: (a) => (
        <div className="asset-path">
          {a.download_url ? (
            <a href={downloadHref(a.download_url)} className="asset-path__link" download>
              <IconDownload size={14} className="inline-icon" />
              <span>{a.path}</span>
              <span className="visually-hidden"> (download)</span>
            </a>
          ) : (
            <span className="facts__mono">{a.path}</span>
          )}
          <span className="meta">{a.content_type}</span>
        </div>
      ),
    },
    { key: 'kind', header: 'Kind', cell: (a) => <Pill tone={a.kind === 'content' ? 'info' : 'neutral'}>{a.kind === 'content' ? 'Content' : 'Metadata'}</Pill> },
    {
      key: 'size',
      header: 'Size',
      align: 'right',
      className: 'muted nowrap',
      cell: (a) =>
        layers ? (
          <span className="asset-size">
            {formatBytes(a.size_bytes)}
            <span className="asset-size__bar" aria-hidden="true">
              <span style={{ width: `${(a.size_bytes / biggest) * 100}%` }} />
            </span>
          </span>
        ) : (
          formatBytes(a.size_bytes)
        ),
    },
    {
      key: 'sha',
      header: 'SHA-256',
      cell: (a) => {
        const sha = a.checksums?.sha256 ? `sha256:${a.checksums.sha256}` : a.blob_digest;
        return (
          <span className="facts__mono facts__copy">
            <span title={sha}>{shortDigest(sha)}</span>
            <CopyButton text={sha.replace(/^sha256:/, '')} label={`Copy SHA-256 of ${a.path}`} />
          </span>
        );
      },
    },
    {
      key: 'last',
      header: 'Last downloaded',
      align: 'right',
      className: 'col-last meta',
      cell: (a) => (a.last_downloaded_at ? relativeTime(a.last_downloaded_at, now) : 'Never'),
    },
  ];
  if (q.isError) return <ErrorNote what="files" error={q.error} onRetry={() => void q.refetch()} />;
  return (
    <div className="pkg-table">
      <DataTable caption={layers ? 'Layers' : 'Files'} columns={columns} rows={rows} rowKey={(a) => a.id} busy={q.isFetching} minWidth={760} empty={q.isPending ? 'Loading…' : 'No files.'} />
      <LoadMore q={q} />
    </div>
  );
}

/** Which repositories hold this exact version, and which groups serve it. */
function RepositoriesTab({ c, now }: { c: ComponentDetail; now: Date }) {
  const q = usePackageHolders(c.name);
  const all = useAllRepositories();
  if (q.isError) return <ErrorNote what="the repositories" error={q.error} onRetry={() => void q.refetch()} />;
  if (!q.data) return <p className="meta">Loading…</p>;
  const holders = q.data.items.filter((v) => v.format === c.format && (v.namespace ?? '') === (c.namespace ?? '') && v.version === c.version);
  const byName = new Map((all.data ?? []).map((r) => [r.name, r]));
  const names = new Set(holders.map((h) => h.repository));
  const groups = (all.data ?? []).filter((r) => r.type === 'group' && (r.config.group?.members ?? []).some((m) => names.has(m)));
  const columns: Column<PublishedComponent>[] = [
    {
      key: 'repo',
      header: 'Repository',
      className: 'col-first',
      cell: (h) => (
        <Link to="/browse" search={{ repo: h.repository, ns: h.namespace || undefined, name: h.name, version: h.version }} className="link-strong" aria-current={h.id === c.id ? 'page' : undefined}>
          {h.repository}
        </Link>
      ),
    },
    {
      key: 'type',
      header: 'Type',
      cell: (h) => {
        const r = byName.get(h.repository);
        return r ? <Pill tone={TYPE_META[r.type].tone}>{TYPE_META[r.type].label}</Pill> : null;
      },
    },
    {
      key: 'when',
      header: 'Published or cached',
      className: 'meta',
      cell: (h) => <time dateTime={h.published_at}>{relativeTime(h.published_at, now)}</time>,
    },
    { key: 'dl', header: 'Downloads', align: 'right', className: 'col-last muted', cell: (h) => formatCompact(h.download_count ?? 0) },
  ];
  return (
    <>
      <DataTable caption="Repositories with this version" columns={columns} rows={holders} rowKey={(h) => h.id} minWidth={520} empty="Only this repository has it." />
      {groups.length > 0 && (
        <p className="meta">
          Also served through {groups.length === 1 ? 'the group' : 'the groups'}{' '}
          {groups.map((g, i) => (
            <span key={g.name}>
              {i > 0 && ', '}
              <Link to="/browse" search={{ repo: g.name }}>
                {g.name}
              </Link>
            </span>
          ))}
          .
        </p>
      )}
    </>
  );
}

const SEVERITY_TONE = { critical: 'bad', high: 'warn', medium: 'warn', low: 'neutral', unknown: 'neutral' } as const;

function FindingsTable({ items }: { items: Finding[] }) {
  const columns: Column<Finding>[] = [
    {
      key: 'id',
      header: 'Advisory',
      className: 'col-first',
      cell: (f) => (
        <div className="asset-path">
          <span className="link-strong">{f.advisory_id}</span>
          <span className="meta">{f.title && <MarkdownInline source={f.title} />}</span>
        </div>
      ),
    },
    { key: 'sev', header: 'Severity', cell: (f) => <Pill tone={SEVERITY_TONE[f.severity as keyof typeof SEVERITY_TONE] ?? 'neutral'}>{f.severity}</Pill> },
    {
      key: 'pkg',
      header: 'In',
      className: 'muted',
      cell: (f) =>
        f.image ? (
          <div className="asset-path">
            <span>{packageAt(f)}</span>
            <span className="meta">{imageWhere(f)}</span>
          </div>
        ) : (
          packageAt(f)
        ),
    },
    { key: 'fix', header: 'Fixed in', className: 'muted', cell: (f) => f.fixed_in ?? 'No fix yet' },
    { key: 'status', header: 'Status', className: 'col-last muted', cell: (f) => f.status },
  ];
  return <DataTable caption="Vulnerabilities" columns={columns} rows={items} rowKey={(f) => f.id} minWidth={720} empty="No known vulnerabilities." />;
}
