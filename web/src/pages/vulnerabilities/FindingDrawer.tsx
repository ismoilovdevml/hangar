import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useFinding, usePullers, useRevokeWaiver } from '../../api/findings';
import type { Finding, PackagePlan } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { Pill } from '../../components/Pill';
import { plural } from '../../lib/overview';
import {
  advisoryLink,
  dockerfileUpgrade,
  errorText,
  findingStatus,
  fixStatusText,
  imageRef,
  imageWhere,
  lineFor,
  packageAt,
  planKeyOf,
  PULL_WINDOW,
  SEVERITY_META,
  shortDate,
  sortedAliases,
} from '../../lib/vulnerabilities';
import { CopyTextButton } from './CopyTextButton';
import { Drawer } from './Drawer';
import { AssignDialog, WaiveDialog } from './PlanDialogs';

interface FindingDrawerProps {
  id: string;
  /** The package plan the finding belongs to, when the fix plan shows it. */
  plan?: PackagePlan;
  onClose: () => void;
  canWrite: boolean;
  /** May see who pulls a package (system:security:read or write). */
  canSeePullers: boolean;
}

/** One finding: how to fix it, who pulls it, the advisory, and Assign / Waive. */
export function FindingDrawer({ id, plan, onClose, canWrite, canSeePullers }: FindingDrawerProps) {
  const finding = useFinding(id);
  const f = finding.data;
  const [dialog, setDialog] = useState<'assign' | 'waive' | null>(null);
  const sev = f ? SEVERITY_META[f.severity] : undefined;
  const status = f ? findingStatus(f) : undefined;
  const ids = f ? [f.advisory_id, ...sortedAliases(f.aliases).filter((a) => a !== f.advisory_id && a.startsWith('CVE-'))] : [];
  const resolved = f?.status === 'fixed';

  return (
    <>
      <Drawer
        title={f ? f.title || f.advisory_id : 'Finding'}
        eyebrow={
          f &&
          sev &&
          status && (
            <>
              <Pill tone={sev.tone}>
                {sev.label}
                {f.cvss != null ? ` · ${f.cvss.toFixed(1)}` : ''}
              </Pill>
              <Pill tone={status.tone}>{status.label}</Pill>
              {f.assignee && <Pill tone="info">Assigned to {f.assignee}</Pill>}
            </>
          )
        }
        subtitle={f && <span className="vdrawer__ids">{ids.join(' · ')}</span>}
        onClose={onClose}
        footer={
          f &&
          !resolved &&
          (canWrite ? (
            <div className="vdrawer__actions">
              <button type="button" className="button button--primary" onClick={() => setDialog('assign')}>
                {f.assignee ? 'Reassign' : 'Assign'}
              </button>
              {f.status !== 'waived' && (
                <button type="button" className="button button--secondary" onClick={() => setDialog('waive')}>
                  Waive with reason…
                </button>
              )}
            </div>
          ) : (
            <p className="meta">Assigning and waiving need the security write privilege.</p>
          ))
        }
      >
        {finding.isError ? (
          finding.error instanceof ApiRequestError && finding.error.status === 404 ? (
            <p className="meta">This finding no longer exists or is in a repository you cannot read.</p>
          ) : (
            <ErrorNote what="the finding" error={finding.error} onRetry={() => void finding.refetch()} />
          )
        ) : !f ? (
          <p className="meta" aria-busy="true">
            Loading…
          </p>
        ) : (
          <FindingBody f={f} plan={plan} canWrite={canWrite} canSeePullers={canSeePullers} />
        )}
      </Drawer>
      {f && dialog === 'assign' && (
        <AssignDialog target={{ kind: 'finding', id: f.id, label: f.advisory_id }} current={f.assignee ?? null} onClose={() => setDialog(null)} />
      )}
      {f && f.component && dialog === 'waive' && (
        <WaiveDialog
          packageKey={planKeyOf(f)}
          packageName={f.image ? planKeyOf(f).slice('docker:'.length) : f.package}
          findings={[{ id: f.id, label: `${f.advisory_id} on ${f.installed}` }]}
          initial={f.id}
          onClose={() => setDialog(null)}
        />
      )}
    </>
  );
}

