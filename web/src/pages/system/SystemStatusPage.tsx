import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { isMissingEndpoint, useCanSystem } from '../../api/ops';
import { ErrorNote } from '../../components/ErrorNote';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { formatUptime } from '../../lib/system';
import { formatUtc } from '../../lib/tasks';
import { useNow } from '../../lib/useNow';
import { STATUS_REFRESH_MS, systemKeys, useSystemNodes } from './api';
import { AutoRefresh } from './AutoRefresh';
import { ago, HEALTH, summarize, type HealthStatus, type Subsystem, type SystemNode } from './model';
import './system.css';

export function HealthChip({ status }: { status: HealthStatus }) {
  const h = HEALTH[status];
  return <Pill tone={h.tone}>{h.label}</Pill>;
}

function SubsystemCard({ s, now }: { s: Subsystem; now: Date }) {
  return (
    <li className={`subsystem subsystem--${s.status}`} aria-label={`${s.label}: ${HEALTH[s.status].label}`}>
      <div className="subsystem__head">
        <h4 className="subsystem__title">{s.label}</h4>
        <HealthChip status={s.status} />
      </div>
      <p className="subsystem__detail">{s.detail}</p>
      {s.facts && s.facts.length > 0 && (
        <dl className="subsystem__facts">
          {s.facts.map((f) => (
            <div key={f.label}>
              <dt>{f.label}</dt>
              <dd>{f.value}</dd>
            </div>
          ))}
        </dl>
      )}
      <p className="subsystem__meta">
        Checked{' '}
        <time dateTime={s.checked_at} title={formatUtc(s.checked_at, { seconds: true })}>
          {ago(s.checked_at, now)}
        </time>
        {s.latency_ms !== undefined && ` · ${s.latency_ms} ms`}
      </p>
    </li>
  );
}

function NodePanel({ node, now }: { node: SystemNode; now: Date }) {
  const titleId = `node-${node.id.replace(/[^\w-]/g, '_')}`;
  return (
    <Panel
      title={
        <span className="node-title">
          {node.id}
          {node.current && <Pill tone="neutral">This node</Pill>}
        </span>
      }
      titleId={titleId}
      className="node-panel"
      action={
        <div className="node-panel__badges">
          <Pill tone={node.role === 'leader' ? 'info' : 'neutral'}>{node.role === 'leader' ? 'Scheduler leader' : 'Follower'}</Pill>
          <HealthChip status={node.status} />
        </div>
      }
    >
      {node.stale && (
        <p className="node-panel__stale" role="note">
          No heartbeat for {ago(node.last_heartbeat, now).replace(' ago', '')}: the node is stopped, hung or cannot reach the database. The subsystems below are
          its last report.
        </p>
      )}
      <dl className="facts facts--compact">
        <div>
          <dt>Host name</dt>
          <dd>{node.hostname}</dd>
        </div>
        <div>
          <dt>Node URL</dt>
          <dd>
            <code className="inline-code">{node.url}</code>
          </dd>
        </div>
        <div>
          <dt>Version</dt>
          <dd>
            {node.version}
            {node.commit && <span className="meta"> · {node.commit.slice(0, 12)}</span>}
          </dd>
        </div>
        <div>
          <dt>Uptime</dt>
          <dd title={`Started ${formatUtc(node.started_at)}`}>{formatUptime(node.uptime_seconds)}</dd>
        </div>
        <div>
          <dt>Last heartbeat</dt>
          <dd>
            <time dateTime={node.last_heartbeat} title={formatUtc(node.last_heartbeat, { seconds: true })}>
              {ago(node.last_heartbeat, now)}
            </time>
          </dd>
        </div>
      </dl>
      <ul className="subsystems" aria-label={`Subsystems of ${node.id}`}>
        {node.subsystems.map((s) => (
          <SubsystemCard key={s.name} s={s} now={now} />
        ))}
      </ul>
    </Panel>
  );
}

function Forbidden() {
  return (
    <Panel>
      <p className="meta">
        The system status needs the <code className="inline-code">system:cluster:read</code> privilege. Ask an administrator for it, or see the
        overall state on the <Link to="/settings">System</Link> page.
      </p>
    </Panel>
  );
}

export function SystemStatusPage() {
  const canRead = useCanSystem('cluster', 'read');
  const [paused, setPaused] = useState(false);
  const nodes = useSystemNodes(paused, canRead);
  const qc = useQueryClient();
  const now = useNow(5_000);

  const forbidden = !canRead || (nodes.error instanceof ApiRequestError && nodes.error.status === 403);
  const data = nodes.data;
  const summary = data ? summarize(data.items) : undefined;

  return (
    <div className="screen">
      <PageHeader
        title="System status"
        description="Every Hangar node and the services it depends on, checked on each node every 15 seconds."
        actions={
          !forbidden && (
            <AutoRefresh
              what="status"
              updatedAt={nodes.dataUpdatedAt || undefined}
              intervalMs={STATUS_REFRESH_MS}
              paused={paused}
              fetching={nodes.isFetching}
              onPause={setPaused}
              onRefresh={() => void qc.refetchQueries({ queryKey: systemKeys.nodes })}
            />
          )
        }
      />

      {forbidden ? (
        <Forbidden />
      ) : nodes.isError && !data ? (
        isMissingEndpoint(nodes.error) ? (
          <Panel>
            <p className="meta">This server does not report node status yet. Upgrade Hangar to see it here.</p>
          </Panel>
        ) : (
          <ErrorNote what="the system status" onRetry={() => void nodes.refetch()} />
        )
      ) : !data || !summary ? (
        <Panel aria-busy="true">
          <p className="meta">Checking every node…</p>
        </Panel>
      ) : (
        <>
          <Panel className="system-summary" aria-label="Summary">
            <div className="system-summary__state" role="status">
              <StatusDot tone={HEALTH[summary.status].tone} halo label={<strong>{HEALTH[summary.status].label}</strong>} />
              <span className="meta">{summary.message}</span>
            </div>
            <dl className="facts">
              <div>
                <dt>Nodes up</dt>
                <dd>
                  {summary.up} of {summary.total}
                </dd>
              </div>
              <div>
                <dt>Scheduler leader</dt>
                <dd>{summary.leader?.id ?? 'None'}</dd>
              </div>
              <div>
                <dt>{summary.versions.length > 1 ? 'Versions (mixed)' : 'Version'}</dt>
                <dd>{summary.versions.join(', ') || '—'}</dd>
              </div>
              <div>
                <dt>Answered by</dt>
                <dd>{data.current}</dd>
              </div>
            </dl>
            {nodes.isError && <p className="field__error">The last refresh failed; showing the previous answer.</p>}
            {data.partial && (
              <p className="node-panel__stale" role="note">
                {data.message ?? 'Only this node is shown.'}
              </p>
            )}
          </Panel>
          {data.items.map((n) => (
            <NodePanel key={n.id} node={n} now={now} />
          ))}
        </>
      )}
    </div>
  );
}
