import { useState, type ReactNode } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { isMissingEndpoint, useCanSystem } from '../../api/ops';
import { ErrorNote } from '../../components/ErrorNote';
import { IconRefresh } from '../../components/opsIcons';
import { Pill } from '../../components/Pill';
import { formatUptime } from '../../lib/system';
import { formatUtc } from '../../lib/tasks';
import { useNow } from '../../lib/useNow';
import { STATUS_REFRESH_MS, systemKeys, useSystemNodes } from './api';
import { StateMark } from './icons';
import {
  ago,
  checkKey,
  checkMetric,
  HEALTH,
  isTimestamp,
  period,
  problemKeys,
  shortTime,
  summarize,
  versionText,
  type HealthStatus,
  type Subsystem,
  type SystemNode,
} from './model';
import './system.css';

type MarkTone = 'ok' | 'warn' | 'bad' | 'neutral';

export function HealthChip({ status }: { status: HealthStatus }) {
  const h = HEALTH[status];
  return <Pill tone={h.tone}>{h.label}</Pill>;
}

/** The big state line at the top: mark, headline, one line of facts and the refresh controls. */
function Hero({ tone, title, sub, controls }: { tone: MarkTone; title: string; sub?: ReactNode; controls?: ReactNode }) {
  return (
    <section className="status-hero" aria-label="Summary">
      <span className={`status-hero__mark status-hero__mark--${tone}`}>
        <StateMark tone={tone} />
      </span>
      <div className="status-hero__text" aria-live="polite">
        <h1 className="status-hero__title">{title}</h1>
        {sub && <p className="status-hero__sub">{sub}</p>}
      </div>
      {controls}
    </section>
  );
}

interface LiveProps {
  paused: boolean;
  fetching: boolean;
  updatedAt?: number;
  onPause: (paused: boolean) => void;
  onRefresh: () => void;
}

/** "Live · every 15 s" toggles auto-refresh; the round button refreshes now. */
function LiveControls({ paused, fetching, updatedAt, onPause, onRefresh }: LiveProps) {
  const updated = updatedAt ? `Updated ${formatUtc(new Date(updatedAt).toISOString(), { seconds: true })}` : undefined;
  return (
    <div className="live" role="group" aria-label="Refresh status">
      <button
        type="button"
        className={`live__toggle glass${paused ? ' live__toggle--paused' : ''}`}
        onClick={() => onPause(!paused)}
        title={paused ? 'Resume auto-refresh' : 'Pause auto-refresh'}
      >
        <span className="live__dot" aria-hidden="true" />
        {paused ? 'Paused' : `Live · every ${STATUS_REFRESH_MS / 1000} s`}{' '}
        <span className="visually-hidden">{paused ? '– resume' : '– pause'}</span>
      </button>
      <button
        type="button"
        className={`live__refresh glass${fetching ? ' live__refresh--busy' : ''}`}
        onClick={onRefresh}
        disabled={fetching}
        aria-label="Refresh now"
        title={updated}
      >
        <IconRefresh size={16} />
      </button>
    </div>
  );
}

/** Where to go from a subsystem, when the user may open that page. */
function CheckAction({ s, node, canLogs, canSettings }: { s: Subsystem; node: SystemNode; canLogs: boolean; canSettings: boolean }) {
  const arrow = <span aria-hidden="true"> →</span>;
  switch (s.name) {
    case 'blobstores':
      return (
        <Link to="/blob-stores" className="check__action">
          Blob stores{arrow}
        </Link>
      );
    case 'scheduler':
      return (
        <Link to="/tasks" className="check__action">
          Scheduled tasks{arrow}
        </Link>
      );
    case 'upstreams':
      return (
        <Link to="/repositories" className="check__action">
          Repositories{arrow}
        </Link>
      );
    case 'auth':
      return canSettings ? (
        <Link to="/settings/authentication" className="check__action">
          Authentication{arrow}
        </Link>
      ) : null;
    default:
      return canLogs ? (
        <Link to="/system/logs" search={{ node: node.id }} className="check__action">
          Logs <span className="visually-hidden">of {node.id}</span>
          {arrow}
        </Link>
      ) : null;
  }
}

function FactValue({ value, now }: { value: string; now: Date }) {
  if (isTimestamp(value)) {
    return (
      <time dateTime={value} title={formatUtc(value, { seconds: true })}>
        {shortTime(value, now)}
      </time>
    );
  }
  return <>{value}</>;
}

/** Values too long for the large stat figures. */
const isLong = (v: string) => v.length > 18 && !isTimestamp(v);

interface RowProps {
  s: Subsystem;
  node: SystemNode;
  open: boolean;
  onToggle: () => void;
  now: Date;
  canLogs: boolean;
  canSettings: boolean;
}

