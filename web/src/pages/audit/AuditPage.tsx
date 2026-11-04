import { Fragment, useId, useMemo, useState, type FormEvent } from 'react';
import { getRouteApi, Link, useRouterState } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useAuditEvents } from '../../api/ops';
import type { AuditEvent } from '../../api/opsTypes';
import { ErrorNote } from '../../components/ErrorNote';
import { IconChevronRight, IconDownload } from '../../components/opsIcons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import {
  ACTION_SUGGESTIONS,
  AUDIT_OUTCOMES,
  AUDIT_RANGES,
  auditExportUrl,
  auditQuery,
  DEFAULT_AUDIT_RANGE,
  flattenDetails,
  hasFilters,
  isAuditOutcome,
  isAuditRange,
  isoToUtcInput,
  outcomeMeta,
  utcInputToIso,
  type AuditSearch,
} from '../../lib/audit';
import { relativeTime } from '../../lib/format';
import { formatUtc } from '../../lib/tasks';

const route = getRouteApi('/app/audit');

/** Text filters are edited locally and applied together, so a half-typed actor does not query. */
interface TextDraft {
  actor: string;
  action: string;
  target: string;
  from: string;
  to: string;
}

const toDraft = (s: AuditSearch): TextDraft => ({
  actor: s.actor ?? '',
  action: s.action ?? '',
  target: s.target ?? '',
  from: isoToUtcInput(s.from),
  to: isoToUtcInput(s.to),
});

function EventRow({ e, now }: { e: AuditEvent; now: Date }) {
  const [open, setOpen] = useState(false);
  const detailsId = useId();
  const outcome = outcomeMeta(e.outcome);
  const details = flattenDetails(e.details as Record<string, unknown> | undefined);
  return (
    <>
      <tr className={open ? 'is-open' : undefined}>
        <td className="col-first audit-toggle-cell">
          <button
            type="button"
            className="icon-button icon-button--sm audit-toggle"
            aria-expanded={open}
            aria-controls={detailsId}
            aria-label={`${open ? 'Hide' : 'Show'} details of event ${e.id}`}
            onClick={() => setOpen((v) => !v)}
          >
            <IconChevronRight size={16} />
          </button>
        </td>
        <td className="nowrap">
          <time dateTime={e.at} title={formatUtc(e.at, { seconds: true })}>
            {formatUtc(e.at)}
          </time>
          <span className="audit-sub">{relativeTime(e.at, now)}</span>
        </td>
        <td className="audit-actor">{e.actor}</td>
        <td>
          <code className="audit-action">{e.action}</code>
        </td>
        <td className="audit-target">{e.target}</td>
        <td>
          <Pill tone={outcome.tone}>{outcome.label}</Pill>
        </td>
        <td className="muted nowrap col-last">{e.ip ?? '—'}</td>
      </tr>
      <tr id={detailsId} className="audit-details" hidden={!open}>
        <td />
        <td colSpan={6} className="col-last">
          {open && (
            <dl className="facts facts--compact">
              <div>
                <dt>Event id</dt>
                <dd>
                  <code>{e.id}</code>
                </dd>
              </div>
              <div>
                <dt>Time</dt>
                <dd>{formatUtc(e.at, { seconds: true })}</dd>
              </div>
              {details.length === 0 && (
                <div>
                  <dt>Details</dt>
                  <dd>None recorded</dd>
                </div>
              )}
              {details.map(([k, v]) => (
                <div key={k}>
                  <dt>{k}</dt>
                  <dd>
                    <code>{v}</code>
                  </dd>
                </div>
              ))}
            </dl>
          )}
        </td>
      </tr>
    </>
  );
}

