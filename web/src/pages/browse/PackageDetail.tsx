import { useId, useRef, useState, type KeyboardEvent } from 'react';
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import {
  flatItems,
  useComponent,
  useComponentAssets,
  useComponentDependents,
  useComponentFindings,
  useComponentVersions,
  useDeleteComponent,
  useRepository,
} from '../../api/catalog';
import { usePermissions } from '../../api/permissions';
import type { Asset, ComponentDetail, Finding, PublishedComponent } from '../../api/types';
import { CodeBlock } from '../../components/CodeBlock';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { CopyButton } from '../../components/CopyButton';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { FormatBadge } from '../../components/FormatBadge';
import { IconTrash } from '../../components/icons';
import { IconDownload } from '../../components/opsIcons';
import { NotAvailable } from '../../components/NotAvailable';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { Segmented } from '../../components/Segmented';
import { formatBytes, formatCompact, relativeTime } from '../../lib/format';
import { componentName } from '../../lib/overview';
import { downloadHref, hasVersion, installOptions, securityPill, type BrowseTab } from '../../lib/packages';
import { useNow } from '../../lib/useNow';

const route = getRouteApi('/app/browse');

const shortDigest = (d: string) => {
  const [algo, hex] = d.includes(':') ? d.split(':', 2) : ['', d];
  const h = hex ?? '';
  return `${algo ? `${algo}:` : ''}${h.length > 16 ? `${h.slice(0, 8)}…${h.slice(-4)}` : h}`;
};

const dateTime = (iso: string) =>
  new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: 'UTC' }).format(
    new Date(iso),
  ) + ' UTC';

function apiMessage(err: unknown): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return 'You do not have permission to delete versions in this repository.';
    return err.body?.message ?? 'The server could not delete this version.';
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

interface PackageDetailProps {
  id: string;
  /** Versions of this package found by the lookup, newest first. */
  versions: PublishedComponent[];
}

export function PackageDetail({ id, versions }: PackageDetailProps) {
  const search = route.useSearch();
  const navigate = useNavigate();
  const detail = useComponent(id);
  const c = detail.data && detail.data.id === id ? detail.data : undefined;
  const repo = useRepository(c?.repository);
  const dependents = useComponentDependents(c?.id);
  const findings = useComponentFindings(c?.id);
  const perms = usePermissions();
  const remove = useDeleteComponent();
  const [toDelete, setToDelete] = useState<PublishedComponent | null>(null);
  const now = useNow();
  const versionSelectId = useId();

  if (detail.isError) {
    return (
      <Panel>
        <ErrorNote what="the package" onRetry={() => void detail.refetch()} />
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
  const base = { repo: c.repository, ns: c.namespace || undefined, name: c.name };
  const hasDependents = !!dependents.data;
  const hasFindings = !!findings.data;
  const available: BrowseTab[] = ['overview', 'versions', 'files', ...(hasDependents ? (['dependents'] as const) : []), ...(hasFindings ? (['security'] as const) : [])];
  const tab: BrowseTab = search.tab && available.includes(search.tab) ? search.tab : 'overview';
  const setTab = (t: BrowseTab) =>
    void navigate({ to: '/browse', search: (s) => ({ ...s, tab: t === 'overview' ? undefined : t }), replace: true });

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
        const remaining = versions.filter((v) => v.id !== deleted.id);
        // Stay on the package (its newest remaining version) unless the deleted one was the last.
        if (deleted.id !== c.id) return;
        void navigate({
          to: '/browse',
          search: remaining.length ? { ...base, tab: search.tab } : { repo: c.repository },
          replace: true,
        });
      },
    });
  };

  const description = typeof c.attributes?.description === 'string' ? c.attributes.description : '';
  const metaBits = [
    description,
    c.platforms?.length ? c.platforms.join(', ') : '',
    `published by ${c.publisher.display_name}`,
  ].filter(Boolean);

  return (
    <>
      <Panel className="pkg-head" aria-label={`${label} ${c.version}`}>
        <div className="pkg-head__row">
          <FormatBadge format={c.format} size={48} />
          <div className="pkg-head__text">
            <div className="pkg-head__titles">
              <h1 className="pkg-head__title">{label}</h1>
              {hasVersion(c.version) && <Pill tone="info">{c.version}</Pill>}
              {c.signed && <Pill tone="ok">Signed</Pill>}
              {c.quarantined && <Pill tone="bad">Quarantined</Pill>}
              <Pill tone={pill.tone}>{pill.label}</Pill>
            </div>
            <p className="pkg-head__meta">
              {metaBits.join(' · ')} · <time dateTime={c.published_at} title={dateTime(c.published_at)}>{relativeTime(c.published_at, now).toLowerCase()}</time>
            </p>
            {c.quarantined && c.quarantine_reason && <p className="form-error">Quarantined: {c.quarantine_reason}</p>}
          </div>
          <div className="pkg-head__actions">
            {versions.length > 1 && (
              <div className="field pkg-head__version">
                <label htmlFor={versionSelectId} className="visually-hidden">
                  Version
                </label>
                <select
                  id={versionSelectId}
                  className="input"
                  value={c.version}
                  onChange={(e) => void navigate({ to: '/browse', search: { ...base, version: e.target.value, tab: search.tab } })}
                >
                  {versions.map((v, i) => (
                    <option key={v.id} value={v.version}>
                      {v.version}
                      {i === 0 ? ' (newest)' : ''}
                    </option>
                  ))}
                </select>
              </div>
            )}
            <button
              type="button"
              className="button button--danger-outline"
              disabled={!canDelete}
              title={canDelete ? undefined : 'Needs delete rights on this repository'}
              onClick={() => askDelete(c)}
            >
              Delete
            </button>
          </div>
        </div>

        <InstallSnippet c={c} repoUrl={repo.data?.url} />

        <Tabs tabs={available} active={tab} onChange={setTab} counts={{ versions: versions.length }} format={c.format} />

        <div role="tabpanel" id={`pkg-panel-${tab}`} aria-labelledby={`pkg-tab-${tab}`} className="pkg-panel">
          {tab === 'overview' && <Overview c={c} now={now} />}
          {tab === 'versions' && <VersionsTable c={c} canDelete={canDelete} onDelete={askDelete} now={now} />}
          {tab === 'files' && <FilesTable c={c} now={now} />}
          {tab === 'dependents' && dependents.data && <DependentsTable items={dependents.data.items} />}
          {tab === 'security' && findings.data && <FindingsTable items={findings.data.items} />}
        </div>
      </Panel>

      {toDelete && (
        <ConfirmDialog
          title={`Delete ${label}${hasVersion(toDelete.version) ? ` ${toDelete.version}` : ''}?`}
          confirmLabel="Delete version"
          tone="danger"
          busy={remove.isPending}
          error={remove.isError ? apiMessage(remove.error) : null}
          onCancel={() => setToDelete(null)}
          onConfirm={confirmDelete}
        >
          <p>
            Clients will no longer be able to download it from <strong>{c.repository}</strong>. Its files are released for compaction. This cannot be
            undone.
          </p>
        </ConfirmDialog>
      )}
    </>
  );
}