function CheckRow({ s, node, open, onToggle, now, canLogs, canSettings }: RowProps) {
  const h = HEALTH[s.status];
  const metric = checkMetric(s);
  const detailId = `check-${node.id.replace(/[^\w-]/g, '_')}-${s.name}`;
  // A long value the summary already says ("PostgreSQL 17.2") is not repeated.
  const facts = (s.facts ?? []).filter((f) => !(isLong(f.value) && s.detail.includes(f.value)));
  return (
    <li className={`check check--${s.status}${open ? ' check--open' : ''}`}>
      <button type="button" className="check__row" aria-expanded={open} aria-controls={open ? detailId : undefined} onClick={onToggle}>
        <span className={`check__dot dot-${h.tone}`} aria-hidden="true" />
        <span className="check__name">
          {s.label} <span className="visually-hidden">({h.label.toLowerCase()})</span>
        </span>
        <span className="check__summary">{s.detail}</span>
        <span className="check__end">
          {s.status !== 'healthy' && <Pill tone={h.tone}>{h.label}</Pill>}
          {metric && <span className="check__metric">{metric}</span>}
        </span>
        <span className="check__caret" aria-hidden="true">
          {open ? '▴' : '▾'}
        </span>
      </button>
      {open && (
        <div className="check__detail" id={detailId}>
          {facts.length > 0 && (
            <dl className="check__stats">
              {facts.map((f) => (
                <div key={f.label} className={isLong(f.value) ? 'check__stat--long' : undefined}>
                  <dt>{f.label}</dt>
                  <dd>
                    <FactValue value={f.value} now={now} />
                  </dd>
                </div>
              ))}
            </dl>
          )}
          <div className="check__foot">
            {s.target && <code className="check__target">{s.target}</code>}
            <span>
              {s.since && (
                <>
                  {h.label} since{' '}
                  <time dateTime={s.since} title={formatUtc(s.since, { seconds: true })}>
                    {shortTime(s.since, now)}
                  </time>
                  {' · '}
                </>
              )}
              {s.since ? 'checked ' : 'Checked '}
              <time dateTime={s.checked_at} title={formatUtc(s.checked_at, { seconds: true })}>
                {ago(s.checked_at, now)}
              </time>
            </span>
            {s.last_incident && (
              <span className="check__incident" title={s.last_incident.detail}>
                Was {HEALTH[s.last_incident.status].label.toLowerCase()}{' '}
                {period(s.last_incident.started_at, s.last_incident.ended_at, now)}
              </span>
            )}
            <span className="check__spacer" />
            <CheckAction s={s} node={node} canLogs={canLogs} canSettings={canSettings} />
          </div>
        </div>
      )}
    </li>
  );
}

interface NodeProps {
  node: SystemNode;
  /** More than one node: each gets a heading. */
  multi: boolean;
  now: Date;
  isOpen: (key: string) => boolean;
  toggle: (key: string) => void;
  canLogs: boolean;
  canSettings: boolean;
}

function NodeChecks({ node, multi, now, isOpen, toggle, canLogs, canSettings }: NodeProps) {
  const titleId = `node-${node.id.replace(/[^\w-]/g, '_')}`;
  const leader = node.role === 'leader';
  const meta: ReactNode[] = [];
  if (node.hostname && node.hostname !== node.id) meta.push(`Host ${node.hostname}`);
  if (!multi) meta.push(leader ? 'Scheduler leader' : 'Scheduler follower');
  if (multi) meta.push(versionText(node.version));
  meta.push(<span title={`Started ${formatUtc(node.started_at)}`}>Up {formatUptime(node.uptime_seconds)}</span>);
  meta.push(
    <span>
      Heartbeat{' '}
      <time dateTime={node.last_heartbeat} title={formatUtc(node.last_heartbeat, { seconds: true })}>
        {ago(node.last_heartbeat, now)}
      </time>
    </span>,
  );
  if (node.url) meta.push(<code className="node__url">{node.url}</code>);
  if (node.commit) meta.push(`Commit ${node.commit.slice(0, 12)}`);

  return (
    <section className={`node${node.stale ? ' node--stale' : ''}`} aria-labelledby={multi ? titleId : undefined} aria-label={multi ? undefined : node.id}>
      {multi && (
        <div className="node__head">
          <h2 className="node__title" id={titleId}>
            {node.id}
          </h2>
          {node.current && <Pill tone="neutral">This node</Pill>}
          <span className="node__badges">
            <Pill tone={leader ? 'info' : 'neutral'}>{leader ? 'Scheduler leader' : 'Follower'}</Pill>
            {node.stale ? <Pill tone="bad">Not responding</Pill> : <HealthChip status={node.status} />}
          </span>
        </div>
      )}
      {node.stale && (
        <p className="node__stale" role="note">
          No heartbeat for {ago(node.last_heartbeat, now).replace(' ago', '')}: the node is stopped, hung or cannot reach the database.
          {node.subsystems.length > 0 && ' The checks below are its last report.'}
        </p>
      )}
      {node.subsystems.length > 0 ? (
        <ul className="checklist glass" aria-label={`Checks of ${node.id}`}>
          {node.subsystems.map((s) => {
            const key = checkKey(node, s);
            return (
              <CheckRow
                key={s.name}
                s={s}
                node={node}
                open={isOpen(key)}
                onToggle={() => toggle(key)}
                now={now}
                canLogs={canLogs}
                canSettings={canSettings}
              />
            );
          })}
        </ul>
      ) : (
        <div className="checklist checklist--empty glass">
          <p className="meta">This node has not reported any checks yet.</p>
        </div>
      )}
      <p className="node__meta">
        {meta.map((m, i) => (
          <span key={i} className="node__meta-item">
            {m}
          </span>
        ))}
      </p>
    </section>
  );
}

