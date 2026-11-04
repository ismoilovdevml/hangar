import { useEffect, useId, useRef, useState } from 'react';
import { getRouteApi, Link, useRouterState } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import {
  useFindingRepositories,
  useFindings,
  useFindingTrend,
  useScanningStatus,
  useScanRepository,
} from '../../api/findings';
import { isMissingEndpoint, useCanSystem } from '../../api/ops';
import { usePermissions } from '../../api/permissions';
import type { Finding, FindingList, ScanningStatus, Severity } from '../../api/types';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { FormatBadge } from '../../components/FormatBadge';
import { IconSearch } from '../../components/icons';
import { KpiCard, KpiCardSkeleton } from '../../components/KpiCard';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { relativeTime } from '../../lib/format';
import { plural } from '../../lib/overview';
import {
  findingQuery,
  findingStatus,
  FIX_FILTERS,
  hasFilters,
  packageAt,
  SEVERITIES,
  SEVERITY_META,
  sortedAliases,
  STATUS_FILTERS,
  type VulnSearch,
} from '../../lib/vulnerabilities';
import { FindingDrawer } from './FindingDrawer';
import { Picker } from './Picker';
import { TrendChart } from './TrendChart';
import './vulnerabilities.css';

const route = getRouteApi('/app/vulnerabilities');

const CARD_SEVERITIES = ['critical', 'high', 'medium', 'low'] as const;

function SeverityCards({ list, loading }: { list?: FindingList; loading: boolean }) {
  if (loading || !list) {
    return (
      <div className="kpis" aria-busy="true">
        {CARD_SEVERITIES.map((s) => (
          <KpiCardSkeleton key={s} label={SEVERITY_META[s].label} />
        ))}
      </div>
    );
  }
  const total = SEVERITIES.reduce((n, s) => n + (list.totals[s] ?? 0), 0);
  return (
    <div className="kpis">
      {CARD_SEVERITIES.map((s) => {
        const n = list.totals[s] + (s === 'low' ? (list.totals.unknown ?? 0) : 0);
        const fixable = list.fixable[s] + (s === 'low' ? (list.fixable.unknown ?? 0) : 0);
        const meta = SEVERITY_META[s];
        return (
          <KpiCard
            key={s}
            label={meta.label}
            tag={n > 0 ? { text: 'Open', tone: meta.tone } : { text: 'None open', tone: 'ok' }}
            value={n}
            unit={n === 1 ? 'finding' : 'findings'}
            meter={{ value: total ? n / total : 0, label: `${meta.label} share of open findings`, color: meta.color }}
            note={
              n === 0 ? (
                'Nothing open at this severity.'
              ) : (
                <>
                  {fixable === n ? 'All have a fix' : fixable === 0 ? 'No fix released yet' : `${fixable} of ${n} have a fix`} ·{' '}
                  <Link to="/vulnerabilities" search={(prev) => ({ ...prev, severity: s, status: undefined, cursor: undefined, finding: undefined })}>
                    Show
                  </Link>
                </>
              )
            }
          />
        );
      })}
    </div>
  );
}

function ScannerPanel({ status, repo, canScanRepo }: { status?: ScanningStatus; repo?: string; canScanRepo: boolean }) {
  const scan = useScanRepository();
  if (!status) {
    return (
      <Panel title="Scanner" titleId="vuln-scanner" className="vscanner">
        <p className="meta">Loading…</p>
      </Panel>
    );
  }
  const c = status.coverage;
  const source = (() => {
    try {
      return new URL(status.source_url).host;
    } catch {
      return status.source_url;
    }
  })();
  return (
    <Panel title="Scanner" titleId="vuln-scanner" className="vscanner" action={<Pill tone={status.enabled ? 'ok' : 'neutral'}>{status.enabled ? 'On' : 'Off'}</Pill>}>
      {!status.enabled && <p className="vscanner__off">Scanning is turned off on this server (scanning.enabled). Stored findings are still shown.</p>}
      <dl className="vscanner__facts">
        <div>
          <dt>Scanned</dt>
          <dd>{c.scanned}</dd>
        </div>
        <div>
          <dt>Waiting for a first scan</dt>
          <dd>{c.never}</dd>
        </div>
        <div>
          <dt>Last scan failed</dt>
          <dd className={c.failed ? 'vscanner__bad' : undefined}>{c.failed}</dd>
        </div>
        <div>
          <dt>Not scanned</dt>
          <dd>{c.unsupported}</dd>
        </div>
      </dl>
      <p className="meta">
        Advisories from OSV ({source}). Packages are checked on arrival in repositories with scan on arrival, and every package again
        once its last check is older than {status.rescan_interval.replace(/0m0s$/, '').replace(/h$/, ' h')}.
        {c.last_scan_at ? ` Last check ${relativeTime(c.last_scan_at)}.` : ' No package checked yet.'}
      </p>
      {c.unsupported > 0 && <p className="meta">Docker images are not scanned: image scanning is not configured. Raw files have no advisory source.</p>}
      {status.enabled &&
        (repo ? (
          canScanRepo ? (
            <div className="vscanner__actions">
              <button type="button" className="button button--secondary button--sm" disabled={scan.isPending} onClick={() => scan.mutate(repo)}>
                {scan.isPending ? 'Queuing…' : `Scan ${repo} now`}
              </button>
              <p className="meta" role="status">
                {scan.data ? `Queued a scan of ${plural(scan.data.components, 'package')} in ${scan.data.repository}. Results appear here as it runs.` : ''}
              </p>
              {scan.isError && (
                <p className="form-error" role="alert">
                  {scan.error instanceof ApiRequestError ? scan.error.message : 'Could not queue the scan.'}
                </p>
              )}
            </div>
          ) : (
            <p className="meta">Scanning {repo} on demand needs write access to it.</p>
          )
        ) : (
          <p className="meta">Pick a repository in the filters to scan it now.</p>
        ))}
    </Panel>
  );
}