const TAB_LABEL: Record<BrowseTab, string> = {
  overview: 'Overview',
  versions: 'Versions',
  files: 'Files',
  dependents: 'Dependents',
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

function InstallSnippet({ c, repoUrl }: { c: ComponentDetail; repoUrl: string | undefined }) {
  const options = installOptions(c, repoUrl);
  const [choice, setChoice] = useState(options[0]!.id);
  const current = options.find((o) => o.id === choice) ?? options[0]!;
  return (
    <div className="install">
      {options.length > 1 && (
        <Segmented
          label="Client"
          options={options.map((o) => ({ value: o.id, label: o.label }))}
          value={current.id}
          onChange={setChoice}
          className="install__clients"
        />
      )}
      <CodeBlock label={`${current.label} install command`} lines={current.lines} />
    </div>
  );
}

function Fact({ label, children, wide }: { label: string; children: React.ReactNode; wide?: boolean }) {
  return (
    <div className={wide ? 'facts__wide' : undefined}>
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

function Overview({ c, now }: { c: ComponentDetail; now: Date }) {
  const checksums = Object.entries(c.checksums ?? {}).sort(([a], [b]) => a.localeCompare(b));
  const scan = c.scan;
  return (
    <>
      <dl className="facts">
        {c.digest && (
          <Fact label="Digest">
            <span className="facts__mono facts__copy">
              <span title={c.digest}>{shortDigest(c.digest)}</span>
              <CopyButton text={c.digest} label="Copy digest" />
            </span>
          </Fact>
        )}
        <Fact label="Size">{formatBytes(c.size_bytes)}</Fact>
        <Fact label="Downloads">
          {formatCompact(c.download_count ?? 0)}
          {c.downloads_30d !== undefined && <span className="meta"> · {formatCompact(c.downloads_30d)} in 30 days</span>}
        </Fact>
        <Fact label="Last downloaded">
          {c.last_downloaded_at ? (
            <time dateTime={c.last_downloaded_at} title={dateTime(c.last_downloaded_at)}>
              {relativeTime(c.last_downloaded_at, now)}
              {c.last_downloaded_by ? ` · ${c.last_downloaded_by}` : ''}
            </time>
          ) : (
            'Never'
          )}
        </Fact>
        <Fact label="Published">
          <time dateTime={c.published_at}>{dateTime(c.published_at)}</time>
          <span className="meta"> · {c.publisher.display_name}</span>
        </Fact>
        {c.license && <Fact label="Licence">{c.license}</Fact>}
        {c.base_image && <Fact label="Base image">{c.base_image}</Fact>}
        {c.source_url && (
          <Fact label="Source">
            {/^https?:\/\//.test(c.source_url) ? (
              <a href={c.source_url} target="_blank" rel="noreferrer noopener">
                {c.source_url.replace(/^https?:\/\//, '')}
              </a>
            ) : (
              c.source_url
            )}
          </Fact>
        )}
        {c.platforms && c.platforms.length > 0 && <Fact label="Platforms">{c.platforms.join(', ')}</Fact>}
        {(scan.state === 'findings' || scan.state === 'quarantined') && (
          <Fact label="Security">
            {scan.critical} critical · {scan.high} high · {scan.medium} medium · {scan.low} low
          </Fact>
        )}
      </dl>
      {checksums.length > 0 && (
        <div className="checksums">
          <h3 className="checksums__title">Checksums</h3>
          <dl className="checksums__list">
            {checksums.map(([algo, value]) => (
              <div key={algo} className="checksums__row">
                <dt>{algo}</dt>
                <dd className="facts__mono">
                  <span className="checksums__value">{value}</span>
                  <CopyButton text={value} label={`Copy ${algo}`} />
                </dd>
              </div>
            ))}
          </dl>
        </div>
      )}
    </>
  );
}

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

function VersionsTable({
  c,
  canDelete,
  onDelete,
  now,
}: {
  c: ComponentDetail;
  canDelete: boolean;
  onDelete: (v: PublishedComponent) => void;
  now: Date;
}) {
  const q = useComponentVersions(c.id);
  const rows = flatItems(q.data);
  const base = { repo: c.repository, ns: c.namespace || undefined, name: c.name };
  const columns: Column<PublishedComponent>[] = [
    {
      key: 'version',
      header: 'Version',
      className: 'col-first',
      cell: (v) => (
        <Link to="/browse" search={{ ...base, version: v.version }} className="link-strong" aria-current={v.id === c.id ? 'page' : undefined}>
          {hasVersion(v.version) ? v.version : <NotAvailable reason="no version" />}
        </Link>
      ),
    },
    { key: 'when', header: 'Published', className: 'meta', cell: (v) => <time dateTime={v.published_at} title={dateTime(v.published_at)}>{relativeTime(v.published_at, now)}</time> },
    { key: 'by', header: 'Published by', className: 'muted', cell: (v) => v.publisher.display_name },
    { key: 'dl', header: 'Downloads', align: 'right', className: 'muted', cell: (v) => formatCompact(v.download_count ?? 0) },
    {
      key: 'last',
      header: 'Last downloaded',
      className: 'meta',
      cell: (v) => (v.last_downloaded_at ? relativeTime(v.last_downloaded_at, now) : 'Never'),
    },
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
  if (q.isError) return <ErrorNote what="versions" onRetry={() => void q.refetch()} />;
  return (
    <div className="pkg-table">
      <DataTable caption="Versions" columns={columns} rows={rows} rowKey={(v) => v.id} busy={q.isFetching} minWidth={760} empty={q.isPending ? 'Loading…' : 'No versions.'} />
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
  if (q.isError) return <ErrorNote what="files" onRetry={() => void q.refetch()} />;
  return (
    <div className="pkg-table">
      <DataTable caption={layers ? 'Layers' : 'Files'} columns={columns} rows={rows} rowKey={(a) => a.id} busy={q.isFetching} minWidth={760} empty={q.isPending ? 'Loading…' : 'No files.'} />
      <LoadMore q={q} />
    </div>
  );
}

function DependentsTable({ items }: { items: PublishedComponent[] }) {
  const columns: Column<PublishedComponent>[] = [
    {
      key: 'pkg',
      header: 'Package',
      className: 'col-first',
      cell: (d) => (
        <Link to="/browse" search={{ repo: d.repository, ns: d.namespace || undefined, name: d.name, version: d.version }} className="link-strong">
          {componentName(d)} {d.version}
        </Link>
      ),
    },
    { key: 'repo', header: 'Repository', className: 'muted col-last', cell: (d) => d.repository },
  ];
  return <DataTable caption="Dependents" columns={columns} rows={items} rowKey={(d) => d.id} empty="Nothing in this registry depends on this version." />;
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
          <span className="meta">{f.title}</span>
        </div>
      ),
    },
    { key: 'sev', header: 'Severity', cell: (f) => <Pill tone={SEVERITY_TONE[f.severity as keyof typeof SEVERITY_TONE] ?? 'neutral'}>{f.severity}</Pill> },
    { key: 'pkg', header: 'In', className: 'muted', cell: (f) => `${f.package}@${f.installed}` },
    { key: 'fix', header: 'Fixed in', className: 'muted', cell: (f) => f.fixed_in ?? 'No fix yet' },
    { key: 'status', header: 'Status', className: 'col-last muted', cell: (f) => f.status },
  ];
  return <DataTable caption="Vulnerabilities" columns={columns} rows={items} rowKey={(f) => f.id} minWidth={720} empty="No known vulnerabilities." />;
}