/** Seven quiet rows while the first answer is on its way. */
function ChecklistSkeleton() {
  return (
    <ul className="checklist glass" aria-hidden="true">
      {[72, 64, 80, 58, 70, 52, 66].map((w, i) => (
        <li key={i} className="check">
          <span className="check__row check__row--skeleton">
            <span className="check__dot dot-neutral" />
            <span className="skeleton" style={{ width: 110, height: 14 }} />
            <span className="skeleton" style={{ width: `${w}%`, height: 12 }} />
            <span className="skeleton" style={{ width: 40, height: 12 }} />
            <span />
          </span>
        </li>
      ))}
    </ul>
  );
}

function Notice({ children }: { children: ReactNode }) {
  return (
    <p className="status-note" role="note">
      {children}
    </p>
  );
}

const TONE: Record<HealthStatus, MarkTone> = { healthy: 'ok', degraded: 'warn', down: 'bad' };

export function SystemStatusPage() {
  const canRead = useCanSystem('cluster', 'read');
  const canLogs = useCanSystem('cluster', 'admin');
  const canSettings = useCanSystem('settings', 'read');
  const [paused, setPaused] = useState(false);
  const nodes = useSystemNodes(paused, canRead);
  const qc = useQueryClient();
  const now = useNow(5_000);
  // Until the user opens or closes a row, the rows with a problem are open.
  const [chosen, setChosen] = useState<Set<string> | null>(null);

  const forbidden = !canRead || (nodes.error instanceof ApiRequestError && nodes.error.status === 403);
  const data = nodes.data;

  if (forbidden) {
    return (
      <div className="screen status-page">
        <Hero tone="neutral" title="System status" sub="Every Hangar node and the services it depends on." />
        <div className="checklist checklist--empty glass">
          <p className="meta">
            The system status needs the <code className="inline-code">system:cluster:read</code> privilege. Ask an administrator for it, or see
            the overall state on the <Link to="/settings/system">System</Link> page.
          </p>
        </div>
      </div>
    );
  }

  const live = (
    <LiveControls
      paused={paused}
      fetching={nodes.isFetching}
      updatedAt={nodes.dataUpdatedAt || undefined}
      onPause={setPaused}
      onRefresh={() => void qc.refetchQueries({ queryKey: systemKeys.nodes })}
    />
  );

  if (!data) {
    if (nodes.isError) {
      return (
        <div className="screen status-page">
          <Hero tone="neutral" title="System status" sub="The status could not be read." controls={live} />
          {isMissingEndpoint(nodes.error) ? (
            <div className="checklist checklist--empty glass">
              <p className="meta">This server does not report node status yet. Upgrade Hangar to see it here.</p>
            </div>
          ) : (
            <ErrorNote what="the system status" onRetry={() => void nodes.refetch()} />
          )}
        </div>
      );
    }
    return (
      <div className="screen status-page" aria-busy="true">
        <Hero tone="neutral" title="Checking every node…" sub="Database, storage, event bus, scheduler, search, sign-in and upstreams." controls={live} />
        <ChecklistSkeleton />
      </div>
    );
  }

  const summary = summarize(data.items);
  const multi = data.items.length > 1;
  const only = data.items[0];
  const sub: string[] = [`${summary.passing} of ${summary.checks} checks passing`];
  if (!multi && only) sub.push(only.id, versionText(only.version));
  else if (multi) {
    sub.push(`${summary.up} of ${summary.total} nodes up`);
    if (summary.versions.length === 1) sub.push(versionText(summary.versions[0]!));
    else if (summary.versions.length > 1) sub.push(`mixed versions ${summary.versions.map(versionText).join(', ')}`);
  }

  const open = chosen ?? problemKeys(data.items);
  const toggle = (key: string) => {
    const next = new Set(open);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    setChosen(next);
  };

  return (
    <div className="screen status-page">
      <Hero
        tone={data.items.length === 0 ? 'neutral' : TONE[summary.status]}
        title={summary.headline}
        sub={data.items.length > 0 ? sub.join(' · ') : 'No node has published a heartbeat yet. A node reports within 15 seconds of starting.'}
        controls={live}
      />
      {nodes.isError && <Notice>The last refresh failed; showing the answer from {ago(new Date(nodes.dataUpdatedAt).toISOString(), now)}.</Notice>}
      {data.partial && <Notice>{data.message ?? 'Only this node is shown: the other nodes cannot be read.'}</Notice>}
      {summary.notes.map((n) => (
        <Notice key={n}>{n}</Notice>
      ))}
      {data.items.map((n) => (
        <NodeChecks
          key={n.id}
          node={n}
          multi={multi}
          now={now}
          isOpen={(k) => open.has(k)}
          toggle={toggle}
          canLogs={canLogs}
          canSettings={canSettings}
        />
      ))}
    </div>
  );
}