export function AuditPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const pageCursors = useRouterState({ select: (s) => s.location.state.pageCursors });
  // The window ("last 7 days") is resolved once per set of filters, so paging stays inside it.
  const filterKey = JSON.stringify({ ...search, cursor: undefined });
  const [win, setWin] = useState(() => ({ key: filterKey, now: new Date() }));
  if (win.key !== filterKey) setWin({ key: filterKey, now: new Date() });
  const now = win.now;
  const query = useMemo(() => auditQuery(search, now), [search, now]);
  const events = useAuditEvents(query);
  const ids = { actor: useId(), action: useId(), target: useId(), outcome: useId(), range: useId(), from: useId(), to: useId(), list: useId() };

  const [draft, setDraft] = useState<TextDraft>(() => toDraft(search));
  const [synced, setSynced] = useState(filterKey);
  if (synced !== filterKey) {
    // The URL changed (applied filters, back button, shared link): adopt it.
    setSynced(filterKey);
    setDraft(toDraft(search));
  }

  const range = search.range ?? DEFAULT_AUDIT_RANGE;
  const setFilter = (patch: Partial<AuditSearch>) => void navigate({ search: (s) => ({ ...s, ...patch, cursor: undefined }) });

  const apply = (e: FormEvent) => {
    e.preventDefault();
    setFilter({
      actor: draft.actor.trim() || undefined,
      action: draft.action.trim() || undefined,
      target: draft.target.trim() || undefined,
      ...(range === 'custom' ? { from: utcInputToIso(draft.from), to: utcInputToIso(draft.to) } : {}),
    });
  };

  const stack = search.cursor ? (pageCursors ?? []) : [];
  const goNext = () => {
    const next = events.data?.next_cursor;
    if (!next) return;
    void navigate({ search: (s) => ({ ...s, cursor: next }), state: { pageCursors: [...stack, search.cursor ?? ''] } });
  };
  const goPrev = () => {
    const prev = stack.length ? stack[stack.length - 1] : '';
    void navigate({ search: (s) => ({ ...s, cursor: prev || undefined }), state: { pageCursors: stack.slice(0, -1) } });
  };

  // Older servers may ignore the outcome filter; never show rows that do not match it.
  const rows = (events.data?.items ?? []).filter((e) => !search.outcome || e.outcome === search.outcome);
  const forbidden = events.error instanceof ApiRequestError && events.error.status === 403;
  const customInvalid = range === 'custom' && !!search.from && !!search.to && search.from >= search.to;

  return (
    <div className="screen">
      <PageHeader
        title="Audit log"
        description="Every sign-in, change and denied request, newest first. Times are in UTC."
        actions={
          !forbidden && (
            <>
              <a className="button button--secondary" href={auditExportUrl(search, 'csv', now)} download>
                <IconDownload size={16} /> Export CSV
              </a>
              <a className="button button--secondary" href={auditExportUrl(search, 'json', now)} download>
                <IconDownload size={16} /> Export NDJSON
              </a>
            </>
          )
        }
      />

      <Panel aria-label="Audit log filters" className="audit-filters">
        <form className="audit-filters__form" onSubmit={apply} role="search" aria-label="Filter audit events">
          <div className="field">
            <label htmlFor={ids.actor}>Actor</label>
            <input
              id={ids.actor}
              className="input"
              value={draft.actor}
              onChange={(e) => setDraft({ ...draft, actor: e.target.value })}
              placeholder="username or service account"
              autoComplete="off"
              spellCheck={false}
            />
          </div>
          <div className="field">
            <label htmlFor={ids.action}>Action</label>
            <input
              id={ids.action}
              className="input input--mono"
              list={ids.list}
              value={draft.action}
              onChange={(e) => setDraft({ ...draft, action: e.target.value })}
              placeholder="repository.*"
              autoComplete="off"
              spellCheck={false}
              aria-describedby={`${ids.action}-hint`}
            />
            <datalist id={ids.list}>
              {ACTION_SUGGESTIONS.map((a) => (
                <option key={a} value={a} />
              ))}
            </datalist>
            <span className="field__hint" id={`${ids.action}-hint`}>
              Exact, or a prefix ending in *
            </span>
          </div>
          <div className="field">
            <label htmlFor={ids.target}>Target</label>
            <input
              id={ids.target}
              className="input input--mono"
              value={draft.target}
              onChange={(e) => setDraft({ ...draft, target: e.target.value })}
              placeholder="repo:npm-*"
              autoComplete="off"
              spellCheck={false}
              aria-describedby={`${ids.target}-hint`}
            />
            <span className="field__hint" id={`${ids.target}-hint`}>
              Exact, or a prefix ending in *
            </span>
          </div>
          <div className="field">
            <label htmlFor={ids.outcome}>Outcome</label>
            <select
              id={ids.outcome}
              className="input"
              value={search.outcome ?? ''}
              onChange={(e) => setFilter({ outcome: isAuditOutcome(e.target.value) ? e.target.value : undefined })}
            >
              <option value="">Any outcome</option>
              {AUDIT_OUTCOMES.map((o) => (
                <option key={o.id} value={o.id}>
                  {o.label}
                </option>
              ))}
            </select>
          </div>
          <div className="field">
            <label htmlFor={ids.range}>Time range</label>
            <select
              id={ids.range}
              className="input"
              value={range}
              onChange={(e) => {
                const r = isAuditRange(e.target.value) ? e.target.value : undefined;
                setFilter(
                  r === 'custom'
                    ? { range: r, from: search.from ?? new Date(Date.now() - 86_400_000).toISOString(), to: search.to }
                    : { range: r === DEFAULT_AUDIT_RANGE ? undefined : r, from: undefined, to: undefined },
                );
              }}
            >
              {AUDIT_RANGES.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.label}
                </option>
              ))}
            </select>
          </div>
          {range === 'custom' && (
            <>
              <div className="field">
                <label htmlFor={ids.from}>From (UTC)</label>
                <input id={ids.from} className="input" type="datetime-local" value={draft.from} onChange={(e) => setDraft({ ...draft, from: e.target.value })} />
              </div>
              <div className="field">
                <label htmlFor={ids.to}>
                  To (UTC) <span className="field__optional">(optional)</span>
                </label>
                <input id={ids.to} className="input" type="datetime-local" value={draft.to} onChange={(e) => setDraft({ ...draft, to: e.target.value })} />
              </div>
            </>
          )}
          <div className="audit-filters__actions">
            <button type="submit" className="button button--primary">
              Apply filters
            </button>
            <button
              type="button"
              className="button button--secondary"
              onClick={() => {
                setWin({ key: filterKey, now: new Date() });
                if (search.cursor) void navigate({ search: (s) => ({ ...s, cursor: undefined }) });
              }}
            >
              Refresh
            </button>
            {hasFilters(search) && (
              <Link to="/audit" search={{}} className="button button--secondary">
                Clear
              </Link>
            )}
          </div>
        </form>
        {customInvalid && <p className="field__error">The end of the range must be after its start.</p>}
      </Panel>

      <Panel flush aria-label="Audit events" className="audit-panel">
        {forbidden ? (
          <p className="panel-note">
            Reading the audit log needs the <code className="inline-code">system:audit:read</code> privilege. Ask an administrator for it.
          </p>
        ) : events.isError ? (
          <ErrorNote what="audit events" onRetry={() => void events.refetch()} />
        ) : (
          <div className="table-scroll" tabIndex={0} role="region" aria-label="Audit events table">
            <table className="data-table audit-table" style={{ minWidth: 920 }} aria-busy={events.isFetching || undefined}>
              <caption className="visually-hidden">Audit events, newest first</caption>
              <thead>
                <tr>
                  <th scope="col" className="col-first">
                    <span className="visually-hidden">Details</span>
                  </th>
                  <th scope="col">Time</th>
                  <th scope="col">Actor</th>
                  <th scope="col">Action</th>
                  <th scope="col">Target</th>
                  <th scope="col">Outcome</th>
                  <th scope="col" className="col-last">
                    IP
                  </th>
                </tr>
              </thead>
              <tbody>
                {rows.length === 0 ? (
                  <tr>
                    <td colSpan={7} className="data-table__empty">
                      {events.isPending ? (
                        'Loading…'
                      ) : hasFilters(search) ? (
                        <>
                          No events match these filters.{' '}
                          <Link to="/audit" search={{}}>
                            Clear filters
                          </Link>
                        </>
                      ) : (
                        'No events in this time range.'
                      )}
                    </td>
                  </tr>
                ) : (
                  rows.map((e) => (
                    <Fragment key={e.id}>
                      <EventRow e={e} now={now} />
                    </Fragment>
                  ))
                )}
              </tbody>
            </table>
          </div>
        )}
        <div className="repo-footer">
          <p className="meta" aria-live="polite">
            {events.data ? `${rows.length} ${rows.length === 1 ? 'event' : 'events'} on this page` : ' '}
          </p>
          <div className="repo-footer__pager">
            <button type="button" className="button button--secondary button--sm" onClick={goPrev} disabled={!search.cursor}>
              Previous
            </button>
            <button type="button" className="button button--secondary button--sm" onClick={goNext} disabled={!events.data?.next_cursor}>
              Next
            </button>
          </div>
        </div>
      </Panel>
    </div>
  );
}
