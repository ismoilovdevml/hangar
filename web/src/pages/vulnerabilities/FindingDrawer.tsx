import { useId, useState, type FormEvent } from 'react';
import { Link } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useFinding, useRevokeWaiver, useScanComponent, useWaiveFinding } from '../../api/findings';
import type { Finding } from '../../api/types';
import { ErrorNote } from '../../components/ErrorNote';
import { Pill } from '../../components/Pill';
import { relativeTime } from '../../lib/format';
import { componentName } from '../../lib/overview';
import {
  advisoryUrl,
  dateInputValue,
  findingStatus,
  MAX_WAIVER_DAYS,
  packageAt,
  SEVERITY_META,
  shortDate,
  sortedAliases,
  validateWaiver,
  waiverUntil,
} from '../../lib/vulnerabilities';
import { Drawer } from './Drawer';

interface FindingDrawerProps {
  id: string;
  onClose: () => void;
  canWaive: boolean;
  canScan: (repository: string) => boolean;
}

const errorText = (err: unknown) => (err instanceof ApiRequestError ? err.message : 'Something went wrong. Try again.');

function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/** The details of one finding, its references, waiver and a rescan. */
export function FindingDrawer({ id, onClose, canWaive, canScan }: FindingDrawerProps) {
  const finding = useFinding(id);
  const f = finding.data;
  return (
    <Drawer
      title={f ? f.advisory_id : 'Finding'}
      subtitle={f ? <span className="vdrawer__summary">{f.title || 'No summary in the advisory.'}</span> : undefined}
      onClose={onClose}
    >
      {finding.isError ? (
        finding.error instanceof ApiRequestError && finding.error.status === 404 ? (
          <p className="meta">This finding no longer exists or is in a repository you cannot read.</p>
        ) : (
          <ErrorNote what="the finding" onRetry={() => void finding.refetch()} />
        )
      ) : !f ? (
        <p className="meta" aria-busy="true">
          Loading…
        </p>
      ) : (
        <FindingBody f={f} canWaive={canWaive} canScan={canScan(f.component?.repository ?? '')} />
      )}
    </Drawer>
  );
}