/** Free-text filter: typed locally, written to the URL after a short pause. */
function SearchFilter({ value, onChange }: { value: string; onChange: (q: string | undefined) => void }) {
  const [draft, setDraft] = useState(value);
  const [synced, setSynced] = useState(value);
  if (value !== synced) {
    setSynced(value);
    setDraft(value);
  }
  const commit = useRef(onChange);
  useEffect(() => {
    commit.current = onChange;
  }, [onChange]);
  useEffect(() => {
    if (draft.trim() === value) return;
    const t = setTimeout(() => commit.current(draft.trim() || undefined), 300);
    return () => clearTimeout(t);
  }, [draft, value]);
  const id = useId();
  return (
    <div className="filter-search">
      <label htmlFor={id} className="visually-hidden">
        Search advisories and packages
      </label>
      <IconSearch size={16} />
      <input
        id={id}
        className="filter-search__input"
        type="search"
        placeholder="CVE, advisory or package"
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  );
}

export function VulnerabilitiesPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const pageCursors = useRouterState({ select: (s) => s.location.state.pageCursors });
  const list = useFindings(findingQuery(search));
  // The cards count open findings of the repository filter only.
  const totals = useFindings({ repository: search.repo, limit: 1 });
  const trend = useFindingTrend(search.repo);
  const status = useScanningStatus();
  const repos = useFindingRepositories();
  const perms = usePermissions();
  const canSecurity = useCanSystem('security', 'write');
  const canScan = (repo: string) => canSecurity || perms.can('write', 'repo', repo);

  const setFilter = (patch: Partial<VulnSearch>) =>
    void navigate({ search: (s) => ({ ...s, ...patch, cursor: undefined }), replace: 'q' in patch });
  const openFinding = (id: string | undefined) => void navigate({ search: (s) => ({ ...s, finding: id }), replace: true });

  const data = list.data;
  const stack = search.cursor ? (pageCursors ?? []) : [];
  const goNext = () => {
    if (!data?.next_cursor) return;
    void navigate({ search: (s) => ({ ...s, cursor: data.next_cursor ?? undefined }), state: { pageCursors: [...stack, search.cursor ?? ''] } });
  };
  const goPrev = () => {
    const prev = stack[stack.length - 1];
    void navigate({ search: (s) => ({ ...s, cursor: prev || undefined }), state: { pageCursors: stack.slice(0, -1) } });
  };

  if (list.isError && isMissingEndpoint(list.error)) {
    return (
      <div className="screen">
        <PageHeader title="Vulnerabilities" />
        <Panel>
          <p className="meta">This server does not scan packages for vulnerabilities. Upgrade Hangar to see findings here.</p>
        </Panel>
      </div>
    );
  }

  const severityOptions = [{ value: '', label: 'All' }, ...SEVERITIES.map((s) => ({ value: s, label: SEVERITY_META[s].label }))];
  const statusOptions = STATUS_FILTERS.map((f) => ({ value: f.id, label: f.label }));
  const fixOptions = FIX_FILTERS.map((f) => ({ value: f.id, label: f.label }));
  const repoOptions = [{ value: '', label: 'All' }, ...(repos.data ?? []).map((r) => ({ value: r.name, label: r.name }))];
  if (search.repo && !repoOptions.some((o) => o.value === search.repo)) repoOptions.push({ value: search.repo, label: search.repo });

  const columns: Column<Finding>[] = [
    {
      key: 'advisory',
      header: 'Advisory',
      className: 'col-first',
      cell: (f) => {
        const cve = sortedAliases(f.aliases).find((a) => a.startsWith('CVE-'));
        return (
          <div className="vrow-advisory">
            <button type="button" className="vrow-advisory__open" onClick={() => openFinding(f.id)} aria-haspopup="dialog">
              {f.advisory_id}
            </button>
            {cve && cve !== f.advisory_id && <span className="vrow-advisory__alias">{cve}</span>}
            <span className="vrow-advisory__title">{f.title}</span>
          </div>
        );
      },
    },
    {
      key: 'severity',
      header: 'Severity',
      cell: (f) => (
        <span className="vrow-severity">
          <Pill tone={SEVERITY_META[f.severity].tone}>{SEVERITY_META[f.severity].label}</Pill>
          {f.cvss != null && <span className="meta">{f.cvss.toFixed(1)}</span>}
        </span>
      ),
    },
    {
      key: 'package',
      header: 'Package',
      cell: (f) => (
        <span className="vrow-package">
          {f.component && <FormatBadge format={f.component.format} size={28} />}
          <span className="vrow-package__name">{packageAt(f)}</span>
        </span>
      ),
    },
    { key: 'repo', header: 'Repository', className: 'muted', cell: (f) => f.component?.repository ?? '—' },
    { key: 'fix', header: 'Fixed in', cell: (f) => f.fixed_in ?? <span className="muted">No fix yet</span> },
    {
      key: 'status',
      header: 'Status',
      className: 'col-last',
      cell: (f) => {
        const st = findingStatus(f);
        return <Pill tone={st.tone}>{st.label}</Pill>;
      },
    },
  ];

  const filtered = hasFilters(search);
  const emptyText = list.isPending ? (
    'Loading…'
  ) : filtered ? (
    <>
      No findings match these filters.{' '}
      <Link to="/vulnerabilities" search={{}}>
        Clear filters
      </Link>
    </>
  ) : status.data && status.data.coverage.scanned === 0 ? (
    'No package has been scanned yet. Findings appear here after the first scan.'
  ) : (
    'No known vulnerabilities in the scanned packages.'
  );

  return (
    <div className="screen">
      <PageHeader
        title="Vulnerabilities"
        description="Known vulnerabilities in npm, Maven, PyPI, NuGet and Go packages, from the OSV advisory database. Waive what does not apply, with a reason and an end date."
      />

      {totals.isError && !isMissingEndpoint(totals.error) ? (
        <ErrorNote what="the severity counts" onRetry={() => void totals.refetch()} />
      ) : (
        <SeverityCards list={totals.data} loading={totals.isPending} />
      )}

      <div className="vgrid">
        <Panel title="Last 30 days" titleId="vuln-trend" className="vtrend-panel">
          {trend.isError ? (
            <ErrorNote what="the trend" onRetry={() => void trend.refetch()} />
          ) : trend.data ? (
            <TrendChart points={trend.data.items} />
          ) : (
            <p className="meta" aria-busy="true">
              Loading…
            </p>
          )}
          <p className="meta vtrend-panel__note">Unresolved findings at the end of each day, by severity. Waived findings are included.</p>
        </Panel>
        {status.isError ? (
          <Panel title="Scanner">
            <ErrorNote what="the scanner status" onRetry={() => void status.refetch()} />
          </Panel>
        ) : (
          <ScannerPanel status={status.data} repo={search.repo} canScanRepo={!!search.repo && canScan(search.repo)} />
        )}
      </div>

      <Panel flush aria-label="Findings" className="vfindings">
        <div className="repo-toolbar">
          <div className="repo-toolbar__filters repo-toolbar__filters--start vfilters">
            <Picker
              label="Severity"
              value={search.severity ?? ''}
              options={severityOptions}
              onChange={(v) => setFilter({ severity: (v || undefined) as Severity | undefined })}
            />
            <Picker
              label="Status"
              value={search.status ?? 'active'}
              options={statusOptions}
              onChange={(v) => setFilter({ status: v === 'active' ? undefined : (v as VulnSearch['status']) })}
            />
            <Picker label="Fix" value={search.fix ?? 'any'} options={fixOptions} onChange={(v) => setFilter({ fix: v === 'any' ? undefined : (v as VulnSearch['fix']) })} />
            <Picker label="Repository" value={search.repo ?? ''} options={repoOptions} onChange={(v) => setFilter({ repo: v || undefined })} />
            <SearchFilter value={search.q ?? ''} onChange={(q) => setFilter({ q })} />
          </div>
        </div>
        {list.isError ? (
          <ErrorNote what="findings" onRetry={() => void list.refetch()} />
        ) : (
          <DataTable caption="Findings" columns={columns} rows={data?.items ?? []} rowKey={(f) => f.id} minWidth={960} busy={list.isFetching} empty={emptyText} />
        )}
        <div className="repo-footer">
          <p className="meta" aria-live="polite">
            {data ? `${plural(data.total, 'finding')}${filtered ? ' match' : ''}${data.waived ? ` · ${data.waived} waived` : ''}` : ' '}
          </p>
          <div className="repo-footer__pager">
            <button type="button" className="button button--secondary button--sm" onClick={goPrev} disabled={!search.cursor}>
              Previous
            </button>
            <button type="button" className="button button--secondary button--sm" onClick={goNext} disabled={!data?.next_cursor}>
              Next
            </button>
          </div>
        </div>
      </Panel>

      {search.finding && <FindingDrawer id={search.finding} onClose={() => openFinding(undefined)} canWaive={canSecurity} canScan={canScan} />}
    </div>
  );
}
