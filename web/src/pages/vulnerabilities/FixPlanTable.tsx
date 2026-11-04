import { Fragment, type ReactNode } from 'react';
import type { PackagePlan, PlanFinding } from '../../api/types';
import { Pill } from '../../components/Pill';
import { plural } from '../../lib/overview';
import { advisoryIds, findingsSummary, findingStatus, SEVERITY_META, sortedAliases, upgradeCommands, upgradeTargets } from '../../lib/vulnerabilities';
import { CopyTextButton } from './CopyTextButton';

interface FixPlanTableProps {
  items: PackagePlan[];
  tab: 'in_use' | 'all' | 'waived';
  windowDays: number;
  inUseDays: number;
  expanded: readonly string[];
  onToggle: (key: string) => void;
  onOpenFinding: (id: string) => void;
  onWaive: (p: PackagePlan) => void;
  onAssign: (p: PackagePlan) => void;
  canWrite: boolean;
  busy: boolean;
  empty: ReactNode;
}

const COLUMNS = 7;

function Chevron() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false">
      <path d="M9 6l6 6-6 6" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

/** One row per package, expandable into its findings. */
export function FixPlanTable({ items, tab, windowDays, inUseDays, expanded, onToggle, onOpenFinding, onWaive, onAssign, canWrite, busy, empty }: FixPlanTableProps) {
  return (
    <div className="table-scroll" tabIndex={0} role="region" aria-label="Fix plan">
      <table className="data-table vplan" aria-busy={busy || undefined}>
        <caption className="visually-hidden">Fix plan by package</caption>
        <thead>
          <tr>
            <th scope="col" className="col-first">
              Package
            </th>
            <th scope="col">Vulnerable versions</th>
            <th scope="col">Findings</th>
            <th scope="col">{tab === 'waived' ? 'Fix' : 'Upgrade to'}</th>
            <th scope="col">Pulled · {windowDays} d</th>
            <th scope="col">Assignee</th>
            <th scope="col" className="col-last is-right">
              Action
            </th>
          </tr>
        </thead>
        <tbody>
          {items.length === 0 ? (
            <tr>
              <td colSpan={COLUMNS} className="data-table__empty">
                {empty}
              </td>
            </tr>
          ) : (
            items.map((p) => {
              const open = expanded.includes(p.key);
              const rowId = `vplan-${p.key.replace(/[^A-Za-z0-9_-]/g, '_')}`;
              const target = upgradeTargets(p);
              const commands = upgradeCommands(p);
              const sev = SEVERITY_META[p.max_severity];
              return (
                <Fragment key={p.key}>
                  <tr className={['vplan__row', p.in_use || tab === 'waived' ? '' : 'is-unused', open ? 'is-open' : ''].join(' ')} onClick={() => onToggle(p.key)}>
                    <td className="col-first">
                      <div className="vplan__pkg">
                        <button
                          type="button"
                          className="vplan__toggle"
                          aria-expanded={open}
                          aria-controls={rowId}
                          aria-label={`${open ? 'Hide' : 'Show'} the ${plural(p.findings, 'finding')} of ${p.package}`}
                          onClick={(e) => {
                            e.stopPropagation();
                            onToggle(p.key);
                          }}
                        >
                          <Chevron />
                        </button>
                        <span className="vplan__names">
                          <span className="vplan__name">{p.package}</span>
                          <span className="vplan__ids">{advisoryIds(p)}</span>
                        </span>
                      </div>
                    </td>
                    <td>
                      <span className="vplan__versions">{p.versions.join(', ')}</span>
                    </td>
                    <td>
                      <Pill tone={sev.tone} className="vplan__sev">
                        {findingsSummary(p)}
                      </Pill>
                    </td>
                    <td>
                      {target ? (
                        <span className="vplan__upgrade">→ {target}</span>
                      ) : (
                        <span className="muted">No fix yet</span>
                      )}
                      {target && p.fixable < p.findings && tab !== 'waived' && (
                        <span className="vplan__partial">{p.findings - p.fixable} without a fix</span>
                      )}
                    </td>
                    <td className="vplan__pulls">
                      <PulledCell p={p} inUseDays={inUseDays} />
                    </td>
                    <td className="vplan__assignee" onClick={(e) => canWrite && e.stopPropagation()}>
                      {canWrite ? (
                        <button
                          type="button"
                          className={p.assignee ? 'vplan__owner vplan__owner--btn' : 'vplan__assign'}
                          onClick={() => onAssign(p)}
                          aria-label={p.assignee ? `Reassign ${p.package} (assigned to ${p.assignee})` : `Assign ${p.package}`}
                        >
                          {p.assignee ?? (p.assignee_mixed ? 'Several' : 'Assign')}
                        </button>
                      ) : p.assignee ? (
                        <span className="vplan__owner">{p.assignee}</span>
                      ) : p.assignee_mixed ? (
                        <span className="muted">Several</span>
                      ) : (
                        <span className="muted">—</span>
                      )}
                    </td>
                    <td className="col-last is-right" onClick={(e) => e.stopPropagation()}>
                      <div className="vplan__actions">
                        {tab !== 'waived' && commands && (
                          <CopyTextButton
                            text={commands}
                            label="Copy upgrade"
                            className="button button--sm vplan__copy"
                            ariaLabel={`Copy the upgrade command for ${p.package}`}
                          />
                        )}
                        {canWrite && tab !== 'waived' && (
                          <button type="button" className="button button--secondary button--sm" onClick={() => onWaive(p)} aria-label={`Waive ${p.package}`}>
                            Waive
                          </button>
                        )}
                      </div>
                    </td>
                  </tr>
                  {open && (
                    <tr className="vplan__detail" id={rowId}>
                      <td colSpan={COLUMNS}>
                        <ul className="vplan__findings" aria-label={`Findings of ${p.package}`}>
                          {p.items.map((f) => (
                            <li key={f.id}>
                              <FindingRow f={f} pkg={p.package} onOpen={() => onOpenFinding(f.id)} />
                            </li>
                          ))}
                        </ul>
                        {p.upgrades.length > 1 && (
                          <p className="meta vplan__lines">
                            {p.upgrades
                              .map((u) => `${u.from.join(', ')} → ${u.to ?? 'no fix yet'}`)
                              .join(' · ')}
                          </p>
                        )}
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })
          )}
        </tbody>
      </table>
    </div>
  );
}

const dayFmt = new Intl.DateTimeFormat('en-GB', { day: 'numeric', month: 'short', timeZone: 'UTC' });
const shortDay = (iso: string) => dayFmt.format(new Date(iso));

function PulledCell({ p, inUseDays }: { p: PackagePlan; inUseDays: number }) {
  if (p.pulls > 0) {
    return (
      <>
        <b>{p.pulls.toLocaleString('en-US')}</b> by {plural(p.clients, 'client')}
      </>
    );
  }
  if (p.in_use) {
    return (
      <>
        <b>0</b> <span className="muted">· last {p.last_pulled_at ? shortDay(p.last_pulled_at) : `in ${inUseDays} d`}</span>
      </>
    );
  }
  return (
    <>
      <b>0</b> <span className="muted" title={`Not downloaded in the last ${inUseDays} days`}>not used</span>
    </>
  );
}

function FindingRow({ f, pkg, onOpen }: { f: PlanFinding; pkg: string; onOpen: () => void }) {
  const sev = SEVERITY_META[f.severity];
  const cve = sortedAliases(f.aliases).find((a) => a.startsWith('CVE-'));
  const status = findingStatus({ status: f.status, waiver_until: f.waiver_until, quarantined: false });
  return (
    <button type="button" className="vfrow" onClick={onOpen} aria-haspopup="dialog">
      <span className="vfrow__main">
        <span className="vfrow__title">{f.title || f.advisory_id}</span>
        <span className="vfrow__meta">
          <span className="vfrow__id">{f.advisory_id}</span>
          {cve && cve !== f.advisory_id && <span className="vfrow__id">{cve}</span>}
          <span className="vfrow__pkg">
            {pkg}@{f.installed}
          </span>
          <span className="muted">{f.repository}</span>
          {f.fixed_in && <span className="vfrow__fix">fixed in {f.fixed_in}</span>}
          {f.assignee && <span className="muted">· {f.assignee}</span>}
          {f.status !== 'open' && <span className="muted">· {status.label}</span>}
        </span>
      </span>
      <Pill tone={sev.tone} className="vfrow__sev">
        {sev.label}
        {f.cvss != null ? ` ${f.cvss.toFixed(1)}` : ''}
      </Pill>
    </button>
  );
}