function FindingBody({ f, plan, canWrite, canSeePullers }: { f: Finding; plan?: PackagePlan; canWrite: boolean; canSeePullers: boolean }) {
  const c = f.component;
  const line = plan && !f.image ? lineFor(plan, f.installed) : undefined;
  const advisory = advisoryLink(f);
  return (
    <div className="vfinding">
      {f.status !== 'fixed' &&
        (f.image ? (
          <ImageHowToFix f={f} />
        ) : (
          <HowToFix f={f} target={line?.to ?? f.fixed_in} command={line?.command ?? null} lineFixes={line?.fixes ?? 0} />
        ))}

      <section className="vfinding__section vfinding__section--first" aria-label={`Who pulls it, last ${PULL_WINDOW.replace('d', ' days')}`}>
        <h3 className="vfinding__heading">Who pulls it · last {PULL_WINDOW.replace('d', ' days')}</h3>
        {canSeePullers ? <Pullers id={f.id} /> : <p className="meta">Seeing who pulls a package needs the security read privilege.</p>}
      </section>

      <dl className="facts vfinding__facts">
        {f.image ? (
          <div className="facts__wide">
            <dt>Found in</dt>
            <dd className="facts__mono">{imageWhere(f)}</dd>
          </div>
        ) : (
          <div>
            <dt>Affected</dt>
            <dd className="facts__mono">{f.affected_versions || <span className="muted">Not in the advisory</span>}</dd>
          </div>
        )}
        <div>
          <dt>Published</dt>
          <dd>{f.advisory_published_at ? <time dateTime={f.advisory_published_at}>{shortDate(f.advisory_published_at)}</time> : '—'}</dd>
        </div>
        {f.image && c && (
          <div>
            <dt>Image</dt>
            <dd>
              <Link to="/browse" search={{ repo: c.repository, ns: c.namespace || undefined, name: c.name, version: c.version || undefined }}>
                {imageRef(c)}
              </Link>
            </dd>
          </div>
        )}
        <div>
          <dt>Package</dt>
          <dd>
            {f.image ? (
              packageAt(f)
            ) : c ? (
              <Link to="/browse" search={{ repo: c.repository, ns: c.namespace || undefined, name: c.name, version: c.version || undefined }}>
                {packageAt(f)}
              </Link>
            ) : (
              packageAt(f)
            )}
          </dd>
        </div>
        {c && (
          <div>
            <dt>Repository</dt>
            <dd>{c.repository}</dd>
          </div>
        )}
        <div>
          <dt>First seen</dt>
          <dd>{f.first_seen_at ? <time dateTime={f.first_seen_at}>{shortDate(f.first_seen_at)}</time> : '—'}</dd>
        </div>
        <div>
          <dt>Assignee</dt>
          <dd>
            {f.assignee ?? <span className="muted">Nobody</span>}
            {f.assignee && f.assigned_by && <span className="muted"> · by {f.assigned_by}</span>}
          </dd>
        </div>
        {f.cvss_vector && (
          <div className="facts__wide">
            <dt>Vector</dt>
            <dd className="facts__mono">{f.cvss_vector}</dd>
          </div>
        )}
      </dl>

      <section className="vfinding__section" aria-label="Summary">
        <h3 className="vfinding__heading">Summary</h3>
        <p className="vfinding__details">{f.details || f.title || 'The advisory has no description.'}</p>
        <a className="vfinding__osv" href={advisory.href} target="_blank" rel="noreferrer noopener">
          {advisory.label}
        </a>
      </section>

      {(f.status === 'waived' || (f.waiver_until && f.waiver_reason)) && <WaiverSection f={f} canWrite={canWrite} />}
    </div>
  );
}

function HowToFix({ f, target, command, lineFixes }: { f: Finding; target: string | null | undefined; command: string | null; lineFixes: number }) {
  if (!target) {
    return (
      <div className="vfix vfix--none" role="note" aria-label="How to fix">
        <span className="vfix__title">No fix yet</span>
        <span>
          No release of <b>{f.package}</b> fixes this advisory yet. Waive it with a reason if the vulnerable code is not reachable, or replace the
          package.
        </span>
      </div>
    );
  }
  return (
    <div className="vfix" role="note" aria-label="How to fix">
      <span className="vfix__title">How to fix</span>
      <span>
        Upgrade <b>{f.package}</b> to <b>{target}</b> or later.
        {lineFixes > 1 && ` That also fixes the other ${plural(lineFixes - 1, 'finding')} on ${f.installed}.`}
      </span>
      {command && (
        <div className="vfix__cmd">
          <code title={command}>{command}</code>
          <CopyTextButton text={command} label="Copy" copiedLabel="Copied" className="vfix__copy" ariaLabel="Copy the upgrade command" />
        </div>
      )}
    </div>
  );
}