function FindingBody({ f, canWaive, canScan }: { f: Finding; canWaive: boolean; canScan: boolean }) {
  const sev = SEVERITY_META[f.severity];
  const status = findingStatus(f);
  const c = f.component;
  const aliases = sortedAliases(f.aliases);
  return (
    <div className="vfinding">
      <div className="vfinding__pills">
        <Pill tone={sev.tone}>{sev.label}</Pill>
        <Pill tone={status.tone}>{status.label}</Pill>
        {f.quarantined && f.status !== 'open' && <Pill tone="bad">Quarantined</Pill>}
      </div>

      <dl className="facts vfinding__facts">
        <div>
          <dt>Package</dt>
          <dd>
            {c ? (
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
          <dt>Fixed in</dt>
          <dd>{f.fixed_in ?? <span className="muted">No fix yet</span>}</dd>
        </div>
        <div>
          <dt>CVSS</dt>
          <dd>{f.cvss != null ? `${f.cvss.toFixed(1)} · ${sev.label}` : <span className="muted">No score in the advisory</span>}</dd>
        </div>
        {f.affected_versions && (
          <div className="facts__wide">
            <dt>Affected versions</dt>
            <dd className="facts__mono">{f.affected_versions}</dd>
          </div>
        )}
        {f.cvss_vector && (
          <div className="facts__wide">
            <dt>Vector</dt>
            <dd className="facts__mono">{f.cvss_vector}</dd>
          </div>
        )}
        {aliases.length > 0 && (
          <div className="facts__wide">
            <dt>Also known as</dt>
            <dd className="vfinding__aliases">
              {aliases.map((a) => (
                <a key={a} href={advisoryUrl(a)} target="_blank" rel="noreferrer noopener">
                  {a}
                </a>
              ))}
            </dd>
          </div>
        )}
        <div>
          <dt>First seen</dt>
          <dd>{f.first_seen_at ? <time dateTime={f.first_seen_at}>{shortDate(f.first_seen_at)}</time> : '—'}</dd>
        </div>
        <div>
          <dt>{f.status === 'fixed' ? 'Fixed' : 'Last seen'}</dt>
          <dd>
            {f.status === 'fixed' && f.resolved_at ? (
              <time dateTime={f.resolved_at}>{shortDate(f.resolved_at)}</time>
            ) : f.last_seen_at ? (
              <time dateTime={f.last_seen_at}>{relativeTime(f.last_seen_at)}</time>
            ) : (
              '—'
            )}
          </dd>
        </div>
        <div>
          <dt>Affected packages</dt>
          <dd>{f.affected_components}</dd>
        </div>
        {f.advisory_published_at && (
          <div>
            <dt>Advisory published</dt>
            <dd>
              <time dateTime={f.advisory_published_at}>{shortDate(f.advisory_published_at)}</time>
            </dd>
          </div>
        )}
      </dl>

      {f.details && (
        <section className="vfinding__section" aria-label="Advisory">
          <h3 className="vfinding__heading">Advisory</h3>
          <p className="vfinding__details">{f.details}</p>
        </section>
      )}

      <section className="vfinding__section" aria-label="References">
        <h3 className="vfinding__heading">References</h3>
        <ul className="vfinding__refs">
          <li>
            <a href={advisoryUrl(f.advisory_id)} target="_blank" rel="noreferrer noopener">
              {f.advisory_id} on osv.dev
            </a>
          </li>
          {(f.references ?? []).map((r) => (
            <li key={r.url}>
              <span className="vfinding__ref-type">{r.type.toLowerCase()}</span>
              <a href={r.url} target="_blank" rel="noreferrer noopener" title={r.url}>
                {hostOf(r.url)}
                <span className="vfinding__ref-path">{r.url.replace(/^https?:\/\/[^/]+/, '')}</span>
              </a>
            </li>
          ))}
        </ul>
      </section>

      <WaiverSection f={f} canWaive={canWaive} />
      {c && <RescanSection componentId={c.id} label={componentName(c)} canScan={canScan} />}
    </div>
  );
}

function WaiverSection({ f, canWaive }: { f: Finding; canWaive: boolean }) {
  const revoke = useRevokeWaiver();
  if (f.status === 'fixed') {
    return (
      <section className="vfinding__section" aria-label="Waiver">
        <h3 className="vfinding__heading">Waiver</h3>
        <p className="meta">A rescan no longer reports this advisory for this version; nothing to waive.</p>
      </section>
    );
  }
  if (f.status === 'waived') {
    return (
      <section className="vfinding__section" aria-label="Waiver">
        <h3 className="vfinding__heading">Waiver</h3>
        <dl className="facts facts--compact">
          <div className="facts__wide">
            <dt>Reason</dt>
            <dd className="vfinding__reason">{f.waiver_reason}</dd>
          </div>
          <div>
            <dt>Until</dt>
            <dd>{f.waiver_until ? shortDate(f.waiver_until) : '—'}</dd>
          </div>
          <div>
            <dt>By</dt>
            <dd>
              {f.waived_by ?? '—'}
              {f.waived_at ? `, ${shortDate(f.waived_at)}` : ''}
            </dd>
          </div>
        </dl>
        <p className="meta">The finding re-opens on its own when the waiver ends.</p>
        {canWaive ? (
          <div className="vfinding__actions">
            <button type="button" className="button button--secondary button--sm" disabled={revoke.isPending} onClick={() => revoke.mutate(f.id)}>
              {revoke.isPending ? 'Revoking…' : 'Revoke waiver'}
            </button>
          </div>
        ) : (
          <p className="meta">Changing waivers needs the security write privilege.</p>
        )}
        {revoke.isError && (
          <p className="form-error" role="alert">
            {errorText(revoke.error)}
          </p>
        )}
      </section>
    );
  }
  return (
    <section className="vfinding__section" aria-label="Waiver">
      <h3 className="vfinding__heading">Accept the risk</h3>
      {f.waiver_until && f.waiver_reason && (
        <p className="meta">
          An earlier waiver ended on {shortDate(f.waiver_until)}: “{f.waiver_reason}”
        </p>
      )}
      {canWaive ? (
        <WaiverForm id={f.id} />
      ) : (
        <p className="meta">Waivers need the security write privilege. Ask a security administrator.</p>
      )}
    </section>
  );
}

function WaiverForm({ id }: { id: string }) {
  const waive = useWaiveFinding();
  const reasonId = useId();
  const untilId = useId();
  const [reason, setReason] = useState('');
  const today = new Date();
  const [until, setUntil] = useState(() => dateInputValue(new Date(Date.now() + 30 * 86_400_000)));
  const [touched, setTouched] = useState(false);
  const errors = validateWaiver(reason, until);
  const invalid = !!(errors.reason || errors.until);

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setTouched(true);
    const end = waiverUntil(until);
    if (invalid || !end) return;
    waive.mutate({ id, input: { reason: reason.trim(), until: end.toISOString() } });
  };

  return (
    <form className="vwaiver" onSubmit={submit} noValidate>
      <div className="field">
        <label htmlFor={reasonId} className="field__label">
          Reason
        </label>
        <textarea
          id={reasonId}
          className="textarea"
          rows={3}
          value={reason}
          maxLength={2000}
          onChange={(e) => setReason(e.target.value)}
          aria-invalid={touched && errors.reason ? true : undefined}
          aria-describedby={`${reasonId}-msg`}
          placeholder="Why this does not affect us, or what mitigates it"
        />
        <span id={`${reasonId}-msg`} className={touched && errors.reason ? 'field__error' : 'field__hint'}>
          {touched && errors.reason ? errors.reason : 'Recorded in the audit log with your name.'}
        </span>
      </div>
      <div className="field">
        <label htmlFor={untilId} className="field__label">
          Waived until
        </label>
        <input
          id={untilId}
          className="input"
          type="date"
          value={until}
          min={dateInputValue(today)}
          max={dateInputValue(new Date(today.getTime() + MAX_WAIVER_DAYS * 86_400_000))}
          onChange={(e) => setUntil(e.target.value)}
          aria-invalid={touched && errors.until ? true : undefined}
          aria-describedby={`${untilId}-msg`}
        />
        <span id={`${untilId}-msg`} className={touched && errors.until ? 'field__error' : 'field__hint'}>
          {touched && errors.until ? errors.until : 'Ends at the end of that day (UTC), at most a year ahead.'}
        </span>
      </div>
      {waive.isError && (
        <p className="form-error" role="alert">
          {errorText(waive.error)}
        </p>
      )}
      <div className="vfinding__actions">
        <button type="submit" className="button button--primary" disabled={waive.isPending}>
          {waive.isPending ? 'Saving…' : 'Waive finding'}
        </button>
      </div>
    </form>
  );
}

function RescanSection({ componentId, label, canScan }: { componentId: string; label: string; canScan: boolean }) {
  const scan = useScanComponent();
  if (!canScan) return null;
  const r = scan.data;
  return (
    <section className="vfinding__section" aria-label="Rescan">
      <h3 className="vfinding__heading">Rescan</h3>
      <p className="meta">Check {label} against the advisory database now.</p>
      <div className="vfinding__actions">
        <button type="button" className="button button--secondary button--sm" disabled={scan.isPending} onClick={() => scan.mutate(componentId)}>
          {scan.isPending ? 'Scanning…' : 'Scan now'}
        </button>
      </div>
      <p className="meta" role="status">
        {r
          ? r.scan.status === 'ok'
            ? `Scanned: ${r.summary.findings} known ${r.summary.findings === 1 ? 'vulnerability' : 'vulnerabilities'}, ${r.summary.new_findings} new.`
            : r.scan.status === 'failed'
              ? `The scan failed: ${r.scan.error ?? 'unknown error'}. Earlier findings are kept.`
              : (r.scan.error ?? 'This package is not scanned.')
          : ''}
      </p>
      {scan.isError && (
        <p className="form-error" role="alert">
          {errorText(scan.error)}
        </p>
      )}
    </section>
  );
}
