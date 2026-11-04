import { useEffect, useId, useRef, useState } from 'react';
import { getRouteApi, Link } from '@tanstack/react-router';
import { useQueryClient } from '@tanstack/react-query';
import { ApiRequestError } from '../../api/client';
import {
  downloadFixPlanCsv,
  findingKeys,
  useFindingRepositories,
  useFindings,
  useFixPlan,
  useRescan,
  useScanningStatus,
  useStartRescan,
} from '../../api/findings';
import { isMissingEndpoint, useCanSystem } from '../../api/ops';
import type { FixPlan, FindingList, PackagePlan, Rescan, ScanningStatus } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { FormatIcon } from '../../components/FormatIcon';
import { IconSearch } from '../../components/icons';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { Select, type SelectOption } from '../../components/Select';
import { relativeTime } from '../../lib/format';
import { plural } from '../../lib/overview';
import { errorText, hasFilters, PLAN_TABS, planQuery, SEVERITY_META, type VulnSearch } from '../../lib/vulnerabilities';
import { FindingDrawer } from './FindingDrawer';
import { FixPlanTable } from './FixPlanTable';
import { AssignDialog, WaiveDialog } from './PlanDialogs';
import './vulnerabilities.css';

const route = getRouteApi('/app/vulnerabilities');

const HEADER_SEVERITIES = ['critical', 'high', 'medium', 'low'] as const;

/** "0 critical · 27 high · 6 medium · 0 low": open findings of the readable repositories. */
function SeverityPills({ list }: { list?: FindingList }) {
  return (
    <ul className="vhead__pills" aria-label="Open findings by severity" aria-busy={!list || undefined}>
      {HEADER_SEVERITIES.map((s) => {
        const n = list ? list.totals[s] + (s === 'low' ? (list.totals.unknown ?? 0) : 0) : null;
        return (
          <li key={s}>
            <Pill tone={n ? SEVERITY_META[s].tone : 'neutral'} className={n ? 'vhead__pill' : 'vhead__pill is-zero'}>
              {n ?? '–'} {s}
            </Pill>
          </li>
        );
      })}
    </ul>
  );
}

/** "451 scanned · OSV · checked 47 min ago" and the Rescan button with its progress. */
function ScanState({ status, canRescan, canFollow }: { status?: ScanningStatus; canRescan: boolean; canFollow: boolean }) {
  const qc = useQueryClient();
  const rescan = useRescan(canFollow);
  const start = useStartRescan();
  const run = rescan.data?.run ?? (start.data?.run as Rescan | undefined);
  const running = run?.state === 'running';
  // When a run finishes, the findings it changed are read again.
  const wasRunning = useRef(false);
  useEffect(() => {
    if (wasRunning.current && !running) void qc.invalidateQueries({ queryKey: findingKeys.all });
    wasRunning.current = running;
  }, [running, qc]);

  if (!status) return <span className="vhead__meta">Loading scanner…</span>;
  const c = status.coverage;
  const pct = run && run.total ? Math.floor((run.done / run.total) * 100) : 0;
  return (
    <div className="vhead__scan">
      <span className="vhead__meta" role="status">
        {!status.enabled
          ? 'Scanning is turned off on this server'
          : running
            ? `Rescanning · ${run.done.toLocaleString('en-US')} of ${plural(run.total, 'package')}`
            : `${c.scanned.toLocaleString('en-US')} scanned · OSV · ${c.last_scan_at ? `checked ${relativeTime(c.last_scan_at).toLowerCase()}` : 'not checked yet'}`}
        {!running && run?.state === 'stalled' && ` · the last rescan stopped at ${run.done} of ${run.total}`}
      </span>
      {canRescan && status.enabled && (
        <button
          type="button"
          className="button button--secondary button--sm vhead__rescan"
          disabled={running || start.isPending}
          onClick={() => start.mutate()}
        >
          {start.isPending ? 'Queuing…' : running ? `Rescanning ${pct}%` : 'Rescan'}
        </button>
      )}
      {running && (
        <span className="vhead__progress" role="progressbar" aria-label="Rescan progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct}>
          <span style={{ width: `${pct}%` }} />
        </span>
      )}
      {start.isError && (
        <p className="form-error vhead__error" role="alert">
          {errorText(start.error)}
        </p>
      )}
    </div>
  );
}