/** How to fix a finding inside an image: rebuild it with the fixed package. */
function ImageHowToFix({ f }: { f: Finding }) {
  const img = f.image!;
  const image = f.component ? imageRef(f.component) : 'the image';
  if (!f.fixed_in) {
    return (
      <div className="vfix vfix--none" role="note" aria-label="How to fix">
        <span className="vfix__title">No fix yet</span>
        <span>
          {img.class === 'os-pkgs' ? (
            <>
              {img.os || 'The distribution'} has no fixed <b>{f.package}</b> yet. {fixStatusText(img.fix_status)} A newer base image may not
              include it; otherwise waive it with a reason.
            </>
          ) : (
            <>
              No release of <b>{f.package}</b> fixes this advisory yet. Waive it with a reason if the vulnerable code is not reachable, or
              replace the library.
            </>
          )}
        </span>
      </div>
    );
  }
  const command = img.class === 'os-pkgs' ? dockerfileUpgrade(img.os ?? '', [f.package]) : '';
  return (
    <div className="vfix" role="note" aria-label="How to fix">
      <span className="vfix__title">How to fix</span>
      {img.class === 'os-pkgs' ? (
        <span>
          Rebuild <b>{image}</b> on a base image that ships <b>{f.package}</b> {f.fixed_in} or later, or upgrade it in the Dockerfile.
        </span>
      ) : (
        <span>
          Upgrade <b>{f.package}</b> to <b>{f.fixed_in}</b> or later in the application{f.location ? ` (${f.location})` : ''}, then rebuild{' '}
          <b>{image}</b>.
        </span>
      )}
      {command && (
        <div className="vfix__cmd">
          <code title={command}>{command}</code>
          <CopyTextButton text={command} label="Copy" copiedLabel="Copied" className="vfix__copy" ariaLabel="Copy the Dockerfile line" />
        </div>
      )}
    </div>
  );
}

function Pullers({ id }: { id: string }) {
  const pullers = usePullers(id, PULL_WINDOW, true);
  if (pullers.isError) {
    if (pullers.error instanceof ApiRequestError && pullers.error.status === 403) {
      return <p className="meta">Seeing who pulls a package needs the security read privilege.</p>;
    }
    return <ErrorNote what="who pulls it" error={pullers.error} onRetry={() => void pullers.refetch()} />;
  }
  const p = pullers.data;
  if (!p) {
    return (
      <p className="meta" aria-busy="true">
        Loading…
      </p>
    );
  }
  if (p.items.length === 0) {
    return (
      <p className="meta">
        Not pulled in the last {p.window_days} days.
        {p.pulls_since ? ` Pulls are recorded since ${shortDate(p.pulls_since)}.` : ' No pull has been recorded yet.'}
      </p>
    );
  }
  return (
    <>
      <ul className="vpullers">
        {p.items.map((u) => (
          <li key={`${u.kind}:${u.label}`}>
            <span className="vpullers__who">
              {u.label}
              {u.kind === 'service_account' && <span className="muted"> · service account</span>}
              {u.kind === 'user' && u.address && <span className="muted"> · {u.address}</span>}
            </span>
            <span className="vpullers__n">{plural(u.pulls, 'pull')}</span>
          </li>
        ))}
      </ul>
      {p.clients > p.items.length && <p className="meta">and {plural(p.clients - p.items.length, 'more client')}</p>}
    </>
  );
}

function WaiverSection({ f, canWrite }: { f: Finding; canWrite: boolean }) {
  const revoke = useRevokeWaiver();
  const active = f.status === 'waived';
  return (
    <section className="vfinding__section" aria-label="Waiver">
      <h3 className="vfinding__heading">{active ? 'Waiver' : 'Earlier waiver'}</h3>
      <dl className="facts facts--compact">
        <div className="facts__wide">
          <dt>Reason</dt>
          <dd className="vfinding__reason">{f.waiver_reason}</dd>
        </div>
        <div>
          <dt>{active ? 'Until' : 'Ended'}</dt>
          <dd>{f.waiver_until ? shortDate(f.waiver_until) : '—'}</dd>
        </div>
        {f.waived_by && (
          <div>
            <dt>By</dt>
            <dd>
              {f.waived_by}
              {f.waived_at ? `, ${shortDate(f.waived_at)}` : ''}
            </dd>
          </div>
        )}
      </dl>
      {active && (
        <>
          <p className="meta">The finding re-opens on its own when the waiver ends.</p>
          {canWrite && (
            <div className="vfinding__actions">
              <button type="button" className="button button--secondary button--sm" disabled={revoke.isPending} onClick={() => revoke.mutate(f.id)}>
                {revoke.isPending ? 'Revoking…' : 'Revoke waiver'}
              </button>
            </div>
          )}
          {revoke.isError && (
            <p className="form-error" role="alert">
              {errorText(revoke.error)}
            </p>
          )}
        </>
      )}
    </section>
  );
}