/** "Upgrading 6 packages fixes 22 of 33 findings that builds actually use." */
function ImpactBanner({ plan, status }: { plan: FixPlan; status?: ScanningStatus }) {
  const s = plan.summary;
  if (s.findings === 0) {
    const scanned = status?.coverage.scanned ?? 0;
    return (
      <section className="vimpact glass" aria-label="Impact">
        <div className="vimpact__text">
          <p className="vimpact__lead">
            {scanned === 0 ? 'No package has been scanned yet.' : `No open vulnerabilities in the ${plural(scanned, 'scanned package')}.`}
          </p>
          <span className="vimpact__sub">
            {scanned === 0
              ? 'Packages are checked against OSV when they arrive and every day after; Rescan checks everything now.'
              : s.waived_packages
                ? `${plural(s.waived_packages, 'package')} ${s.waived_packages === 1 ? 'has' : 'have'} waived findings.`
                : 'New advisories are picked up by the daily rescan.'}
          </span>
        </div>
      </section>
    );
  }
  const unusedPct = Math.round((s.findings_unused / s.findings) * 100);
  const notFixed = s.findings_in_use - s.findings_fixed_in_use;
  return (
    <section className="vimpact glass" aria-label="Impact">
      <div className="vimpact__text">
        <p className="vimpact__lead">
          {s.findings_in_use === 0 ? (
            <>
              None of the <b>{plural(s.findings, 'finding')}</b> is in a package builds used in the last {plan.in_use_days} days.
            </>
          ) : s.packages_to_upgrade === 0 ? (
            <>
              <b>{plural(s.findings_in_use, 'finding')}</b> of {s.findings} {s.findings_in_use === 1 ? 'is' : 'are'} in packages builds use, and no upgrade fixes them yet.
            </>
          ) : (
            <>
              Upgrading <b>{plural(s.packages_to_upgrade, 'package')}</b> fixes{' '}
              <b>
                {s.findings_fixed_in_use} of {s.findings}
              </b>{' '}
              findings that builds actually use.
            </>
          )}
        </p>
        <span className="vimpact__sub">
          {s.findings_unused > 0
            ? `The other ${s.findings_unused} ${s.findings_unused === 1 ? 'is' : 'are'} in packages nobody downloaded in ${plan.in_use_days} days.`
            : `Every finding is in a package downloaded in the last ${plan.in_use_days} days.`}
          {notFixed > 0 && ` ${plural(notFixed, 'finding')} in use ${notFixed === 1 ? 'has' : 'have'} no fixed release yet.`}
        </span>
      </div>
      <div
        className="vimpact__bar"
        role="img"
        aria-label={`${s.findings_in_use} findings in used packages, ${s.findings_unused} in unused packages`}
      >
        <div className="vimpact__track">
          <span className="vimpact__used" style={{ width: `${100 - unusedPct}%` }} />
          <span className="vimpact__unused" style={{ width: `${unusedPct}%` }} />
        </div>
        <div className="vimpact__legend">
          <span>
            <b className="vimpact__used-n">{s.findings_in_use}</b> in use
          </span>
          <span>
            <b>{s.findings_unused}</b> unused
          </span>
        </div>
      </div>
    </section>
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
    <div className="filter-search vplan-search">
      <label htmlFor={id} className="visually-hidden">
        Search packages and advisories
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

type Dialog = { kind: 'waive'; p: PackagePlan } | { kind: 'assign'; p: PackagePlan } | null;

export function VulnerabilitiesPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const tab = search.tab ?? 'in_use';
  const plan = useFixPlan(planQuery(search));
  // Header counts and the waived badge ignore the filters.
  const totals = useFindings({ limit: 1 });
  const status = useScanningStatus();
  const repos = useFindingRepositories();
  const canWrite = useCanSystem('security', 'write');
  const canRead = useCanSystem('security', 'read') || canWrite;
  const [dialog, setDialog] = useState<Dialog>(null);
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<string | null>(null);
  const repoId = useId();

  const setSearch = (patch: Partial<VulnSearch>, replace = false) => void navigate({ search: (s) => ({ ...s, ...patch }), replace });
  const toggle = (key: string) => {
    const open = search.open ?? [];
    const next = open.includes(key) ? open.filter((k) => k !== key) : [...open, key];
    setSearch({ open: next.length ? next : undefined }, true);
  };
  const openFinding = (id: string | undefined) => setSearch({ finding: id }, true);

  if (plan.isError && isMissingEndpoint(plan.error)) {
    return (
      <div className="screen">
        <h1 className="vhead__title">Vulnerabilities</h1>
        <Panel>
          <p className="meta">This server does not have the vulnerability fix plan. Upgrade Hangar to see it here.</p>
        </Panel>
      </div>
    );
  }

  const data = plan.data;
  const items = data?.items ?? [];
  const filtered = hasFilters(search);
  const waivedCount = data?.summary.waived_packages;
  const tabLabel = (id: (typeof PLAN_TABS)[number]['id'], label: string) =>
    id === 'all' && data ? `All ${data.summary.packages} packages` : id === 'waived' && waivedCount ? `Waived · ${waivedCount}` : label;

  const empty = plan.isPending ? (
    'Loading…'
  ) : filtered ? (
    search.mine && !search.q && !search.repo ? (
      'Nothing is assigned to you.'
    ) : (
      <>
        No package matches these filters.{' '}
        <Link to="/vulnerabilities" search={{ tab: search.tab }}>
          Clear filters
        </Link>
      </>
    )
  ) : tab === 'waived' ? (
    'No waived findings.'
  ) : status.data && status.data.coverage.scanned === 0 ? (
    'No package has been scanned yet. The fix plan appears after the first scan.'
  ) : (
    'No open vulnerabilities. Nothing to upgrade.'
  );

  const repoOptions: SelectOption[] = [
    { value: '', label: 'All' },
    ...(repos.data ?? []).map((r) => ({ value: r.name, label: r.name, icon: <FormatIcon format={r.format} size={16} label={null} /> })),
  ];
  if (search.repo && !repoOptions.some((o) => o.value === search.repo)) repoOptions.push({ value: search.repo, label: search.repo });

  const exportCsv = async () => {
    setExporting(true);
    setExportError(null);
    try {
      await downloadFixPlanCsv(planQuery(search));
    } catch (err) {
      setExportError(errorText(err));
    } finally {
      setExporting(false);
    }
  };

  const drawerPlan = search.finding ? items.find((p) => p.items.some((f) => f.id === search.finding)) : undefined;
  const waivable = (p: PackagePlan) =>
    p.items.filter((f) => f.status !== 'waived').map((f) => ({ id: f.id, label: `${f.advisory_id} on ${f.installed}` }));

  return (
    <div className="screen vscreen">
      <header className="vhead">
        <h1 className="vhead__title">Vulnerabilities</h1>
        {totals.isError ? <span className="meta">Counts unavailable</span> : <SeverityPills list={totals.data} />}
        <span className="vhead__spacer" />
        {status.isError ? (
          <span className="vhead__meta">Scanner status unavailable</span>
        ) : (
          <ScanState status={status.data} canRescan={canWrite} canFollow={canRead} />
        )}
      </header>

      {plan.isError ? null : data ? (
        <ImpactBanner plan={data} status={status.data} />
      ) : (
        <section className="vimpact glass" aria-label="Impact" aria-busy="true">
          <div className="vimpact__text">
            <p className="vimpact__lead vimpact__lead--skeleton">Working out the fix plan…</p>
          </div>
        </section>
      )}

      <section className="vplan-panel panel glass panel--flush" aria-labelledby="vplan-title">
        <div className="vplan-panel__head">
          <h2 id="vplan-title" className="vplan-panel__title">
            Fix plan
          </h2>
          <div role="tablist" aria-label="Show" className="vtabs">
            {PLAN_TABS.map((t) => (
              <button
                key={t.id}
                type="button"
                role="tab"
                aria-selected={tab === t.id}
                className={tab === t.id ? 'vtabs__tab is-active' : 'vtabs__tab'}
                onClick={() => setSearch({ tab: t.id === 'in_use' ? undefined : t.id, open: undefined })}
              >
                {tabLabel(t.id, t.label)}
              </button>
            ))}
          </div>
          <button type="button" className="button button--secondary button--sm" onClick={() => void exportCsv()} disabled={exporting || !data}>
            {exporting ? 'Exporting…' : 'Export CSV'}
          </button>
        </div>
        <div className="vplan-panel__filters">
          <SearchFilter value={search.q ?? ''} onChange={(q) => setSearch({ q }, true)} />
          <div className="filter-select">
            <label htmlFor={repoId} className="filter-select__label">
              Repository
            </label>
            <Select id={repoId} variant="inline" value={search.repo ?? ''} options={repoOptions} onChange={(v) => setSearch({ repo: v || undefined })} />
          </div>
          <button type="button" className="button button--secondary button--sm" aria-pressed={!!search.mine} onClick={() => setSearch({ mine: search.mine ? undefined : true })}>
            Assigned to me
          </button>
          {exportError && (
            <p className="form-error" role="alert">
              {exportError}
            </p>
          )}
        </div>
        {plan.isError ? (
          plan.error instanceof ApiRequestError && plan.error.status === 401 ? (
            <p className="meta vplan-panel__note">Sign in again to see the fix plan.</p>
          ) : (
            <ErrorNote what="the fix plan" onRetry={() => void plan.refetch()} />
          )
        ) : (
          <FixPlanTable
            items={items}
            tab={tab}
            windowDays={data?.window_days ?? 7}
            inUseDays={data?.in_use_days ?? 30}
            expanded={search.open ?? []}
            onToggle={toggle}
            onOpenFinding={(id) => openFinding(id)}
            onWaive={(p) => setDialog({ kind: 'waive', p })}
            onAssign={(p) => setDialog({ kind: 'assign', p })}
            canWrite={canWrite}
            busy={plan.isFetching}
            empty={empty}
          />
        )}
        <div className="repo-footer">
          <p className="meta" aria-live="polite">
            {data
              ? `${plural(items.length, 'package')}${filtered ? ' match' : ''} · pulls of the last ${data.window_days} days${
                  data.pulls_since ? `, recorded since ${new Date(`${data.pulls_since}T00:00:00Z`).toLocaleDateString('en-GB', { day: 'numeric', month: 'short', timeZone: 'UTC' })}` : ''
                } · in use = downloaded in the last ${data.in_use_days} days`
              : ' '}
          </p>
        </div>
      </section>

      {search.finding && (
        <FindingDrawer id={search.finding} plan={drawerPlan} onClose={() => openFinding(undefined)} canWrite={canWrite} canSeePullers={canRead} />
      )}
      {dialog?.kind === 'waive' && (
        <WaiveDialog packageKey={dialog.p.key} packageName={dialog.p.package} findings={waivable(dialog.p)} onClose={() => setDialog(null)} />
      )}
      {dialog?.kind === 'assign' && (
        <AssignDialog
          target={{ kind: 'package', key: dialog.p.key, label: dialog.p.package }}
          current={dialog.p.assignee ?? null}
          onClose={() => setDialog(null)}
        />
      )}
    </div>
  );
}
